package liquidation

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestService() *Service {
	return NewService(nil, func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) })
}

func mustPlan(t *testing.T, s *Service, assets, fee, dispute Money, snapshot []SnapshotEntry) *Plan {
	t.Helper()
	p, err := s.CreatePlan("P1", "F1", time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), assets, fee, dispute, snapshot)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.Confirm(p.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return p
}

// 首笔分配：冻结份额计算、确定性舍入、总额守恒。
func TestInitialDistributionRoundingAndReconcile(t *testing.T) {
	s := newTestService()
	snap := []SnapshotEntry{
		{InvestorID: "A", Shares: 1},
		{InvestorID: "B", Shares: 1},
		{InvestorID: "C", Shares: 1},
	}
	p := mustPlan(t, s, 10000, 1000, 500, snap) // 可分配 8500，3 人各 2833，尾差 1

	b, err := s.CreateInitialBatch(p.ID)
	if err != nil {
		t.Fatalf("initial batch: %v", err)
	}
	if b.Total != 8500 {
		t.Fatalf("batch total = %d, want 8500", b.Total)
	}
	for _, it := range b.Items {
		if it.Amount != 2833 {
			t.Fatalf("investor %s amount = %d, want 2833", it.InvestorID, it.Amount)
		}
	}
	if b.Dust != 1 {
		t.Fatalf("dust = %d, want 1", b.Dust)
	}
	if err := p.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 幂等：重复创建首笔批次返回同一批次，不重复分配。
	b2, err := s.CreateInitialBatch(p.ID)
	if err != nil || b2 != b {
		t.Fatalf("initial batch not idempotent: %v %v", b2, err)
	}
}

// 登记日后的持仓变化不得进入本次清算。
func TestRecordDateFreezesSnapshot(t *testing.T) {
	s := newTestService()
	p := mustPlan(t, s, 1000, 0, 0, []SnapshotEntry{{InvestorID: "A", Shares: 10}})

	if err := s.CheckTradeAllowed(p.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrTradingHalted) {
		t.Fatalf("trade after confirm: err = %v", err)
	}
	got, _ := s.GetSnapshot(p.ID)
	if len(got) != 1 || got[0].InvestorID != "A" || got[0].Shares != 10 {
		t.Fatalf("snapshot changed: %+v", got)
	}
}

// 中断恢复：扫描中断后重试只补齐未完成明细，不重复付款。
func TestInterruptedExecutionResumes(t *testing.T) {
	s := newTestService()
	snap := []SnapshotEntry{
		{InvestorID: "A", Shares: 1},
		{InvestorID: "B", Shares: 1},
		{InvestorID: "C", Shares: 1},
	}
	p := mustPlan(t, s, 900, 0, 0, snap)
	b, _ := s.CreateInitialBatch(p.ID)

	paid := map[string]int{}
	calls := 0
	gw := GatewayFunc(func(batchID, investorID string, amount Money) error {
		calls++
		if calls == 2 { // 第二笔付款时中断
			return errors.New("network down")
		}
		paid[investorID]++
		return nil
	})
	res, err := s.ExecuteBatch(p.ID, b.ID, gw)
	if err == nil || !res.Interrupted {
		t.Fatalf("expected interruption, got res=%+v err=%v", res, err)
	}
	if paid["A"] != 1 || len(paid) != 1 {
		t.Fatalf("paid = %v, want only A paid once", paid)
	}

	// 重试：只补 B、C，A 不重复付款。
	res, err = s.ExecuteBatch(p.ID, b.ID, GatewayFunc(func(batchID, investorID string, amount Money) error {
		paid[investorID]++
		return nil
	}))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if paid["A"] != 1 || paid["B"] != 1 || paid["C"] != 1 {
		t.Fatalf("paid = %v, want each exactly once", paid)
	}
	pr, _ := s.PaymentProgress(p.ID, b.ID)
	if pr.Status != BatchDone || pr.PaidCount != 3 || pr.PaidAmount != 900 {
		t.Fatalf("progress = %+v", pr)
	}
	if p.Status != PlanCompleted {
		t.Fatalf("plan status = %s, want COMPLETED", p.Status)
	}
}

// 补充分配：准备金释放后按原冻结份额分配；退出投资者进入待处理而非跳过。
func TestSupplementaryDistributionAndOnHold(t *testing.T) {
	accounts := map[string]AccountStatus{"A": AccountActive, "B": AccountExited, "C": AccountActive}
	s := NewService(func(id string) AccountStatus { return accounts[id] }, nil)
	snap := []SnapshotEntry{
		{InvestorID: "A", Shares: 2},
		{InvestorID: "B", Shares: 1},
		{InvestorID: "C", Shares: 1},
	}
	p := mustPlan(t, s, 10000, 400, 0, snap)

	ib, _ := s.CreateInitialBatch(p.ID)
	if ib.Total != 9600 {
		t.Fatalf("initial total = %d, want 9600", ib.Total)
	}
	if _, err := s.ExecuteBatch(p.ID, ib.ID, GatewayFunc(func(string, string, Money) error { return nil })); err != nil {
		t.Fatalf("execute initial: %v", err)
	}

	// 释放费用准备金 400 形成补充分配，仍按冻结份额 2:1:1。
	sb, err := s.ReleaseReserve(p.ID, false, 400)
	if err != nil {
		t.Fatalf("release reserve: %v", err)
	}
	if sb.Source != SourceReserveRelease || sb.Total != 400 {
		t.Fatalf("supplementary batch = %+v", sb)
	}
	want := map[string]Money{"A": 200, "B": 100, "C": 100}
	for _, it := range sb.Items {
		if it.Amount != want[it.InvestorID] {
			t.Fatalf("investor %s amount = %d, want %d", it.InvestorID, it.Amount, want[it.InvestorID])
		}
	}

	res, err := s.ExecuteBatch(p.ID, sb.ID, GatewayFunc(func(string, string, Money) error { return nil }))
	if err != nil {
		t.Fatalf("execute supplementary: %v", err)
	}
	if res.OnHoldCount != 1 {
		t.Fatalf("on-hold = %d, want 1", res.OnHoldCount)
	}
	var bItem *Item
	for _, it := range sb.Items {
		if it.InvestorID == "B" {
			bItem = it
		}
	}
	if bItem.Status != ItemOnHold || bItem.Reason == "" {
		t.Fatalf("exited investor item = %+v, want ON_HOLD with reason", bItem)
	}
	if sb.Status != BatchPartial {
		t.Fatalf("batch status = %s, want PARTIAL", sb.Status)
	}

	// 超额释放被拒绝。
	if _, err := s.ReleaseReserve(p.ID, false, 1); !errors.Is(err, ErrReserveExceed) {
		t.Fatalf("over-release: err = %v", err)
	}
	if err := p.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// 剩余资产 = B 在首笔（2400）与补充（100）批次中的待处理金额。
	rem, _ := s.RemainingAssets(p.ID)
	if rem.UnpaidItems != 2500 || rem.Total != 2500 {
		t.Fatalf("remaining = %+v", rem)
	}
}

// 竞态：取消、付款确认、准备金释放并发时保持唯一结果。
func TestConcurrentCancelConfirmRelease(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := newTestService()
		p := mustPlan(t, s, 1000, 100, 0, []SnapshotEntry{{InvestorID: "A", Shares: 1}})
		b, _ := s.CreateInitialBatch(p.ID)

		var wg sync.WaitGroup
		results := make([]error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); results[0] = s.Cancel(p.ID) }()
		go func() { defer wg.Done(); results[1] = s.ConfirmPayment(p.ID, b.ID, "A") }()
		go func() { defer wg.Done(); _, results[2] = s.ReleaseReserve(p.ID, false, 100) }()
		wg.Wait()

		// 取消与付款确认互斥：两者不可同时成功。
		if results[0] == nil && results[1] == nil {
			t.Fatalf("iter %d: cancel and confirm both succeeded", i)
		}
		if err := p.Reconcile(); err != nil {
			t.Fatalf("iter %d: reconcile: %v", i, err)
		}
	}
}

// 并发执行同一批次：总额不变、每人只付一次。
func TestConcurrentExecuteSameBatch(t *testing.T) {
	s := newTestService()
	snap := []SnapshotEntry{{InvestorID: "A", Shares: 1}, {InvestorID: "B", Shares: 1}}
	p := mustPlan(t, s, 1000, 0, 0, snap)
	b, _ := s.CreateInitialBatch(p.ID)

	var mu sync.Mutex
	paid := map[string]int{}
	gw := GatewayFunc(func(batchID, investorID string, amount Money) error {
		mu.Lock()
		paid[investorID]++
		mu.Unlock()
		return nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.ExecuteBatch(p.ID, b.ID, gw) }()
	}
	wg.Wait()
	if paid["A"] != 1 || paid["B"] != 1 {
		t.Fatalf("paid = %v, want each exactly once", paid)
	}
	if err := p.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// 查询接口：方案、快照、批次、进度、剩余资产。
func TestQueries(t *testing.T) {
	s := newTestService()
	p := mustPlan(t, s, 1000, 100, 50, []SnapshotEntry{{InvestorID: "A", Shares: 3}, {InvestorID: "B", Shares: 1}})

	if got, err := s.GetPlan(p.ID); err != nil || got.FundID != "F1" {
		t.Fatalf("GetPlan: %v %+v", err, got)
	}
	if _, err := s.GetPlan("nope"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("GetPlan missing: %v", err)
	}
	snap, _ := s.GetSnapshot(p.ID)
	if len(snap) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	b, _ := s.CreateInitialBatch(p.ID)
	batches, _ := s.ListBatches(p.ID)
	if len(batches) != 1 || batches[0].ID != b.ID {
		t.Fatalf("batches = %+v", batches)
	}
	pr, _ := s.PaymentProgress(p.ID, b.ID)
	if pr.PendingCount != 2 || pr.Total != 850 {
		t.Fatalf("progress = %+v", pr)
	}
	rem, _ := s.RemainingAssets(p.ID)
	// 850 按 3:1 分配为 637+212=849，尾差 1 留存。
	if rem.FeeReserve != 100 || rem.DisputeReserve != 50 || rem.UnpaidItems != 849 || rem.Dust != 1 {
		t.Fatalf("remaining = %+v", rem)
	}
}

// 大数舍入确定性：乘法不溢出，结果确定。
func TestAllocationDeterministicLargeNumbers(t *testing.T) {
	snap := []SnapshotEntry{
		{InvestorID: "A", Shares: 3_333_333_333},
		{InvestorID: "B", Shares: 6_666_666_667},
	}
	a1, d1 := allocate(9_000_000_000_000, snap, 10_000_000_000)
	a2, d2 := allocate(9_000_000_000_000, snap, 10_000_000_000)
	if a1[0] != a2[0] || a1[1] != a2[1] || d1 != d2 {
		t.Fatalf("non-deterministic: %v %v vs %v %v", a1, d1, a2, d2)
	}
	if a1[0]+a1[1]+d1 != 9_000_000_000_000 {
		t.Fatalf("not conserved: %v dust %d", a1, d1)
	}
}
