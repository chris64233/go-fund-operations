package sidepocket

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func newConfirmedPlan(t *testing.T, s *Service, holdings map[string]int64, effective map[string]time.Time) *Plan {
	t.Helper()
	recordDate := date("2026-06-30")
	for investor, shares := range holdings {
		eff := effective[investor]
		if eff.IsZero() {
			eff = date("2026-01-01")
		}
		if err := s.Subscribe("FUND1", investor, shares, eff); err != nil {
			t.Fatalf("subscribe %s: %v", investor, err)
		}
	}
	if _, err := s.CreatePlan("P1", "FUND1", "ASSET-X", recordDate, 1_000_000, 1, 2, 1); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatalf("confirm plan: %v", err)
	}
	p, err := s.Plan("P1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	return p
}

// 登记日边界：登记日当天生效的份额参与划分，登记日之后生效的不参与。
func TestRecordDateBoundary(t *testing.T) {
	s := NewService()
	p := newConfirmedPlan(t, s,
		map[string]int64{"on-date": 1000, "after-date": 1000, "before-date": 1000},
		map[string]time.Time{
			"on-date":     date("2026-06-30"),
			"after-date":  date("2026-07-01"),
			"before-date": date("2026-06-29"),
		})

	if got := s.EntitlementOf("P1", "after-date"); got != 0 {
		t.Fatalf("investor effective after record date should get 0 entitlement, got %d", got)
	}
	on := s.EntitlementOf("P1", "on-date")
	before := s.EntitlementOf("P1", "before-date")
	if on <= 0 || before <= 0 {
		t.Fatalf("investors effective on/before record date should get entitlements, got on=%d before=%d", on, before)
	}
	if p.TotalEntitlement != p.Valuation {
		t.Fatalf("total entitlement %d != valuation %d", p.TotalEntitlement, p.Valuation)
	}
}

// 总量核对：不能整除时所有投资者权益合计仍必须等于方案总量。
func TestEntitlementTotalMatchesValuation(t *testing.T) {
	s := NewService()
	p := newConfirmedPlan(t, s,
		map[string]int64{"a": 1, "b": 1, "c": 1, "d": 7, "e": 13},
		nil)

	var sum int64
	for _, investor := range []string{"a", "b", "c", "d", "e"} {
		sum += s.EntitlementOf("P1", investor)
	}
	if sum != p.Valuation || p.TotalEntitlement != p.Valuation {
		t.Fatalf("sum=%d total=%d valuation=%d, must all be equal", sum, p.TotalEntitlement, p.Valuation)
	}
}

// 登记日之后申购的新投资者只取得主基金份额，不取得侧袋权益。
func TestNewInvestorAfterRecordDateGetsNoEntitlement(t *testing.T) {
	s := NewService()
	newConfirmedPlan(t, s, map[string]int64{"old": 1000}, nil)

	if err := s.Subscribe("FUND1", "new", 5000, date("2026-08-01")); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := s.EntitlementOf("P1", "new"); got != 0 {
		t.Fatalf("new investor should have no entitlement, got %d", got)
	}
	if got := s.MainShares("FUND1", "new"); got != 5000 {
		t.Fatalf("new investor main shares = %d, want 5000", got)
	}
}

// 原投资者赎回主基金后侧袋权益继续保留，且侧袋权益不计入可赎回数量。
func TestRedeemKeepsEntitlement(t *testing.T) {
	s := NewService()
	p := newConfirmedPlan(t, s, map[string]int64{"old": 1000}, nil)

	ent := s.EntitlementOf("P1", "old")
	if err := s.Redeem("FUND1", "old", 1000); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got := s.MainShares("FUND1", "old"); got != 0 {
		t.Fatalf("main shares after full redeem = %d, want 0", got)
	}
	if got := s.EntitlementOf("P1", "old"); got != ent {
		t.Fatalf("entitlement changed after redeem: got %d, want %d", got, ent)
	}
	if err := s.Redeem("FUND1", "old", 1); err != ErrInsufficient {
		t.Fatalf("redeem against side pocket entitlement should fail with ErrInsufficient, got %v", err)
	}
	if p.TotalEntitlement != p.Valuation {
		t.Fatalf("total entitlement %d != valuation %d", p.TotalEntitlement, p.Valuation)
	}
}

// 转移竞态：并发双向转移下总量守恒、不出现负权益。
func TestConcurrentTransfersConserveTotal(t *testing.T) {
	s := NewService()
	p := newConfirmedPlan(t, s, map[string]int64{"a": 1000, "b": 1000}, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 4000)
	for i := 0; i < 1000; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- s.TransferEntitlement("P1", "a", "b", 10) }()
		go func() { defer wg.Done(); errs <- s.TransferEntitlement("P1", "b", "a", 10) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && err != ErrInsufficient {
			t.Fatalf("unexpected transfer error: %v", err)
		}
	}

	ea := s.EntitlementOf("P1", "a")
	eb := s.EntitlementOf("P1", "b")
	if ea < 0 || eb < 0 {
		t.Fatalf("negative entitlement: a=%d b=%d", ea, eb)
	}
	if ea+eb != p.Valuation {
		t.Fatalf("total not conserved: a=%d b=%d sum=%d valuation=%d", ea, eb, ea+eb, p.Valuation)
	}
}

// 转移后分配按当前有效权益归属最终持有人。
func TestDistributionFollowsCurrentHolder(t *testing.T) {
	s := NewService()
	newConfirmedPlan(t, s, map[string]int64{"a": 1000}, nil)

	ent := s.EntitlementOf("P1", "a")
	if err := s.TransferEntitlement("P1", "a", "b", ent/2); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if err := s.RecoverCash("P1", "R1", 100_000); err != nil {
		t.Fatalf("recover: %v", err)
	}
	details, err := s.ScanDistributions("R1")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	amounts := map[string]int64{}
	var sum int64
	for _, d := range details {
		amounts[d.InvestorID] += d.Amount
		sum += d.Amount
	}
	if sum != 100_000 {
		t.Fatalf("distribution sum = %d, want 100000", sum)
	}
	if amounts["a"] != 50_000 || amounts["b"] != 50_000 {
		t.Fatalf("distribution should follow current holders equally, got %v", amounts)
	}
}

// 重复/并发扫描：同一回收只生成一份明细，不重复分配。
func TestConcurrentDuplicateScanDistributesOnce(t *testing.T) {
	s := NewService()
	newConfirmedPlan(t, s, map[string]int64{"a": 300, "b": 700}, nil)
	if err := s.RecoverCash("P1", "R1", 99_999); err != nil {
		t.Fatalf("recover: %v", err)
	}

	const workers = 32
	results := make([][]DistributionDetail, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := s.ScanDistributions("R1")
			if err != nil {
				t.Errorf("scan: %v", err)
				return
			}
			results[i] = d
		}(i)
	}
	wg.Wait()

	for i := 1; i < workers; i++ {
		if fmt.Sprint(results[i]) != fmt.Sprint(results[0]) {
			t.Fatalf("concurrent scans returned different details:\n%v\n%v", results[0], results[i])
		}
	}
	var sum int64
	seen := map[string]int{}
	for _, d := range results[0] {
		sum += d.Amount
		seen[d.InvestorID]++
	}
	if sum != 99_999 {
		t.Fatalf("distribution sum = %d, want 99999", sum)
	}
	for investor, n := range seen {
		if n != 1 {
			t.Fatalf("investor %s appears %d times in one scan, want exactly 1", investor, n)
		}
	}
	stored, err := s.DistributionDetails("R1")
	if err != nil {
		t.Fatalf("query details: %v", err)
	}
	if fmt.Sprint(stored) != fmt.Sprint(results[0]) {
		t.Fatalf("stored details differ from scanned details")
	}
}

// 权益转移、回收确认与分配扫描并发时，每份权益只归入一名最终持有人。
func TestTransferAndScanConcurrent(t *testing.T) {
	s := NewService()
	p := newConfirmedPlan(t, s, map[string]int64{"a": 1000}, nil)
	if err := s.RecoverCash("P1", "R1", 10_000); err != nil {
		t.Fatalf("recover: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.TransferEntitlement("P1", "a", "b", s.EntitlementOf("P1", "a"))
	}()
	var details []DistributionDetail
	var scanErr error
	go func() {
		defer wg.Done()
		details, scanErr = s.ScanDistributions("R1")
	}()
	wg.Wait()
	if scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}

	var sum int64
	holders := map[string]bool{}
	for _, d := range details {
		sum += d.Amount
		holders[d.InvestorID] = true
	}
	if sum != 10_000 {
		t.Fatalf("distribution sum = %d, want 10000", sum)
	}
	if len(holders) != 1 {
		t.Fatalf("entitlement should belong to exactly one final holder, got %v", holders)
	}
	if ea, eb := s.EntitlementOf("P1", "a"), s.EntitlementOf("P1", "b"); ea+eb != p.Valuation {
		t.Fatalf("entitlement total changed: a=%d b=%d valuation=%d", ea, eb, p.Valuation)
	}
}
