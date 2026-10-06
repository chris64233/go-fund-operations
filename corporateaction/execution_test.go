package corporateaction

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 重复执行:同行动号同参数返回原结果且不重复增减份额;
// 基金、份额类型、日期或比例不一致返回冲突。
func TestRepeatExecutionIdempotentAndConflict(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-03-01", 10000)
	mustTrade(t, s, "T2", "B", "2026-03-01", 20000)
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}

	first, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
	if err != nil {
		t.Fatal(err)
	}
	totalA, frozenA := s.Holdings(testFund, testClass, "A", MustDate("2026-12-31"))

	second, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
	if err != nil {
		t.Fatalf("重复执行应成功: %v", err)
	}
	if *first != *second {
		t.Fatalf("重复执行应返回原结果:\n%+v\n%+v", first, second)
	}
	if got, _ := s.Holdings(testFund, testClass, "A", MustDate("2026-12-31")); got != totalA {
		t.Fatalf("重复扫描改变了份额: %d -> %d", totalA, got)
	}
	if frozenA != 0 {
		t.Fatalf("A 冻结额异常: %d", frozenA)
	}

	cases := []ExecuteRequest{
		{ActionID: "CA1", FundID: "OTHER", ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: "B", RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-02"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-06"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 3, Den: 1}},
	}
	for i, req := range cases {
		if _, err := s.Execute(req); !errors.Is(err, ErrExecutionConflict) {
			t.Fatalf("第 %d 例应返回冲突, got %v", i, err)
		}
	}

	// 未确认行动不能执行。
	mustRegister(t, s, "CA2", "2026-05-01", "2026-05-05", Ratio{Num: 2, Den: 1})
	if _, err := s.Execute(execReq("CA2", "2026-05-01", "2026-05-05", Ratio{Num: 2, Den: 1})); !errors.Is(err, ErrActionNotConfirmed) {
		t.Fatalf("未确认行动执行应拒绝, got %v", err)
	}
}

// 生效日之后的新交易不能被错误地重复转换:
// 执行依据登记日快照,执行当日与之后的申购原样保留,不产生第二次转换。
func TestPostEffectiveTradesNotReconverted(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-03-01", 10000) // 登记 100 份
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}
	// 登记日后、生效日前申购 30 份:不纳入快照。
	mustTrade(t, s, "T2", "A", "2026-03-04", 3000)
	if _, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})); err != nil {
		t.Fatal(err)
	}
	// 生效日当日与之后再申购。
	mustTrade(t, s, "T3", "A", "2026-03-05", 5000)
	mustTrade(t, s, "T4", "A", "2026-03-06", 7000)

	// 转换只作用于快照的 100 份 → 200 份;其余 30+50+70 份保持旧份额。
	got, _ := s.Holdings(testFund, testClass, "A", MustDate("2026-12-31"))
	if want := Shares(20000 + 3000 + 5000 + 7000); got != want {
		t.Fatalf("生效日后新交易被错误转换: got %d want %d", got, want)
	}
	// 重复扫描结果不变。
	res, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalBefore != 10000 || res.TotalAfter != 20000 {
		t.Fatalf("重复执行汇总变化: %+v", res)
	}
	got, _ = s.Holdings(testFund, testClass, "A", MustDate("2026-12-31"))
	if want := Shares(35000); got != want {
		t.Fatalf("重复执行后份额变化: got %d want %d", got, want)
	}

	// 转换明细可追溯:原数量、新数量、版本、持有人齐备。
	c := convMap(t, s, "CA1")["A"]
	if c.HolderID != "A" || c.BeforeShares != 10000 || c.AfterShares != 20000 || c.ActionVersion != 1 {
		t.Fatalf("转换明细追溯字段错误: %+v", c)
	}
	hc, err := s.HolderConversion("CA1", "A")
	if err != nil || hc.AfterShares != 20000 {
		t.Fatalf("持有人转换结果查询错误: %v %+v", err, hc)
	}
	if _, err := s.HolderConversion("CA1", "ZZZ"); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("不存在的持有人应返回未找到, got %v", err)
	}
}

// 中断恢复:执行中途失败后行动停留在 EXECUTING,
// 再次扫描只补齐未完成转换,旧份额不重复注销、新份额不重复发放。
func TestInterruptedExecutionRecovery(t *testing.T) {
	s := NewService()
	holders := []string{"A", "B", "C", "D", "E"}
	for i, h := range holders {
		mustTrade(t, s, fmt.Sprintf("T%d", i), h, "2026-03-01", Shares(10000+i*100))
	}
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("crash")
	s.applyHook = func(_ string, applied int) error {
		if applied >= 2 {
			return boom
		}
		return nil
	}
	if _, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})); !errors.Is(err, boom) {
		t.Fatalf("应中断, got %v", err)
	}
	a, _ := s.GetAction("CA1")
	if a.Status != ActionExecuting {
		t.Fatalf("中断后应停留 EXECUTING, got %s", a.Status)
	}
	done := 0
	for _, c := range convMap(t, s, "CA1") {
		if c.Status == ConversionDone {
			done++
		}
	}
	if done != 2 {
		t.Fatalf("中断时应完成 2 条, got %d", done)
	}

	// 恢复执行:补齐剩余 3 条。
	s.applyHook = nil
	res, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if res.HolderCount != 5 || res.Status != ActionExecuted {
		t.Fatalf("恢复结果错误: %+v", res)
	}
	for i, h := range holders {
		want := Shares(10000+i*100) * 2
		if got, _ := s.Holdings(testFund, testClass, h, MustDate("2026-12-31")); got != want {
			t.Fatalf("持有人 %s 应为 %d(不重复转换), got %d", h, want, got)
		}
	}
	for _, c := range convMap(t, s, "CA1") {
		if c.Status != ConversionDone {
			t.Fatalf("存在未完成明细: %+v", c)
		}
	}
	// 再次执行为幂等返回。
	again, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
	if err != nil || *again != *res {
		t.Fatalf("恢复后重复执行应幂等: %v %+v", err, again)
	}
}

// 并发:多协程同时执行同一行动,伴随执行后的持仓变动与取消尝试,
// 每名持有人只形成一份最终转换结果,汇总与入账完全一致。
func TestConcurrentExecutionSingleResult(t *testing.T) {
	s := NewService()
	const n = 20
	for i := 0; i < n; i++ {
		mustTrade(t, s, fmt.Sprintf("T%02d", i), fmt.Sprintf("H%02d", i), "2026-03-01", Shares(10000+i*100))
	}
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]*ExecutionResult, 8)
	errs := make([]error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			results[g], errs[g] = s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1}))
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 执行启动后取消必须失败;登记日后的新交易可正常入账。
		for i := 0; i < n; i++ {
			_ = s.PostTrade(TradeCmd{
				TradeID: fmt.Sprintf("L%02d", i), FundID: testFund, ShareClass: testClass,
				HolderID: fmt.Sprintf("H%02d", i), TradeDate: MustDate("2026-03-06"), Shares: 100,
			})
		}
		_ = s.CancelAction("CA1", "并发取消")
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
	a, _ := s.GetAction("CA1")
	if a.Status != ActionExecuted {
		t.Fatalf("并发取消不应影响已完成执行, got %s", a.Status)
	}

	convs := convMap(t, s, "CA1")
	if len(convs) != n {
		t.Fatalf("每名持有人应恰有一份转换, got %d", len(convs))
	}
	seen := map[string]bool{}
	var sumBefore, sumAfter Shares
	for _, c := range convs {
		if seen[c.HolderID] {
			t.Fatalf("重复转换: %s", c.HolderID)
		}
		seen[c.HolderID] = true
		if c.Status != ConversionDone || c.BeforeShares*2 != c.AfterShares {
			t.Fatalf("明细异常: %+v", c)
		}
		sumBefore += c.BeforeShares
		sumAfter += c.AfterShares
		// 入账逐户一致:转换后份额 + 生效日后新申购 100(旧份额,不转换)。
		want := c.AfterShares + 100
		if got, _ := s.Holdings(testFund, testClass, c.HolderID, MustDate("2026-12-31")); got != want {
			t.Fatalf("%s 入账 %d != %d", c.HolderID, got, want)
		}
	}
	if results[0].TotalBefore != sumBefore || results[0].TotalAfter != sumAfter || sumAfter != 2*sumBefore {
		t.Fatalf("汇总与明细不一致: res=%+v before=%d after=%d", results[0], sumBefore, sumAfter)
	}
}
