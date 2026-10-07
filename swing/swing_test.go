package swing

import (
	"strings"
	"testing"
)

// newTestService 构造带一条默认规则的服务：
// 门槛 1000000，上调 0.005，下调 0.008，最大调整 0.01，2026-01-01 起生效。
func newTestService(t *testing.T) *Service {
	t.Helper()
	s := NewService()
	_, err := s.PublishRule(SwingRule{
		FundID:        "FUND01",
		EffectiveDate: "2026-01-01",
		Threshold:     MustDecimal("1000000"),
		UpFactor:      MustDecimal("0.005"),
		DownFactor:    MustDecimal("0.008"),
		MaxAdjustment: MustDecimal("0.01"),
	})
	if err != nil {
		t.Fatalf("publish rule: %v", err)
	}
	return s
}

func addTrade(t *testing.T, s *Service, id, fund, date string, typ TradeType, amount string) {
	t.Helper()
	_, err := s.AddTrade(Trade{ID: id, FundID: fund, TradeDate: date, Type: typ, Amount: MustDecimal(amount)})
	if err != nil {
		t.Fatalf("add trade %s: %v", id, err)
	}
}

func TestDecimalExactArithmetic(t *testing.T) {
	// 0.1 + 0.2 在浮点下不等于 0.3，精确十进制必须相等。
	got := MustDecimal("0.1").Add(MustDecimal("0.2"))
	if got.Cmp(MustDecimal("0.3")) != 0 {
		t.Fatalf("0.1+0.2 = %s, want 0.3", got)
	}
	// 净值乘法保持精确：1.2345 * 1.005 = 1.2406725。
	nav := MustDecimal("1.2345").Mul(MustDecimal("1.005"))
	if nav.String() != "1.2406725" {
		t.Fatalf("1.2345*1.005 = %s, want 1.2406725", nav)
	}
	if MustDecimal("-2.50").Abs().String() != "2.50" {
		t.Fatalf("abs failed")
	}
}

func TestRuleImmutabilityAndEffectiveVersion(t *testing.T) {
	s := newTestService(t)
	// 同一生效日期不允许重复发布（版本不可修改）。
	_, err := s.PublishRule(SwingRule{
		FundID: "FUND01", EffectiveDate: "2026-01-01",
		Threshold: MustDecimal("1"), UpFactor: MustDecimal("0.1"),
		DownFactor: MustDecimal("0.1"), MaxAdjustment: MustDecimal("0.1"),
	})
	if err == nil {
		t.Fatal("expected error publishing duplicate effective date")
	}
	// 发布第二版，2026-06-01 起生效。
	_, err = s.PublishRule(SwingRule{
		FundID: "FUND01", EffectiveDate: "2026-06-01",
		Threshold: MustDecimal("500000"), UpFactor: MustDecimal("0.01"),
		DownFactor: MustDecimal("0.01"), MaxAdjustment: MustDecimal("0.02"),
	})
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	// 交易日必须引用当时有效的版本。
	r1, err := s.EffectiveRule("FUND01", "2026-03-01")
	if err != nil || r1.Version != 1 {
		t.Fatalf("2026-03-01 should use v1, got %+v, err=%v", r1, err)
	}
	r2, err := s.EffectiveRule("FUND01", "2026-06-01")
	if err != nil || r2.Version != 2 {
		t.Fatalf("2026-06-01 should use v2, got %+v, err=%v", r2, err)
	}
	if _, err := s.EffectiveRule("FUND01", "2025-12-31"); err == nil {
		t.Fatal("expected no effective rule before first effective date")
	}
	versions := s.ListRuleVersions("FUND01")
	if len(versions) != 2 {
		t.Fatalf("want 2 rule versions, got %d", len(versions))
	}
}

func TestThresholdBoundary(t *testing.T) {
	s := newTestService(t)
	// 净资金流恰好等于门槛：不触发（严格大于才触发）。
	addTrade(t, s, "T1", "FUND01", "2026-03-02", Subscription, "1000000")
	c, err := s.TrialCalculate("FUND01", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Triggered {
		t.Fatalf("net flow equal to threshold must not trigger, got %+v", c)
	}
	if c.AdjustedNAV.String() != "1.0000" {
		t.Fatalf("NAV should stay 1.0000, got %s", c.AdjustedNAV)
	}
	// 多 0.01：触发。
	addTrade(t, s, "T2", "FUND01", "2026-03-02", Subscription, "0.01")
	c2, err := s.TrialCalculate("FUND01", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Triggered || c2.Direction != DirectionUp {
		t.Fatalf("net flow just above threshold must trigger up-swing, got %+v", c2)
	}
}

func TestSwingUpDirection(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	addTrade(t, s, "R1", "FUND01", "2026-03-02", Redemption, "500000")
	c, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.2345"))
	if err != nil {
		t.Fatal(err)
	}
	// 净流入 1500000 > 门槛，上调 0.005：1.2345 * 1.005 = 1.2406725。
	if !c.Triggered || c.Direction != DirectionUp {
		t.Fatalf("want up swing, got %+v", c)
	}
	if c.NetFlow.String() != "1500000" {
		t.Fatalf("net flow = %s, want 1500000", c.NetFlow)
	}
	if c.AdjustedNAV.String() != "1.2406725" {
		t.Fatalf("adjusted NAV = %s, want 1.2406725", c.AdjustedNAV)
	}
}

func TestSwingDownDirection(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "100000")
	addTrade(t, s, "R1", "FUND01", "2026-03-02", Redemption, "1500000")
	c, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("2.5000"))
	if err != nil {
		t.Fatal(err)
	}
	// 净流出 1400000，下调 0.008：2.5 * 0.992 = 2.48。
	if !c.Triggered || c.Direction != DirectionDown {
		t.Fatalf("want down swing, got %+v", c)
	}
	if c.AdjustedNAV.String() != "2.4800000" {
		t.Fatalf("adjusted NAV = %s, want 2.4800000", c.AdjustedNAV)
	}
}

func TestMaxAdjustmentCap(t *testing.T) {
	s := NewService()
	_, err := s.PublishRule(SwingRule{
		FundID: "FUND02", EffectiveDate: "2026-01-01",
		Threshold: MustDecimal("100"), UpFactor: MustDecimal("0.05"),
		DownFactor: MustDecimal("0.05"), MaxAdjustment: MustDecimal("0.02"),
	})
	if err != nil {
		t.Fatal(err)
	}
	addTrade(t, s, "S1", "FUND02", "2026-03-02", Subscription, "1000")
	c, err := s.Confirm("FUND02", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	// 上调比例 0.05 被最大幅度 0.02 封顶：1 * 1.02 = 1.02。
	if c.AdjustFactor.String() != "0.02" {
		t.Fatalf("factor = %s, want capped 0.02", c.AdjustFactor)
	}
	if c.AdjustedNAV.String() != "1.020000" {
		t.Fatalf("adjusted NAV = %s, want 1.020000", c.AdjustedNAV)
	}
}

func TestDuplicateConfirmRejected(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	if _, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000")); err == nil {
		t.Fatal("duplicate confirm must be rejected")
	}
	// 试算不产生确认版本，不影响确认。
	s2 := newTestService(t)
	addTrade(t, s2, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	if _, err := s2.TrialCalculate("FUND01", "2026-03-02", MustDecimal("1.0000")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000")); err != nil {
		t.Fatalf("confirm after trial should succeed: %v", err)
	}
}

func TestLateTradeNotMixedIntoConfirmedSnapshot(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	confirmed, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	// 确认后到达的迟到申请。
	addTrade(t, s, "LATE1", "FUND01", "2026-03-02", Subscription, "500000")
	// 原快照必须保持不变。
	again, err := s.GetCalculation("FUND01", "2026-03-02", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.TradeIDs) != 1 || again.TradeIDs[0] != "S1" {
		t.Fatalf("confirmed snapshot must not change, got %v", again.TradeIDs)
	}
	if again.AdjustedNAV.Cmp(confirmed.AdjustedNAV) != 0 {
		t.Fatal("confirmed NAV must not change")
	}
	// 受影响交易查询：迟到申请被标注，不混入快照。
	affected, err := s.AffectedTrades("FUND01", "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	var late *AffectedTrade
	for i := range affected {
		if affected[i].Trade.ID == "LATE1" {
			late = &affected[i]
		}
	}
	if late == nil || late.InSnapshot || !strings.Contains(late.PendingNote, "迟到") {
		t.Fatalf("late trade should be pending, got %+v", late)
	}
}

func TestCorrectionCreatesNewVersionWithDeltas(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	addTrade(t, s, "R1", "FUND01", "2026-03-02", Redemption, "500000")
	v1, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	// 确认后：一笔迟到申购到达，一笔赎回撤销。
	addTrade(t, s, "LATE1", "FUND01", "2026-03-02", Subscription, "300000")
	if err := s.CancelTrade("R1"); err != nil {
		t.Fatal(err)
	}
	v2, err := s.Correct("FUND01", "2026-03-02", MustDecimal("1.0000"))
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || v2.Supersedes != 1 {
		t.Fatalf("want version 2 superseding 1, got %+v", v2)
	}
	// 新快照：S1 + LATE1，净资金流 2300000。
	if v2.NetFlow.String() != "2300000" {
		t.Fatalf("corrected net flow = %s, want 2300000", v2.NetFlow)
	}
	// 差额明细：LATE1 纳入，R1 剔除。
	if len(v2.Deltas) != 2 {
		t.Fatalf("want 2 deltas, got %+v", v2.Deltas)
	}
	var added, removed *TradeDelta
	for i := range v2.Deltas {
		switch v2.Deltas[i].Change {
		case "ADDED":
			added = &v2.Deltas[i]
		case "REMOVED":
			removed = &v2.Deltas[i]
		}
	}
	if added == nil || added.TradeID != "LATE1" || added.AmountDelta.String() != "300000" {
		t.Fatalf("bad ADDED delta: %+v", added)
	}
	if removed == nil || removed.TradeID != "R1" || removed.AmountDelta.String() != "500000" {
		t.Fatalf("bad REMOVED delta: %+v", removed)
	}
	// 原版本不可覆盖。
	orig, err := s.GetCalculation("FUND01", "2026-03-02", 1)
	if err != nil {
		t.Fatal(err)
	}
	if orig.NetFlow.Cmp(v1.NetFlow) != 0 || len(orig.TradeIDs) != 2 {
		t.Fatalf("original version must be preserved, got %+v", orig)
	}
	// 版本差异查询。
	diff, err := s.VersionDiff("FUND01", "2026-03-02", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(diff, "\n")
	if !strings.Contains(joined, "净资金流") || !strings.Contains(joined, "LATE1") || !strings.Contains(joined, "R1") {
		t.Fatalf("diff should mention net flow and both trades:\n%s", joined)
	}
	// 未确认就更正应报错。
	s3 := newTestService(t)
	if _, err := s3.Correct("FUND01", "2026-03-02", MustDecimal("1.0000")); err == nil {
		t.Fatal("correct without confirm must fail")
	}
}

func TestExplain(t *testing.T) {
	s := newTestService(t)
	addTrade(t, s, "S1", "FUND01", "2026-03-02", Subscription, "2000000")
	if _, err := s.Confirm("FUND01", "2026-03-02", MustDecimal("1.0000")); err != nil {
		t.Fatal(err)
	}
	explain, err := s.Explain("FUND01", "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"触发摆动", "UP", "净资金流", "门槛", "1.0050000"} {
		if !strings.Contains(explain, want) {
			t.Fatalf("explain should contain %q:\n%s", want, explain)
		}
	}
}
