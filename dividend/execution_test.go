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
//   - C(现金) 10.00 份 → 1000×12500/1e6 = 12.5 → 四舍五入 13 分
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

	allocs, err := s.Allocations("P1")
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("C 明细错误: %+v", c)
	}

	// 汇总恒等式:Σ应发 = Σ现金 + Σ新增份额折算 = 方案总额。
	var sumEnt, sumCash Money
	var sumShares Shares
	for _, a := range allocs {
		sumEnt += a.Entitlement
		sumCash += a.CashAmount
		sumShares += a.ReinvestShares
		if a.CashAmount+sharesValue(a.ReinvestShares, 1234500) != a.Entitlement {
			t.Fatalf("明细 %s 现金+折算 ≠ 应发: %+v", a.HolderID, a)
		}
	}
	if res.TotalEntitlement != sumEnt || res.TotalCash != sumCash || res.TotalReinvestShares != sumShares {
		t.Fatalf("执行结果与明细之和不一致: %+v", res)
	}
	if sumEnt != 417+1250+13 {
		t.Fatalf("方案总额错误: %v", sumEnt)
	}

	// 入账核对:现金与份额分别到账。
	if got := s.CashBalance(testFund, "A"); got != 417 {
		t.Fatalf("A 现金余额: %v", got)
	}
	if got := s.CashBalance(testFund, "B"); got != 1 {
		t.Fatalf("B 现金余额(折算剩余): %v", got)
	}
	if got := s.Holdings(testFund, "B", MustDate("2026-12-31")); got != 100000+1012 {
		t.Fatalf("B 份额(含再投资): %v", got)
	}
}

// 重复执行:同一方案号与参数重复执行返回原结果,不重复入账;
// 基金、登记日、分红金额或除息净值变化时返回冲突。
func TestExecuteIdempotentAndConflict(t *testing.T) {
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
	second, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil {
		t.Fatal(err)
	}
	if *first != *second {
		t.Fatalf("重复执行应返回原结果: %+v vs %+v", first, second)
	}
	if got := s.CashBalance(testFund, "A"); got != 100 {
		t.Fatalf("重复执行导致重复发放现金: %v", got)
	}
	if got := s.Holdings(testFund, "B", MustDate("2026-12-31")); got != 20000+200 {
		t.Fatalf("重复执行导致重复增加份额: %v", got)
	}

	conflicts := []ExecuteRequest{
		{PlanID: "P1", FundID: "OTHER", RecordDate: MustDate("2026-01-10"), DividendPerShare: 10000, ExNav: 1000000},
		execReq("P1", "2026-01-11", 10000, 1000000), // 登记日变化
		execReq("P1", "2026-01-10", 10001, 1000000), // 分红金额变化
		execReq("P1", "2026-01-10", 10000, 999999),  // 除息净值变化
	}
	for i, req := range conflicts {
		if _, err := s.Execute(req); !errors.Is(err, ErrExecutionConflict) {
			t.Fatalf("冲突场景 %d 应返回 ErrExecutionConflict, got %v", i, err)
		}
	}
}

// 中断恢复:执行中断后方案停留在 EXECUTING,再次扫描只补齐未完成明细,
// 不重复发放现金或重复增加份额。
func TestExecuteResumeAfterInterruption(t *testing.T) {
	s := NewService()
	for i := 0; i < 5; i++ {
		mustTrade(t, s, fmt.Sprintf("T%d", i), fmt.Sprintf("H%d", i), "2026-01-05", 10000)
	}
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.SetChoice("P1", "H2", PayoutReinvest); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	// 第 3 条明细入账前中断。
	boom := errors.New("crash")
	s.applyHook = func(_ string, applied int) error {
		if applied == 2 {
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
		t.Fatalf("中断时应完成 2 条明细, got %d", done)
	}

	// 恢复执行:只补齐剩余 3 条。
	s.applyHook = nil
	res, err := s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	if err != nil {
		t.Fatal(err)
	}
	if res.HolderCount != 5 || res.TotalEntitlement != 500 {
		t.Fatalf("恢复执行结果错误: %+v", res)
	}
	for i := 0; i < 5; i++ {
		h := fmt.Sprintf("H%d", i)
		wantCash := Money(100)
		wantShares := Shares(10000)
		if h == "H2" {
			wantCash = 0
			wantShares = 10000 + 100 // 100 分 ÷ 1.000000 元 = 1.00 份
		}
		if got := s.CashBalance(testFund, h); got != wantCash {
			t.Fatalf("%s 现金被重复发放: %v, want %v", h, got, wantCash)
		}
		if got := s.Holdings(testFund, h, MustDate("2026-12-31")); got != wantShares {
			t.Fatalf("%s 份额被重复增加: %v, want %v", h, got, wantShares)
		}
	}
	allocs, _ = s.Allocations("P1")
	for _, a := range allocs {
		if a.Status != AllocationDone {
			t.Fatalf("恢复后仍有未完成明细: %+v", a)
		}
	}
}

// 并发执行扫描:多 goroutine 同时执行同一方案,每名持有人只生成一份
// 最终分配,现金与份额不重复入账,结果一致。
func TestConcurrentExecuteScans(t *testing.T) {
	s := NewService()
	const holders = 20
	for i := 0; i < holders; i++ {
		mustTrade(t, s, fmt.Sprintf("T%d", i), fmt.Sprintf("H%02d", i), "2026-01-05", 10000)
	}
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.SetChoice("P1", "H03", PayoutReinvest); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	results := make([]*ExecutionResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			results[w], errs[w] = s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
		}(w)
	}
	wg.Wait()

	for w := 0; w < workers; w++ {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		if *results[w] != *results[0] {
			t.Fatalf("并发执行结果不一致: %+v vs %+v", results[w], results[0])
		}
	}
	if results[0].HolderCount != holders {
		t.Fatalf("持有人数错误: %+v", results[0])
	}
	allocs, _ := s.Allocations("P1")
	if len(allocs) != holders {
		t.Fatalf("每名持有人应只有一份分配, got %d", len(allocs))
	}
	for i := 0; i < holders; i++ {
		h := fmt.Sprintf("H%02d", i)
		wantCash := Money(100)
		wantShares := Shares(10000)
		if h == "H03" {
			wantCash = 0
			wantShares = 10100
		}
		if got := s.CashBalance(testFund, h); got != wantCash {
			t.Fatalf("%s 现金错误: %v, want %v", h, got, wantCash)
		}
		if got := s.Holdings(testFund, h, MustDate("2026-12-31")); got != wantShares {
			t.Fatalf("%s 份额错误: %v, want %v", h, got, wantShares)
		}
	}
}

// 并发混合:执行扫描与登记日后的持仓变更并发,快照与分配结果不受影响。
func TestConcurrentTradesDuringExecution(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T0", "A", "2026-01-05", 10000)
	mustPlan(t, s, "P1", "2026-01-10", "2026-01-12", 10000, 1000000)
	if err := s.ConfirmPlan("P1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = s.PostTrade(TradeCmd{
				TradeID: fmt.Sprintf("TL%d", i), FundID: testFund, HolderID: "A",
				TradeDate: MustDate("2026-01-20"), Shares: 100,
			})
		}
	}()
	var res *ExecutionResult
	var err error
	go func() {
		defer wg.Done()
		res, err = s.Execute(execReq("P1", "2026-01-10", 10000, 1000000))
	}()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalEntitlement != 100 {
		t.Fatalf("并发交易影响了分配结果: %+v", res)
	}
	if snap := snapshotOf(t, s, "P1"); snap["A"] != 10000 {
		t.Fatalf("并发交易改变了快照: %v", snap)
	}
	if got := s.CashBalance(testFund, "A"); got != 100 {
		t.Fatalf("现金入账错误: %v", got)
	}
}
