package dividend

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 现金与再投资精度:固定舍入规则,明细之和等于方案总额。
//
// 场景:每份分红 0.0125 元,除息净值 1.2345 元。
//   - A(现金,默认) 333.33 份 → 33333×12500/1e6 = 416.6625 → 四舍五入 417 分
//   - B(再投资) 1000.00 份 → 应发 1250 分;
//     份额 = 1250×1e6/1234500 = 1012.55… → 向下取整 1012(10.12 份);
//     折算金额 = 1012×1234500/1e6 = 1249.314 → 1249 分,差额 1 分以现金补齐
//   - C(现金) 10.00 份 → 125.000…×0.1 → 12.5 分 → 四舍五入 13 分
func TestCashAndReinvestPrecision(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 33333)
	mustTrade(t, s, "T2", "B", "2026-01-05", 100000)
	mustTrade(t, s, "T3", "C", "2026-01-05", 1000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 12500, 1234500)
	if err := s.SetChoice("P1", "B", PayoutReinvest); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Execute(execReq("P1", "2026-01-10", 12500, 1234500))
	if err != nil {
		t.Fatal(err)
	}

	allocs, _ := s.Allocations("P1")
	by := map[string]Allocation{}
	for _, a := range allocs {
		by[a.HolderID] = a
	}
	if a := by["A"]; a.Method != PayoutCash || a.Entitlement != 417 || a.CashAmount != 417 || a.ReinvestShares != 0 {
		t.Fatalf("A 明细错误: %+v", a)
	}
	if b := by["B"]; b.Method != PayoutReinvest || b.Entitlement != 1250 ||
		b.ReinvestShares != 1012 || b.CashAmount != 1 {
		t.Fatalf("B 明细错误: %+v", b)
	}
	if c := by["C"]; c.Entitlement != 13 || c.CashAmount != 13 {
		t.Fatalf("C 明细错误(四舍五入): %+v", c)
	}

	// 总额一致性:Σ应发 = Σ现金 + Σ再投资折算 = 方案总额。
	if res.TotalEntitlement != 417+1250+13 {
		t.Fatalf("方案总额错误: %v", res.TotalEntitlement)
	}
	if res.TotalCash != 417+1+13 || res.TotalReinvestShares != 1012 {
		t.Fatalf("汇总错误: %+v", res)
	}
	if res.TotalEntitlement != res.TotalCash+sharesValue(res.TotalReinvestShares, 1234500) {
		t.Fatalf("明细之和与方案总额不一致: %+v", res)
	}

	// 入账核对。
	if got := s.CashBalance(testFund, "A"); got != 417 {
		t.Fatalf("A 现金: %v", got)
	}
	if got := s.CashBalance(testFund, "B"); got != 1 {
		t.Fatalf("B 剩余现金: %v", got)
	}
	if got := s.Holdings(testFund, "B", MustDate("2026-12-31")); got != 100000+1012 {
		t.Fatalf("B 再投资份额: %v", got)
	}
}

// 重复执行:同方案号同参数返回原结果且不重复入账;关键参数变化返回冲突。
func TestRepeatExecutionIdempotentAndConflict(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-01-05", 10000)
	mustTrade(t, s, "T2", "B", "2026-01-05", 20000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.SetChoice("P1", "B", PayoutReinvest); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	first, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil {
		t.Fatal(err)
	}
	cashA := s.CashBalance(testFund, "A")
	sharesB := s.Holdings(testFund, "B", MustDate("2026-12-31"))

	second, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil {
		t.Fatalf("重复执行应成功: %v", err)
	}
	if *first != *second {
		t.Fatalf("重复执行应返回原结果:\n%+v\n%+v", first, second)
	}
	if got := s.CashBalance(testFund, "A"); got != cashA {
		t.Fatalf("重复发放现金: %v -> %v", cashA, got)
	}
	if got := s.Holdings(testFund, "B", MustDate("2026-12-31")); got != sharesB {
		t.Fatalf("重复增加份额: %v -> %v", sharesB, got)
	}

	// 关键参数变化 → 冲突。
	cases := []ExecuteRequest{
		{PlanID: "P1", FundID: "OTHER", RecordDate: MustDate("2026-01-10"), DividendPerShare: 10000, ExNav: 1000000},
		{PlanID: "P1", FundID: testFund, RecordDate: MustDate("2026-01-11"), DividendPerShare: 10000, ExNav: 1000000},
		{PlanID: "P1", FundID: testFund, RecordDate: MustDate("2026-01-10"), DividendPerShare: 20000, ExNav: 1000000},
		{PlanID: "P1", FundID: testFund, RecordDate: MustDate("2026-01-10"), DividendPerShare: 10000, ExNav: 1100000},
	}
	for i, req := range cases {
		if _, err := s.Execute(req); !errors.Is(err, ErrExecutionConflict) {
			t.Fatalf("第 %d 例应返回冲突, got %v", i, err)
		}
	}
}

// 中断恢复:执行中途失败后,再次扫描只补齐未完成明细。
func TestInterruptedExecutionRecovery(t *testing.T) {
	s := NewService()
	holders := []string{"A", "B", "C", "D", "E"}
	for i, h := range holders {
		mustTrade(t, s, fmt.Sprintf("T%d", i), h, "2026-01-05", 10000)
	}
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	// 应用 2 条明细后模拟崩溃。
	boom := errors.New("crash")
	s.applyHook = func(_ string, applied int) error {
		if applied >= 2 {
			return boom
		}
		return nil
	}
	if _, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000)); !errors.Is(err, boom) {
		t.Fatalf("应中断, got %v", err)
	}
	plan, _ := s.GetPlan("P1")
	if plan.Status != PlanExecuting {
		t.Fatalf("中断后应停留 EXECUTING, got %s", plan.Status)
	}
	allocs, _ := s.Allocations("P1")
	done := 0
	for _, a := range allocs {
		if a.Status == AllocationDone {
			done++
		}
	}
	if done != 2 {
		t.Fatalf("中断时应完成 2 条, got %d", done)
	}

	// 恢复执行:只补齐剩余 3 条。
	s.applyHook = nil
	res, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil {
		t.Fatal(err)
	}
	if res.HolderCount != 5 || res.TotalCash != 500 {
		t.Fatalf("恢复结果错误: %+v", res)
	}
	for _, h := range holders {
		if got := s.CashBalance(testFund, h); got != 100 {
			t.Fatalf("持有人 %s 现金应为 100(不重复发放), got %v", h, got)
		}
	}
	allocs, _ = s.Allocations("P1")
	for _, a := range allocs {
		if a.Status != AllocationDone {
			t.Fatalf("存在未完成明细: %+v", a)
		}
	}
	// 再次执行为幂等返回。
	again, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil || *again != *res {
		t.Fatalf("恢复后重复执行应幂等: %v %+v", err, again)
	}
}

// 并发:多协程同时执行同一方案并伴随登记日后交易,
// 每名持有人只生成一份最终分配,入账与明细完全一致。
func TestConcurrentExecutionSingleAllocation(t *testing.T) {
	s := NewService()
	const n = 20
	for i := 0; i < n; i++ {
		mustTrade(t, s, fmt.Sprintf("T%d", i), fmt.Sprintf("H%02d", i), "2026-01-05", Shares(10000+i*100))
	}
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 12500, 1234500)
	for i := 0; i < n; i += 2 {
		if err := s.SetChoice("P1", fmt.Sprintf("H%02d", i), PayoutReinvest); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]*ExecutionResult, 8)
	errs := make([]error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			results[g], errs[g] = s.Execute(execReq("P1", "2026-01-10", 12500, 1234500))
		}(g)
	}
	// 并发的登记日后持仓变动。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_ = s.PostTrade(TradeCmd{
				TradeID: fmt.Sprintf("L%d", i), FundID: testFund,
				HolderID:  fmt.Sprintf("H%02d", i),
				TradeDate: MustDate("2026-01-20"), Shares: 100,
			})
		}
	}()
	wg.Wait()

	for g := range errs {
		if errs[g] != nil {
			t.Fatalf("并发执行出错: %v", errs[g])
		}
		if *results[g] != *results[0] {
			t.Fatalf("并发执行结果不一致:\n%+v\n%+v", results[g], results[0])
		}
	}

	allocs, _ := s.Allocations("P1")
	if len(allocs) != n {
		t.Fatalf("每名持有人应恰有一份分配, got %d", len(allocs))
	}
	seen := map[string]bool{}
	var sumEnt, sumCash Money
	var sumShares Shares
	for _, a := range allocs {
		if seen[a.HolderID] {
			t.Fatalf("重复分配: %s", a.HolderID)
		}
		seen[a.HolderID] = true
		if a.Status != AllocationDone {
			t.Fatalf("明细未完成: %+v", a)
		}
		sumEnt += a.Entitlement
		sumCash += a.CashAmount
		sumShares += a.ReinvestShares
		// 入账与明细逐户一致。
		if got := s.CashBalance(testFund, a.HolderID); got != a.CashAmount {
			t.Fatalf("%s 现金入账 %v != 明细 %v", a.HolderID, got, a.CashAmount)
		}
	}
	res := results[0]
	if res.TotalEntitlement != sumEnt || res.TotalCash != sumCash || res.TotalReinvestShares != sumShares {
		t.Fatalf("汇总与明细不一致: %+v", res)
	}
	if sumEnt != sumCash+sharesValue(sumShares, 1234500) {
		t.Fatalf("总额不守恒: ent=%v cash=%v shares=%v", sumEnt, sumCash, sumShares)
	}
}
