package liquidation

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeLedger struct {
	holdings []SnapshotEntry
}

func (l *fakeLedger) HoldingsAt(fundID string, d time.Time) []SnapshotEntry {
	return l.holdings
}

type recordingPayer struct {
	mu   sync.Mutex
	keys map[string]int
	fail map[string]bool
}

func newPayer() *recordingPayer {
	return &recordingPayer{keys: map[string]int{}, fail: map[string]bool{}}
}

func (p *recordingPayer) pay(it PaymentItem) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[it.IdempotencyKey]++
	if p.fail[it.IdempotencyKey] {
		return errors.New("payment channel error")
	}
	return nil
}

func (p *recordingPayer) calls(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keys[key]
}

var recordDate = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func newConfirmedService(t *testing.T, holdings []SnapshotEntry, distributable, fee, dispute Money) (*Service, *Plan, *recordingPayer) {
	t.Helper()
	payer := newPayer()
	svc := NewService(&fakeLedger{holdings: holdings}, payer.pay)
	plan, err := svc.CreatePlan("FUND-1", recordDate, distributable, fee, dispute)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if err := svc.ConfirmPlan(plan.ID); err != nil {
		t.Fatalf("ConfirmPlan: %v", err)
	}
	return svc, plan, payer
}

func TestConfirmFreezesSnapshotAndHaltsTrading(t *testing.T) {
	ledger := &fakeLedger{holdings: []SnapshotEntry{
		{InvestorID: "B", Shares: 300},
		{InvestorID: "A", Shares: 700},
	}}
	svc := NewService(ledger, nil)
	plan, _ := svc.CreatePlan("FUND-1", recordDate, 10000, 500, 300)
	if err := svc.ConfirmPlan(plan.ID); err != nil {
		t.Fatal(err)
	}
	// 登记日后持仓变化不得进入快照
	ledger.holdings = append(ledger.holdings, SnapshotEntry{InvestorID: "C", Shares: 999})
	snap, _ := svc.GetSnapshot(plan.ID)
	if len(snap) != 2 {
		t.Fatalf("snapshot should be frozen, got %d entries", len(snap))
	}
	if snap[0].InvestorID != "A" || snap[1].InvestorID != "B" {
		t.Fatalf("snapshot should be sorted by investor: %+v", snap)
	}
	if err := svc.RecordShareTrade("FUND-1"); !errors.Is(err, ErrTradingHalted) {
		t.Fatalf("expected trading halted, got %v", err)
	}
	// 重复确认被拒绝
	if err := svc.ConfirmPlan(plan.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected invalid state, got %v", err)
	}
}

func TestFirstDistributionRoundingInvariant(t *testing.T) {
	// 10001 分给 3 名等份额投资者，产生尾差 2
	holdings := []SnapshotEntry{
		{InvestorID: "A", Shares: 1},
		{InvestorID: "B", Shares: 1},
		{InvestorID: "C", Shares: 1},
	}
	svc, plan, _ := newConfirmedService(t, holdings, 10001, 500, 300)
	b, err := svc.CreateFirstDistribution(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sum Money
	for _, it := range b.Items {
		sum += it.Amount
		if it.Amount != 3333 {
			t.Fatalf("expected deterministic floor amount 3333, got %d", it.Amount)
		}
	}
	if b.Rounding != 2 {
		t.Fatalf("expected rounding remainder 2, got %d", b.Rounding)
	}
	// 全部分配 + 准备金 + 尾差 == 清算资产
	got, _ := svc.GetPlan(plan.ID)
	total := got.DistributedTotal + (got.FeeReserve + got.DisputeReserve) + got.RoundingRemainder
	if total != got.TotalAssets() {
		t.Fatalf("invariant broken: %d != %d", total, got.TotalAssets())
	}
	// 确定性：相同输入重复计算结果一致
	a1, r1 := allocate(10001, holdings, 3)
	a2, r2 := allocate(10001, holdings, 3)
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatal("allocate not deterministic")
		}
	}
	if r1 != r2 {
		t.Fatal("remainder not deterministic")
	}
}

func TestExecuteInterruptedRetryCompletesWithoutDoublePay(t *testing.T) {
	holdings := []SnapshotEntry{
		{InvestorID: "A", Shares: 100},
		{InvestorID: "B", Shares: 200},
		{InvestorID: "C", Shares: 300},
	}
	svc, plan, payer := newConfirmedService(t, holdings, 60000, 0, 0)
	b, _ := svc.CreateFirstDistribution(plan.ID)

	// 模拟中断：本次只扫描 1 条
	if err := svc.ExecuteBatch(b.ID, 1); err != nil {
		t.Fatal(err)
	}
	pr, _ := svc.GetBatchProgress(b.ID)
	if pr.Paid != 1 || pr.Pending != 2 {
		t.Fatalf("after interrupt: paid=%d pending=%d", pr.Paid, pr.Pending)
	}
	// 重试只补齐未完成明细
	if err := svc.ExecuteBatch(b.ID, 0); err != nil {
		t.Fatal(err)
	}
	pr, _ = svc.GetBatchProgress(b.ID)
	if pr.Paid != 3 || pr.Status != BatchDone {
		t.Fatalf("after retry: %+v", pr)
	}
	for _, it := range b.Items {
		if got := payer.calls(it.IdempotencyKey); got != 1 {
			t.Fatalf("item %s paid %d times, want 1", it.IdempotencyKey, got)
		}
	}
	// 已完成批次再次执行为空操作
	if err := svc.ExecuteBatch(b.ID, 0); err != nil {
		t.Fatal(err)
	}
	for key, n := range payer.keys {
		if n != 1 {
			t.Fatalf("key %s called %d times", key, n)
		}
	}
}

func TestFailedPaymentRetriedWithSameIdempotencyKey(t *testing.T) {
	holdings := []SnapshotEntry{{InvestorID: "A", Shares: 1}}
	svc, plan, payer := newConfirmedService(t, holdings, 1000, 0, 0)
	b, _ := svc.CreateFirstDistribution(plan.ID)
	key := b.Items[0].IdempotencyKey
	payer.fail[key] = true
	svc.ExecuteBatch(b.ID, 0)
	pr, _ := svc.GetBatchProgress(b.ID)
	if pr.Failed != 1 {
		t.Fatalf("expected 1 failed, got %+v", pr)
	}
	payer.fail[key] = false
	svc.ExecuteBatch(b.ID, 0)
	pr, _ = svc.GetBatchProgress(b.ID)
	if pr.Paid != 1 {
		t.Fatalf("expected paid after retry, got %+v", pr)
	}
	if payer.calls(key) != 2 {
		t.Fatalf("expected 2 attempts with same key, got %d", payer.calls(key))
	}
}

func TestSupplementalDistributionAndOnHoldInvestor(t *testing.T) {
	holdings := []SnapshotEntry{
		{InvestorID: "A", Shares: 500},
		{InvestorID: "B", Shares: 500},
	}
	svc, plan, _ := newConfirmedService(t, holdings, 10000, 2000, 1000)
	first, _ := svc.CreateFirstDistribution(plan.ID)
	svc.ExecuteBatch(first.ID, 0)

	// B 在首笔后退出
	svc.SetAccountStatus("B", AccountExited)
	supp, err := svc.ReleaseReserve(plan.ID, 2000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if supp.Kind != BatchSupplemental || supp.Pool != 3000 {
		t.Fatalf("bad supplemental batch: %+v", supp)
	}
	// 补充分配仍按原冻结份额
	for _, it := range supp.Items {
		if it.Amount != 1500 {
			t.Fatalf("expected 1500 by frozen shares, got %d", it.Amount)
		}
	}
	svc.ExecuteBatch(supp.ID, 0)
	pr, _ := svc.GetBatchProgress(supp.ID)
	if pr.Paid != 1 || pr.OnHold != 1 {
		t.Fatalf("exited investor must be ON_HOLD not skipped: %+v", pr)
	}
	// 人工处理后账户恢复，重试完成付款
	svc.SetAccountStatus("B", AccountActive)
	if err := svc.ResolveOnHold(supp.ID, "B"); err != nil {
		t.Fatal(err)
	}
	svc.ExecuteBatch(supp.ID, 0)
	pr, _ = svc.GetBatchProgress(supp.ID)
	if pr.Paid != 2 || pr.Status != BatchDone {
		t.Fatalf("after resolve: %+v", pr)
	}
	// 全部释放且批次完成后方案完结
	got, _ := svc.GetPlan(plan.ID)
	if got.Status != PlanCompleted {
		t.Fatalf("expected COMPLETED, got %s", got.Status)
	}
	rem, _ := svc.GetRemainingAssets(plan.ID)
	if rem != 0 {
		t.Fatalf("expected 0 remaining, got %d", rem)
	}
}

func TestReleaseReserveExceedingBalanceRejected(t *testing.T) {
	holdings := []SnapshotEntry{{InvestorID: "A", Shares: 1}}
	svc, plan, _ := newConfirmedService(t, holdings, 1000, 100, 50)
	if _, err := svc.ReleaseReserve(plan.ID, 101, 0); !errors.Is(err, ErrNothingToRelease) {
		t.Fatalf("expected ErrNothingToRelease, got %v", err)
	}
	if _, err := svc.ReleaseReserve(plan.ID, 100, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseReserve(plan.ID, 1, 0); !errors.Is(err, ErrNothingToRelease) {
		t.Fatalf("expected nothing left to release, got %v", err)
	}
}

func TestCancelConcurrencyUniqueResult(t *testing.T) {
	holdings := []SnapshotEntry{{InvestorID: "A", Shares: 1}}
	for i := 0; i < 50; i++ {
		svc, plan, _ := newConfirmedService(t, holdings, 1000, 100, 0)
		var wg sync.WaitGroup
		results := make([]error, 2)
		ops := []func() error{
			func() error { return svc.CancelPlan(plan.ID) },
			func() error { _, err := svc.CreateFirstDistribution(plan.ID); return err },
		}
		for j, op := range ops {
			wg.Add(1)
			go func(j int, op func() error) {
				defer wg.Done()
				results[j] = op()
			}(j, op)
		}
		wg.Wait()
		succeeded := 0
		for _, err := range results {
			if err == nil {
				succeeded++
			}
		}
		if succeeded != 1 {
			t.Fatalf("iter %d: exactly one op must win, got %d (errs=%v)", i, succeeded, results)
		}
	}
}

func TestConcurrentReserveReleaseNeverOverReleases(t *testing.T) {
	holdings := []SnapshotEntry{{InvestorID: "A", Shares: 1}}
	for i := 0; i < 50; i++ {
		svc, plan, _ := newConfirmedService(t, holdings, 1000, 100, 0)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				_, errs[j] = svc.ReleaseReserve(plan.ID, 100, 0)
			}(j)
		}
		wg.Wait()
		succeeded := 0
		for _, err := range errs {
			if err == nil {
				succeeded++
			}
		}
		if succeeded != 1 {
			t.Fatalf("iter %d: reserve released %d times, want 1", i, succeeded)
		}
		got, _ := svc.GetPlan(plan.ID)
		if got.ReleasedFee != 100 {
			t.Fatalf("iter %d: released fee = %d, want 100", i, got.ReleasedFee)
		}
	}
}

func TestCancelAfterBatchRejected(t *testing.T) {
	holdings := []SnapshotEntry{{InvestorID: "A", Shares: 1}}
	svc, plan, _ := newConfirmedService(t, holdings, 1000, 0, 0)
	if _, err := svc.CreateFirstDistribution(plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelPlan(plan.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cancel after batch must fail, got %v", err)
	}
}

func TestQueries(t *testing.T) {
	holdings := []SnapshotEntry{
		{InvestorID: "A", Shares: 250},
		{InvestorID: "B", Shares: 750},
	}
	svc, plan, _ := newConfirmedService(t, holdings, 10000, 500, 500)
	b, _ := svc.CreateFirstDistribution(plan.ID)
	svc.ExecuteBatch(b.ID, 0)

	p, err := svc.GetPlan(plan.ID)
	if err != nil || p.TotalShares != 1000 || len(p.Snapshot) != 2 {
		t.Fatalf("GetPlan: %+v err=%v", p, err)
	}
	batches, _ := svc.ListBatches(plan.ID)
	if len(batches) != 1 || batches[0].Kind != BatchFirst {
		t.Fatalf("ListBatches: %+v", batches)
	}
	pr, _ := svc.GetBatchProgress(b.ID)
	if pr.PaidAmount != 10000 || pr.Paid != 2 {
		t.Fatalf("progress: %+v", pr)
	}
	rem, _ := svc.GetRemainingAssets(plan.ID)
	if rem != 1000 { // 两项准备金未释放
		t.Fatalf("remaining: %d", rem)
	}
	if _, err := svc.GetPlan("NOPE"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}
