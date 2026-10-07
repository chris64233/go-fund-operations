package fundoperations

import (
	"errors"
	"strings"
	"testing"
)

func newSetup(t *testing.T) (*RuleStore, *TradeStore, *Engine) {
	t.Helper()
	rules := NewRuleStore()
	trades := NewTradeStore()
	eng := NewEngine(rules, trades)
	// 门槛 100 万，上调 0.5%，下调 0.8%，最大调整 1%。
	rule, err := rules.CreateDraft("F001", "2026-01-01",
		MustDecimal("1000000"), MustDecimal("0.005"), MustDecimal("0.008"), MustDecimal("0.01"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Publish(rule.FundID, rule.Version); err != nil {
		t.Fatal(err)
	}
	return rules, trades, eng
}

func sub(id, date, amount string) Trade {
	return Trade{ID: id, FundID: "F001", TradeDate: date, Type: Subscription, Quantity: MustDecimal(amount)}
}

func red(id, date, shares string) Trade {
	return Trade{ID: id, FundID: "F001", TradeDate: date, Type: Redemption, Quantity: MustDecimal(shares)}
}

func TestThresholdBoundary(t *testing.T) {
	_, _, eng := newSetup(t)
	date := "2026-03-02"
	// 净申购恰好等于门槛：不触发。
	if _, err := eng.SubmitTrade(sub("T1", date, "1000000")); err != nil {
		t.Fatal(err)
	}
	calc, err := eng.Trial("F001", date, MustDecimal("1.5"))
	if err != nil {
		t.Fatal(err)
	}
	if calc.Triggered {
		t.Fatalf("net flow equal to threshold should not trigger, got %+v", calc)
	}
	if calc.SwungNAV.Cmp(MustDecimal("1.5")) != 0 {
		t.Fatalf("NAV should stay 1.5, got %s", calc.SwungNAV)
	}
	// 多 0.01 元：触发上调。
	if _, err := eng.SubmitTrade(sub("T2", date, "0.01")); err != nil {
		t.Fatal(err)
	}
	calc, err = eng.Trial("F001", date, MustDecimal("1.5"))
	if err != nil {
		t.Fatal(err)
	}
	if !calc.Triggered || calc.Direction != DirectionUp {
		t.Fatalf("net flow above threshold should trigger up swing, got %+v", calc)
	}
	// 1.5 * 1.005 = 1.5075
	if calc.SwungNAV.String() != "1.5075" {
		t.Fatalf("swung NAV = %s, want 1.5075", calc.SwungNAV)
	}
}

func TestSwingUpAndDown(t *testing.T) {
	_, _, eng := newSetup(t)
	// 净流入：上调。
	if _, err := eng.SubmitTrade(sub("S1", "2026-03-02", "2000000")); err != nil {
		t.Fatal(err)
	}
	up, err := eng.Trial("F001", "2026-03-02", MustDecimal("2"))
	if err != nil {
		t.Fatal(err)
	}
	if up.Direction != DirectionUp || up.SwungNAV.String() != "2.01" {
		t.Fatalf("up swing = %+v", up)
	}
	// 净流出：下调。赎回 600000 份 * 2 = 1200000，申购 0。
	if _, err := eng.SubmitTrade(red("R1", "2026-03-03", "600000")); err != nil {
		t.Fatal(err)
	}
	down, err := eng.Trial("F001", "2026-03-03", MustDecimal("2"))
	if err != nil {
		t.Fatal(err)
	}
	if down.Direction != DirectionDown {
		t.Fatalf("down swing = %+v", down)
	}
	// 2 * (1 - 0.008) = 1.984
	if down.SwungNAV.String() != "1.984" {
		t.Fatalf("swung NAV = %s, want 1.984", down.SwungNAV)
	}
}

func TestMaxAdjustmentCap(t *testing.T) {
	rules := NewRuleStore()
	trades := NewTradeStore()
	eng := NewEngine(rules, trades)
	// 上调 5%，但最大调整 1%。
	rule, _ := rules.CreateDraft("F002", "2026-01-01",
		MustDecimal("100"), MustDecimal("0.05"), MustDecimal("0.05"), MustDecimal("0.01"))
	if _, err := rules.Publish(rule.FundID, rule.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SubmitTrade(Trade{ID: "X1", FundID: "F002", TradeDate: "2026-03-02",
		Type: Subscription, Quantity: MustDecimal("1000")}); err != nil {
		t.Fatal(err)
	}
	calc, err := eng.Trial("F002", "2026-03-02", MustDecimal("1"))
	if err != nil {
		t.Fatal(err)
	}
	if calc.AppliedRate.String() != "0.01" || calc.SwungNAV.String() != "1.01" {
		t.Fatalf("cap not applied: %+v", calc)
	}
}

func TestRuleImmutabilityAndEffectiveDate(t *testing.T) {
	rules, _, _ := newSetup(t)
	if _, err := rules.UpdateDraft("F001", 1, "2026-02-01",
		MustDecimal("1"), MustDecimal("1"), MustDecimal("1"), MustDecimal("1")); !errors.Is(err, ErrRuleImmutable) {
		t.Fatalf("published rule must be immutable, got %v", err)
	}
	// 新版本自 2026-06-01 生效，门槛降为 10。
	r2, err := rules.CreateDraft("F001", "2026-06-01",
		MustDecimal("10"), MustDecimal("0.001"), MustDecimal("0.001"), MustDecimal("0.01"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Publish(r2.FundID, r2.Version); err != nil {
		t.Fatal(err)
	}
	old, err := rules.EffectiveRule("F001", "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if old.Version != 1 {
		t.Fatalf("2026-03-02 should use v1, got v%d", old.Version)
	}
	cur, err := rules.EffectiveRule("F001", "2026-06-01")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Version != 2 {
		t.Fatalf("2026-06-01 should use v2, got v%d", cur.Version)
	}
	if _, err := rules.EffectiveRule("F001", "2025-12-31"); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("before any effective date should fail, got %v", err)
	}
}

func TestDuplicateConfirmRejected(t *testing.T) {
	_, _, eng := newSetup(t)
	date := "2026-03-02"
	if _, err := eng.SubmitTrade(sub("T1", date, "100")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Confirm("F001", date, MustDecimal("1.5")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Confirm("F001", date, MustDecimal("1.5")); err == nil {
		t.Fatal("duplicate confirm must fail")
	}
}

func TestLateTradeNotInConfirmedSnapshot(t *testing.T) {
	_, trades, eng := newSetup(t)
	date := "2026-03-02"
	if _, err := eng.SubmitTrade(sub("T1", date, "2000000")); err != nil {
		t.Fatal(err)
	}
	confirmed, err := eng.Confirm("F001", date, MustDecimal("1"))
	if err != nil {
		t.Fatal(err)
	}
	// 确认后到达的申请自动标记为迟到，且不得混入已确认快照。
	late, err := eng.SubmitTrade(sub("T2", date, "5000000"))
	if err != nil {
		t.Fatal(err)
	}
	if !late.Late {
		t.Fatal("trade submitted after confirm should be marked late")
	}
	if len(confirmed.TradeIDs) != 1 || confirmed.TradeIDs[0] != "T1" {
		t.Fatalf("confirmed snapshot must not include late trade: %v", confirmed.TradeIDs)
	}
	// 撤销的交易也不进入更正快照。
	if err := trades.Cancel("T2"); err != nil {
		t.Fatal(err)
	}
	next, _, err := eng.Correct("F001", date, MustDecimal("1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range next.TradeIDs {
		if id == "T2" {
			t.Fatal("cancelled trade must not enter corrected snapshot")
		}
	}
}

func TestCorrectionCreatesNewVersionWithDiffs(t *testing.T) {
	_, _, eng := newSetup(t)
	date := "2026-03-02"
	// 净申购 200 万 > 门槛 100 万，触发上调 0.5%。
	if _, err := eng.SubmitTrade(sub("T1", date, "2000000")); err != nil {
		t.Fatal(err)
	}
	v1, err := eng.Confirm("F001", date, MustDecimal("1"))
	if err != nil {
		t.Fatal(err)
	}
	if v1.SwungNAV.String() != "1.005" {
		t.Fatalf("v1 swung NAV = %s", v1.SwungNAV)
	}
	// 迟到申购 300 万到达，需要更正。
	if _, err := eng.SubmitTrade(sub("T2", date, "3000000")); err != nil {
		t.Fatal(err)
	}
	v2, diffs, err := eng.Correct("F001", date, MustDecimal("1"))
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || v2.Status != CalcConfirmed {
		t.Fatalf("v2 = %+v", v2)
	}
	if v1.Status != CalcSuperseded {
		t.Fatal("original version must be kept as superseded, not overwritten")
	}
	if len(v2.TradeIDs) != 2 {
		t.Fatalf("corrected snapshot should include late trade: %v", v2.TradeIDs)
	}
	// T1 份额差：2000000/1.005 - 2000000/1.005 = 0（净值未变），
	// T2 为新增交易，OldResult 为 0。
	if len(diffs) != 2 {
		t.Fatalf("want 2 diffs, got %d", len(diffs))
	}
	var t2 *TradeDiff
	for i := range diffs {
		if diffs[i].TradeID == "T2" {
			t2 = &diffs[i]
		}
	}
	if t2 == nil || !t2.OldResult.IsZero() {
		t.Fatalf("late trade diff = %+v", t2)
	}
	// 差额明细与版本差异查询。
	affected, err := eng.AffectedTrades("F001", date, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 2 {
		t.Fatalf("affected trades = %d", len(affected))
	}
	diff, err := eng.DiffVersions("F001", date, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if diff.NewNetFlow.String() != "5000000" || diff.OldNetFlow.String() != "2000000" {
		t.Fatalf("net flow diff = %+v", diff)
	}
	// 原始版本仍可查询且未被覆盖。
	orig, err := eng.GetCalculation("F001", date, 1)
	if err != nil {
		t.Fatal(err)
	}
	if orig.NetFlow.String() != "2000000" {
		t.Fatalf("original result overwritten: %+v", orig)
	}
}

func TestExplain(t *testing.T) {
	_, _, eng := newSetup(t)
	date := "2026-03-02"
	if _, err := eng.SubmitTrade(sub("T1", date, "2000000")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Confirm("F001", date, MustDecimal("1")); err != nil {
		t.Fatal(err)
	}
	text, err := eng.Explain("F001", date)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"触发摆动", "上调", "0.005", "1.005", "1000000"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain should contain %q:\n%s", want, text)
		}
	}
	// 未触发场景。
	if _, err := eng.SubmitTrade(sub("T3", "2026-03-03", "100")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Confirm("F001", "2026-03-03", MustDecimal("1")); err != nil {
		t.Fatal(err)
	}
	text, err = eng.Explain("F001", "2026-03-03")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "未触发摆动") {
		t.Fatalf("explain = %s", text)
	}
}

func TestDecimalExactness(t *testing.T) {
	// 0.1 + 0.2 必须精确等于 0.3。
	sum := MustDecimal("0.1").Add(MustDecimal("0.2"))
	if sum.Cmp(MustDecimal("0.3")) != 0 {
		t.Fatalf("0.1+0.2 = %s", sum)
	}
	// 1.5 * 1.005 = 1.5075，精确表示。
	if got := MustDecimal("1.5").Mul(MustDecimal("1.005")); got.String() != "1.5075" {
		t.Fatalf("got %s", got)
	}
	// 除法保持有理数精度：1/3*3 == 1。
	third := MustDecimal("1").Quo(MustDecimal("3"))
	if third.Mul(MustDecimal("3")).Cmp(MustDecimal("1")) != 0 {
		t.Fatal("rational arithmetic must be exact")
	}
}
