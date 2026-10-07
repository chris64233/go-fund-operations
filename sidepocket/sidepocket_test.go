package sidepocket

import (
	"sync"
	"testing"
	"time"
)

var recordDate = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func mustSubscribe(t *testing.T, s *Service, id string, shares int64) {
	t.Helper()
	if err := s.Subscribe(id, shares); err != nil {
		t.Fatalf("Subscribe(%s): %v", id, err)
	}
}

func mustConfirmPlan(t *testing.T, s *Service, planID string) {
	t.Helper()
	if err := s.ConfirmPlan(planID, recordDate); err != nil {
		t.Fatalf("ConfirmPlan: %v", err)
	}
}

// 登记日边界：登记日前持仓冻结权益；登记日后申购只取得主基金份额，
// 不产生侧袋权益；赎回主基金后侧袋权益保留。
func TestRecordDateBoundary(t *testing.T) {
	s := NewService()
	// 登记日（含）之前申购。
	mustSubscribe(t, s, "alice", 1000)
	mustSubscribe(t, s, "bob", 3000)
	plan, err := s.CreatePlan("FUND1", "ASSET-X", recordDate, 1_000_000, 5000)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	mustConfirmPlan(t, s, plan.ID)

	// 登记日之后：新投资者申购只取得主基金份额。
	mustSubscribe(t, s, "carol", 5000)
	if got := s.EntitlementOf(plan.ID, "carol"); got != 0 {
		t.Fatalf("登记日后申购不应产生侧袋权益, got %d", got)
	}
	if got := s.MainShares("carol"); got != 5000 {
		t.Fatalf("carol 主基金份额 = %d, want 5000", got)
	}

	// 原投资者赎回全部主基金份额，侧袋权益保留。
	before := s.EntitlementOf(plan.ID, "alice")
	if err := s.Redeem("alice", 1000); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got := s.MainShares("alice"); got != 0 {
		t.Fatalf("alice 主基金份额 = %d, want 0", got)
	}
	if got := s.EntitlementOf(plan.ID, "alice"); got != before {
		t.Fatalf("赎回后侧袋权益应保留: got %d, want %d", got, before)
	}

	// 侧袋权益不得混入可赎回数量：alice 主份额为 0，再赎回应失败。
	if err := s.Redeem("alice", 1); err != ErrInsufficientShare {
		t.Fatalf("赎回超过主基金份额应失败, got %v", err)
	}
}

// 总量核对：确认后所有投资者权益合计必须等于方案总量，
// 且转移后合计保持不变。
func TestTotalReconciliation(t *testing.T) {
	s := NewService()
	// 故意使用不能整除的份额，验证最大余数法取整。
	mustSubscribe(t, s, "a", 1)
	mustSubscribe(t, s, "b", 2)
	mustSubscribe(t, s, "c", 7)
	plan, err := s.CreatePlan("FUND1", "ASSET-Y", recordDate, 999_983, 7777)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	mustConfirmPlan(t, s, plan.ID)

	p, _ := s.Plan(plan.ID)
	wantTotal := int64(999_983) * 7777 / 10000
	if p.TotalUnits != wantTotal {
		t.Fatalf("TotalUnits = %d, want %d", p.TotalUnits, wantTotal)
	}
	if got := s.TotalEntitlements(plan.ID); got != p.TotalUnits {
		t.Fatalf("权益合计 = %d, want %d", got, p.TotalUnits)
	}

	// 转移不改变总量。
	if err := s.TransferEntitlement(plan.ID, "c", "d", 100); err != nil {
		t.Fatalf("TransferEntitlement: %v", err)
	}
	if got := s.TotalEntitlements(plan.ID); got != p.TotalUnits {
		t.Fatalf("转移后权益合计 = %d, want %d", got, p.TotalUnits)
	}
	if got := s.EntitlementOf(plan.ID, "d"); got != 100 {
		t.Fatalf("d 权益 = %d, want 100", got)
	}
}

// 转移竞态：并发转移与回收确认、分配扫描同时进行，
// 每份权益只能归入一名最终持有人，总量守恒且不超转。
func TestTransferRace(t *testing.T) {
	s := NewService()
	mustSubscribe(t, s, "alice", 1000)
	plan, err := s.CreatePlan("FUND1", "ASSET-Z", recordDate, 1_000_000, 10000)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	mustConfirmPlan(t, s, plan.ID)
	total := s.TotalEntitlements(plan.ID)

	rec, err := s.ConfirmRecovery(plan.ID, 500_000, recordDate)
	if err != nil {
		t.Fatalf("ConfirmRecovery: %v", err)
	}

	var wg sync.WaitGroup
	// 并发：alice 向 bob/carol 转移（总量超过持仓，部分应失败）。
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.TransferEntitlement(plan.ID, "alice", "bob", 100) }()
		go func() { defer wg.Done(); _ = s.TransferEntitlement(plan.ID, "alice", "carol", 100) }()
	}
	// 并发：重复确认回收与扫描分配。
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.ConfirmRecovery(plan.ID, 1000, recordDate) }()
		go func() { defer wg.Done(); _, _ = s.ScanAndDistribute(plan.ID, recordDate) }()
	}
	wg.Wait()

	// 权益守恒：无人为负，合计不变。
	for _, id := range []string{"alice", "bob", "carol"} {
		if got := s.EntitlementOf(plan.ID, id); got < 0 {
			t.Fatalf("%s 权益为负: %d", id, got)
		}
	}
	if got := s.TotalEntitlements(plan.ID); got != total {
		t.Fatalf("并发后权益合计 = %d, want %d", got, total)
	}

	// 首笔回收只分配一次，明细中每个投资者至多出现一次。
	dists, err := s.DistributionsOf(rec.ID)
	if err != nil {
		t.Fatalf("DistributionsOf: %v", err)
	}
	seen := map[string]bool{}
	var sum int64
	for _, d := range dists {
		if seen[d.InvestorID] {
			t.Fatalf("投资者 %s 被重复分配", d.InvestorID)
		}
		seen[d.InvestorID] = true
		sum += d.Amount
	}
	if sum != rec.Amount {
		t.Fatalf("分配明细合计 = %d, want %d", sum, rec.Amount)
	}
}

// 重复扫描：同一笔回收无论扫描多少次只分配一次，明细不变。
func TestDuplicateScan(t *testing.T) {
	s := NewService()
	mustSubscribe(t, s, "alice", 1000)
	mustSubscribe(t, s, "bob", 1000)
	plan, err := s.CreatePlan("FUND1", "ASSET-W", recordDate, 2_000_000, 10000)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	mustConfirmPlan(t, s, plan.ID)
	rec, err := s.ConfirmRecovery(plan.ID, 100_001, recordDate)
	if err != nil {
		t.Fatalf("ConfirmRecovery: %v", err)
	}

	first, err := s.ScanAndDistribute(plan.ID, recordDate)
	if err != nil {
		t.Fatalf("ScanAndDistribute: %v", err)
	}
	if len(first) != 1 || first[0] != rec.ID {
		t.Fatalf("首次扫描应分配 %s, got %v", rec.ID, first)
	}
	snap1, _ := s.DistributionsOf(rec.ID)

	// 重复扫描（含并发）不应产生新分配。
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ScanAndDistribute(plan.ID, recordDate)
			if err != nil {
				t.Errorf("ScanAndDistribute: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("重复扫描不应再分配, got %v", got)
			}
		}()
	}
	wg.Wait()

	snap2, _ := s.DistributionsOf(rec.ID)
	if len(snap1) != len(snap2) {
		t.Fatalf("明细数量变化: %d -> %d", len(snap1), len(snap2))
	}
	var sum int64
	for i := range snap1 {
		if *snap1[i] != *snap2[i] {
			t.Fatalf("明细被重复分配修改: %+v vs %+v", snap1[i], snap2[i])
		}
		sum += snap2[i].Amount
	}
	if sum != rec.Amount {
		t.Fatalf("明细合计 = %d, want %d", sum, rec.Amount)
	}
}

// 转移后分配：回收按分配时刻的当前有效权益归属最终持有人。
func TestDistributionFollowsCurrentHolder(t *testing.T) {
	s := NewService()
	mustSubscribe(t, s, "alice", 1000)
	plan, err := s.CreatePlan("FUND1", "ASSET-V", recordDate, 1_000_000, 10000)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	mustConfirmPlan(t, s, plan.ID)
	// alice 将全部权益转给 bob。
	if err := s.TransferEntitlement(plan.ID, "alice", "bob", s.EntitlementOf(plan.ID, "alice")); err != nil {
		t.Fatalf("TransferEntitlement: %v", err)
	}
	rec, err := s.ConfirmRecovery(plan.ID, 10_000, recordDate)
	if err != nil {
		t.Fatalf("ConfirmRecovery: %v", err)
	}
	dists, err := s.DistributeRecovery(rec.ID, recordDate)
	if err != nil {
		t.Fatalf("DistributeRecovery: %v", err)
	}
	if len(dists) != 1 || dists[0].InvestorID != "bob" || dists[0].Amount != 10_000 {
		t.Fatalf("全部分配应归 bob: %+v", dists)
	}
}
