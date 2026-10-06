package dividend

import (
	"errors"
	"sync"
	"testing"
)

const testFund = "FUND001"

func mustTrade(t *testing.T, s *Service, id, holder, date string, shares Shares) {
	t.Helper()
	err := s.PostTrade(TradeCmd{
		TradeID: id, FundID: testFund, HolderID: holder,
		TradeDate: MustDate(date), Shares: shares,
	})
	if err != nil {
		t.Fatalf("PostTrade %s: %v", id, err)
	}
}

func mustPlan(t *testing.T, s *Service, planID, record, ex string, dps, nav UnitAmount) {
	t.Helper()
	_, err := s.CreatePlan(CreatePlanCmd{
		PlanID: planID, FundID: testFund,
		RecordDate: MustDate(record), ExDate: MustDate(ex),
		DividendPerShare: dps, ExNav: nav,
	})
	if err != nil {
		t.Fatalf("CreatePlan %s: %v", planID, err)
	}
}

func execReq(planID, record string, dps, nav UnitAmount) ExecuteRequest {
	return ExecuteRequest{
		PlanID: planID, FundID: testFund,
		RecordDate: MustDate(record), DividendPerShare: dps, ExNav: nav,
	}
}

func snapshotOf(t *testing.T, s *Service, planID string) map[string]Shares {
	t.Helper()
	rows, err := s.Snapshot(planID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	out := map[string]Shares{}
	for _, r := range rows {
		out[r.HolderID] = r.Shares
	}
	return out
}

// 登记日边界:登记日当日及之前的持仓计入快照,之后的交易不影响资格;
// 冻结后拒绝补登登记日及之前的交易。
func TestRecordDateBoundary(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 100000) // 1000.00 份,登记日前
	mustTrade(t, s, "T2", "B", "2026-01-10", 20000)  // 200.00 份,登记日当日
	mustTrade(t, s, "T3", "A", "2026-01-11", 50000)  // 登记日后,不计入
	mustTrade(t, s, "T4", "C", "2026-01-12", 90000)  // 登记日后,不计入

	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatalf("ConfirmPlan: %v", err)
	}

	snap := snapshotOf(t, s, "P1")
	if len(snap) != 2 || snap["A"] != 100000 || snap["B"] != 20000 {
		t.Fatalf("快照不符合登记日边界: %v", snap)
	}

	// 冻结后:补登登记日(含)之前的交易被拒绝。
	err := s.PostTrade(TradeCmd{TradeID: "T5", FundID: testFund, HolderID: "A",
		TradeDate: MustDate("2026-01-10"), Shares: 100})
	if !errors.Is(err, ErrBackdatedTrade) {
		t.Fatalf("应拒绝补登, got %v", err)
	}
	// 登记日之后的交易正常受理,且不改变快照。
	mustTrade(t, s, "T6", "A", "2026-01-13", 7000)
	snap = snapshotOf(t, s, "P1")
	if snap["A"] != 100000 {
		t.Fatalf("登记日后交易改变了快照: %v", snap)
	}
	if got := s.Holdings(testFund, "A", MustDate("2026-01-13")); got != 157000 {
		t.Fatalf("持仓汇总错误: %v", got)
	}
}

// 领取方式:执行前可反复修改,执行开始后锁定。
func TestChoiceLockedAfterExecutionStarts(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)

	if err := s.SetChoice("P1", "A", PayoutReinvest); err != nil {
		t.Fatalf("DRAFT 阶段应可修改: %v", err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatalf("ConfirmPlan: %v", err)
	}
	if err := s.SetChoice("P1", "A", PayoutCash); err != nil {
		t.Fatalf("CONFIRMED 阶段应可修改: %v", err)
	}

	// 注入中断,使方案停留在 EXECUTING。
	boom := errors.New("boom")
	s.applyHook = func(string, int) error { return boom }
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); !errors.Is(err, boom) {
		t.Fatalf("应中断, got %v", err)
	}
	if err := s.SetChoice("P1", "A", PayoutReinvest); !errors.Is(err, ErrChoiceLocked) {
		t.Fatalf("EXECUTING 阶段应锁定, got %v", err)
	}

	s.applyHook = nil
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); err != nil {
		t.Fatalf("恢复执行: %v", err)
	}
	if err := s.SetChoice("P1", "A", PayoutReinvest); !errors.Is(err, ErrChoiceLocked) {
		t.Fatalf("EXECUTED 阶段应锁定, got %v", err)
	}
}

// 取消竞态:取消与执行并发,恰有一个成功;取消后快照与选择仍可查询。
func TestCancelRaceWithExecution(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewService()
		mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
		mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
		if err := s.SetChoice("P1", "A", PayoutReinvest); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmPlan("P1"); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var cancelErr, execErr error
		wg.Add(2)
		go func() { defer wg.Done(); cancelErr = s.CancelPlan("P1", "竞态测试") }()
		go func() { defer wg.Done(); _, execErr = s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)) }()
		wg.Wait()

		if (cancelErr == nil) == (execErr == nil) {
			t.Fatalf("第 %d 轮: 取消与执行应恰有一个成功, cancel=%v exec=%v", i, cancelErr, execErr)
		}
		plan, _ := s.GetPlan("P1")
		if cancelErr == nil && plan.Status != PlanCancelled {
			t.Fatalf("第 %d 轮: 状态应为 CANCELLED, got %s", i, plan.Status)
		}
		if execErr == nil && plan.Status != PlanExecuted {
			t.Fatalf("第 %d 轮: 状态应为 EXECUTED, got %s", i, plan.Status)
		}
		// 无论谁赢,快照与选择记录均可查询。
		if snap := snapshotOf(t, s, "P1"); snap["A"] != 10000 {
			t.Fatalf("第 %d 轮: 快照丢失: %v", i, snap)
		}
		choices, err := s.Choices("P1")
		if err != nil || len(choices) != 1 || choices[0].Method != PayoutReinvest {
			t.Fatalf("第 %d 轮: 选择记录丢失: %v %v", i, choices, err)
		}
	}
}

// 取消语义:仅执行前可取消;已取消方案不能执行;已执行方案不能取消。
func TestCancelSemantics(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPlan("P1", "计划变更"); err != nil {
		t.Fatalf("执行前应可取消: %v", err)
	}
	if err := s.CancelPlan("P1", "重复取消"); err != nil {
		t.Fatalf("重复取消应幂等: %v", err)
	}
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); !errors.Is(err, ErrPlanCancelled) {
		t.Fatalf("已取消方案不能执行, got %v", err)
	}
	if snap := snapshotOf(t, s, "P1"); snap["A"] != 10000 {
		t.Fatalf("取消后快照应可查询: %v", snap)
	}

	mustPlan(t, s, "P2", "2026-02-10", "2026-02-12", 10000, 1000000)
	if err := s.ConfirmPlan("P2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(execReq("P2", "2026-02-10", 10000, 1000000)); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPlan("P2", "太迟了"); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("已执行方案不能取消, got %v", err)
	}
}

// 更正记录:已执行方案只能通过独立更正调整,分配明细不被修改。
func TestCorrection(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	// 未执行方案不能登记更正。
	_, err := s.PostCorrection(Correction{CorrectionID: "C0", PlanID: "P1", HolderID: "A", CashDelta: 1})
	if !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("未执行方案应拒绝更正, got %v", err)
	}

	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Allocations("P1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.PostCorrection(Correction{
		CorrectionID: "C1", PlanID: "P1", HolderID: "A",
		CashDelta: -50, ShareDelta: 5000, Reason: "金额差错调整",
	}); err != nil {
		t.Fatalf("PostCorrection: %v", err)
	}
	_, err = s.PostCorrection(Correction{CorrectionID: "C1", PlanID: "P1", HolderID: "A", CashDelta: 1})
	if !errors.Is(err, ErrDuplicateCorrection) {
		t.Fatalf("更正号重复应报错, got %v", err)
	}

	after, _ := s.Allocations("P1")
	if len(before) != len(after) || before[0] != after[0] {
		t.Fatalf("更正不得修改分配明细: %+v -> %+v", before, after)
	}
	if got := s.CashBalance(testFund, "A"); got != 100-50 {
		t.Fatalf("更正后现金余额错误: %v", got)
	}
	if got := s.Holdings(testFund, "A", MustDate("2026-12-31")); got != 15000 {
		t.Fatalf("更正后份额错误: %v", got)
	}
	corrections, _ := s.Corrections("P1")
	if len(corrections) != 1 || corrections[0].Reason != "金额差错调整" {
		t.Fatalf("更正记录查询失败: %+v", corrections)
	}
}

// 生命周期校验:重复方案号、未确认执行、状态历史持久化。
func TestLifecycleGuards(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)

	err := s.PostTrade(TradeCmd{TradeID: "T1", FundID: testFund, HolderID: "A",
		TradeDate: MustDate("2026-01-06"), Shares: 1})
	if !errors.Is(err, ErrDuplicateTrade) {
		t.Fatalf("交易号重复应报错, got %v", err)
	}
	mustPlan(t, s, "PX", "2026-01-10", "2026-01-12", 10000, 1000000)
	_, err = s.CreatePlan(CreatePlanCmd{PlanID: "PX", FundID: testFund,
		RecordDate: MustDate("2026-01-10"), ExDate: MustDate("2026-01-12"),
		DividendPerShare: 10000, ExNav: 1000000})
	if !errors.Is(err, ErrPlanExists) {
		t.Fatalf("方案号重复应报错, got %v", err)
	}
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); !errors.Is(err, ErrPlanNotConfirmed) {
		t.Fatalf("未确认不能执行, got %v", err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan("P1"); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("重复确认应报错, got %v", err)
	}
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); err != nil {
		t.Fatal(err)
	}

	history, err := s.StatusHistory("P1")
	if err != nil {
		t.Fatal(err)
	}
	var got []PlanStatus
	for _, h := range history {
		got = append(got, h.To)
	}
	want := []PlanStatus{PlanDraft, PlanConfirmed, PlanExecuting, PlanExecuted}
	if len(got) != len(want) {
		t.Fatalf("状态历史缺失: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("状态历史错误: %v, want %v", got, want)
		}
	}
}
