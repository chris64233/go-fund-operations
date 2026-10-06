package corporateaction

import (
	"errors"
	"testing"
)

const (
	testFund  = "FUND001"
	testClass = "A"
)

func mustTrade(t *testing.T, s *Service, id, holder, date string, shares Shares) {
	t.Helper()
	err := s.PostTrade(TradeCmd{
		TradeID: id, FundID: testFund, ShareClass: testClass, HolderID: holder,
		TradeDate: MustDate(date), Shares: shares,
	})
	if err != nil {
		t.Fatalf("PostTrade %s: %v", id, err)
	}
}

func mustFreeze(t *testing.T, s *Service, id, holder, date string, delta Shares) {
	t.Helper()
	err := s.PostFreeze(FreezeCmd{
		RequestID: id, FundID: testFund, ShareClass: testClass, HolderID: holder,
		Date: MustDate(date), Delta: delta,
	})
	if err != nil {
		t.Fatalf("PostFreeze %s: %v", id, err)
	}
}

func mustRegister(t *testing.T, s *Service, id, record, effective string, r Ratio) *Action {
	t.Helper()
	a, err := s.Register(RegisterCmd{
		ActionID: id, FundID: testFund, ShareClass: testClass,
		RecordDate: MustDate(record), EffectiveDate: MustDate(effective), Ratio: r,
	})
	if err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
	return a
}

func execReq(id, record, effective string, r Ratio) ExecuteRequest {
	return ExecuteRequest{
		ActionID: id, FundID: testFund, ShareClass: testClass,
		RecordDate: MustDate(record), EffectiveDate: MustDate(effective), Ratio: r,
	}
}

func snapshotMap(t *testing.T, s *Service, id string) map[string]SnapshotRow {
	t.Helper()
	rows, err := s.Snapshot(id)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	out := map[string]SnapshotRow{}
	for _, r := range rows {
		out[r.HolderID] = r
	}
	return out
}

func convMap(t *testing.T, s *Service, id string) map[string]Conversion {
	t.Helper()
	convs, err := s.Conversions(id)
	if err != nil {
		t.Fatalf("Conversions: %v", err)
	}
	out := map[string]Conversion{}
	for _, c := range convs {
		out[c.HolderID] = c
	}
	return out
}

// 比例边界:非正比例拒绝;生效日早于登记日拒绝;1:1 合法;
// 比例按 GCD 归一化,重复登记 4/2 与已登记 2/1 返回同一行动。
func TestRatioBoundaries(t *testing.T) {
	s := NewService()

	for _, r := range []Ratio{{Num: 0, Den: 1}, {Num: 2, Den: 0}, {Num: -1, Den: 2}} {
		_, err := s.Register(RegisterCmd{
			ActionID: "X" + r.String(), FundID: testFund, ShareClass: testClass,
			RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: r,
		})
		if !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("比例 %v 应拒绝, got %v", r, err)
		}
	}
	_, err := s.Register(RegisterCmd{
		ActionID: "BADDATE", FundID: testFund, ShareClass: testClass,
		RecordDate: MustDate("2026-03-05"), EffectiveDate: MustDate("2026-03-01"),
		Ratio: Ratio{Num: 2, Den: 1},
	})
	if !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("生效日早于登记日应拒绝, got %v", err)
	}

	a := mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if a.Ratio != (Ratio{Num: 2, Den: 1}) || a.Version != 1 || a.Status != ActionDraft {
		t.Fatalf("登记内容错误: %+v", a)
	}
	again := mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 4, Den: 2})
	if again.ActionID != a.ActionID || again.Version != a.Version {
		t.Fatalf("等比重复登记应返回原行动:\n%+v\n%+v", again, a)
	}
	same := mustRegister(t, s, "CASAME", "2026-04-01", "2026-04-05", Ratio{Num: 1, Den: 1})
	if same.Ratio.Direction() != DirectionSame {
		t.Fatalf("1:1 方向应为 SAME, got %s", same.Ratio.Direction())
	}
}

// 重复登记:内容一致幂等;比例、日期、基金或份额类型不同返回冲突。
func TestRegisterIdempotentAndConflict(t *testing.T) {
	s := NewService()
	first := mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	again := mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if *again != *first {
		t.Fatalf("相同内容重复登记应返回原结果:\n%+v\n%+v", again, first)
	}

	cases := []RegisterCmd{
		{ActionID: "CA1", FundID: "OTHER", ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: "B", RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-02"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-06"), Ratio: Ratio{Num: 2, Den: 1}},
		{ActionID: "CA1", FundID: testFund, ShareClass: testClass, RecordDate: MustDate("2026-03-01"), EffectiveDate: MustDate("2026-03-05"), Ratio: Ratio{Num: 3, Den: 1}},
	}
	for i, cmd := range cases {
		if _, err := s.Register(cmd); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("第 %d 例应返回内容冲突, got %v", i, err)
		}
	}
}

// 重叠生效期间:同一基金同一份额类型的行动窗口不得重叠;
// 相邻不重叠窗口、不同份额类型与已取消行动均允许。
func TestOverlappingWindowRejected(t *testing.T) {
	s := NewService()
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-10", Ratio{Num: 2, Den: 1})

	overlap := []struct {
		record, effective string
	}{
		{"2026-03-05", "2026-03-20"}, // 登记日落入窗口
		{"2026-02-20", "2026-03-01"}, // 生效日等于对方登记日
		{"2026-02-01", "2026-03-10"}, // 生效日等于对方生效日
		{"2026-03-01", "2026-03-08"}, // 登记日相同
	}
	for i, w := range overlap {
		_, err := s.Register(RegisterCmd{
			ActionID: "OVA" + string(rune('A'+i)), FundID: testFund, ShareClass: testClass,
			RecordDate: MustDate(w.record), EffectiveDate: MustDate(w.effective),
			Ratio: Ratio{Num: 3, Den: 1},
		})
		if !errors.Is(err, ErrOverlappingAction) {
			t.Fatalf("第 %d 例重叠窗口应拒绝, got %v", i, err)
		}
	}

	mustRegister(t, s, "CA2", "2026-03-11", "2026-03-20", Ratio{Num: 3, Den: 1})
	if _, err := s.Register(RegisterCmd{
		ActionID: "CAB", FundID: testFund, ShareClass: "B",
		RecordDate: MustDate("2026-03-05"), EffectiveDate: MustDate("2026-03-08"),
		Ratio: Ratio{Num: 2, Den: 1},
	}); err != nil {
		t.Fatalf("不同份额类型不应冲突: %v", err)
	}

	if err := s.CancelAction("CA1", "取消"); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "CA3", "2026-03-05", "2026-03-08", Ratio{Num: 2, Den: 1})
}

// 登记日边界:生效日 <= 登记日的申购/赎回/冻结纳入快照;
// 登记日之后、生效日之前的交易不纳入;快照冻结后拒绝补登。
func TestRecordDateBoundary(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-02-28", 100000)
	mustTrade(t, s, "T2", "B", "2026-03-01", 20000)
	mustFreeze(t, s, "F1", "A", "2026-03-01", 30000)
	mustTrade(t, s, "T3", "A", "2026-03-02", 50000)
	mustTrade(t, s, "T4", "C", "2026-03-03", 90000)
	mustFreeze(t, s, "F2", "B", "2026-03-02", 10000)

	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatalf("ConfirmAction: %v", err)
	}
	snap := snapshotMap(t, s, "CA1")
	if len(snap) != 2 {
		t.Fatalf("快照应只含 A、B: %v", snap)
	}
	if a := snap["A"]; a.TotalShares != 100000 || a.FrozenShares != 30000 {
		t.Fatalf("A 快照错误: %+v", a)
	}
	if b := snap["B"]; b.TotalShares != 20000 || b.FrozenShares != 0 {
		t.Fatalf("B 快照错误: %+v", b)
	}

	err := s.PostTrade(TradeCmd{TradeID: "T5", FundID: testFund, ShareClass: testClass,
		HolderID: "A", TradeDate: MustDate("2026-03-01"), Shares: 100})
	if !errors.Is(err, ErrBackdatedEntry) {
		t.Fatalf("补登交易应拒绝, got %v", err)
	}
	err = s.PostFreeze(FreezeCmd{RequestID: "F3", FundID: testFund, ShareClass: testClass,
		HolderID: "A", Date: MustDate("2026-02-28"), Delta: 100})
	if !errors.Is(err, ErrBackdatedEntry) {
		t.Fatalf("补登冻结应拒绝, got %v", err)
	}
	mustTrade(t, s, "T6", "A", "2026-03-04", 7000)
	snap = snapshotMap(t, s, "CA1")
	if snap["A"].TotalShares != 100000 {
		t.Fatalf("登记日后交易改变了快照: %+v", snap["A"])
	}
}

// 零碎份额:1:2 合并且旧份额含奇数最小单位时,新份额向下取整,
// 差额以 1e-6 份精度保留并按面值 1 元/份四舍五入返还现金。
//
// 份额单位为 0.01 份(内部 1 单位 = 0.01):
//   - A 100.00 份 → 50.00 份,无零碎
//   - B  0.03 份 → 0.01 份 + 0.005 份零碎(=5000 micro,现金 0.005 元 → 四舍五入 1 分)
//   - C  0.01 份 → 0.00 份 + 0.005 份零碎(现金 1 分)
func TestMergeFractionalShares(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-03-01", 10000)
	mustTrade(t, s, "T2", "B", "2026-03-01", 3)
	mustTrade(t, s, "T3", "C", "2026-03-01", 1)
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 1, Den: 2})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 1, Den: 2}))
	if err != nil {
		t.Fatal(err)
	}

	by := convMap(t, s, "CA1")
	a := by["A"]
	if a.AfterShares != 5000 || a.FractionalShares != 0 || a.CashInLieu != 0 {
		t.Fatalf("A 转换错误: %+v", a)
	}
	for _, h := range []string{"B", "C"} {
		c := by[h]
		if c.FractionalShares != 5000 || c.CashInLieu != 1 {
			t.Fatalf("%s 零碎份额应为 5000micro/1 分: %+v", h, c)
		}
	}
	if by["B"].ActionVersion != 1 || by["B"].BeforeShares != 3 {
		t.Fatalf("B 版本/原数量错误: %+v", by["B"])
	}
	if by["C"].BeforeShares != 1 {
		t.Fatalf("C 原数量错误: %+v", by["C"])
	}
	if by["B"].AfterShares != 1 || by["C"].AfterShares != 0 {
		t.Fatalf("取整结果错误: B=%d C=%d", by["B"].AfterShares, by["C"].AfterShares)
	}
	if res.Direction != DirectionMerge || res.HolderCount != 3 ||
		res.TotalBefore != 10004 || res.TotalAfter != 5001 ||
		res.TotalFraction != 10000 || res.TotalCashInLieu != 2 {
		t.Fatalf("执行汇总错误: %+v", res)
	}
	// 入账核对:旧份额注销为负数流水,新份额与零碎现金为正数流水。
	totalA, frozenA := s.Holdings(testFund, testClass, "A", MustDate("2026-12-31"))
	if totalA != 5000 || frozenA != 0 {
		t.Fatalf("A 入账错误: total=%d frozen=%d", totalA, frozenA)
	}
	if got := s.CashBalance(testFund, testClass, "B"); got != 1 {
		t.Fatalf("B 零碎现金应为 1 分, got %v", got)
	}
}

// 拆分边界:3:1 拆分,所有份额整除时零碎与现金均为零;
// 登记日前冻结的份额按同一比例转换并继续冻结。
func TestSplitWithFrozenShares(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-02-28", 10000) // 100 份
	mustFreeze(t, s, "F1", "A", "2026-03-01", 4000) // 冻结 40 份
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 3, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 3, Den: 1}))
	if err != nil {
		t.Fatal(err)
	}
	c := convMap(t, s, "CA1")["A"]
	if c.BeforeShares != 10000 || c.BeforeFrozen != 4000 ||
		c.AfterShares != 30000 || c.AfterFrozen != 12000 ||
		c.FractionalShares != 0 || c.CashInLieu != 0 {
		t.Fatalf("拆分明细错误: %+v", c)
	}
	if res.Direction != DirectionSplit || res.TotalAfter != 30000 {
		t.Fatalf("拆分汇总错误: %+v", res)
	}
	total, frozen := s.Holdings(testFund, testClass, "A", MustDate("2026-12-31"))
	if total != 30000 || frozen != 12000 {
		t.Fatalf("转换后持仓错误: total=%d frozen=%d", total, frozen)
	}
}

// 取消:执行前可取消且幂等;开始执行后不可取消;取消后快照仍可查询。
func TestCancelRules(t *testing.T) {
	s := NewService()
	mustTrade(t, s, "T1", "A", "2026-03-01", 100)
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelAction("CA1", "撤单"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelAction("CA1", "重复取消"); err != nil {
		t.Fatalf("重复取消应幂等: %v", err)
	}
	a, _ := s.GetAction("CA1")
	if a.Status != ActionCancelled {
		t.Fatalf("状态应为 CANCELLED, got %s", a.Status)
	}
	snap := snapshotMap(t, s, "CA1")
	if len(snap) != 1 {
		t.Fatalf("取消后快照应保留: %v", snap)
	}
	if _, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})); !errors.Is(err, ErrActionCancelled) {
		t.Fatalf("已取消行动执行应拒绝, got %v", err)
	}

	mustRegister(t, s, "CA2", "2026-04-01", "2026-04-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(execReq("CA2", "2026-04-01", "2026-04-05", Ratio{Num: 2, Den: 1})); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelAction("CA2", "执行后撤"); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("执行后不可取消, got %v", err)
	}
}

// 状态历史:每次迁移持久化一条,含空 → DRAFT 的登记记录。
func TestStatusHistoryPersisted(t *testing.T) {
	s := NewService()
	mustRegister(t, s, "CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})
	if err := s.ConfirmAction("CA1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(execReq("CA1", "2026-03-01", "2026-03-05", Ratio{Num: 2, Den: 1})); err != nil {
		t.Fatal(err)
	}
	h, err := s.StatusHistory("CA1")
	if err != nil {
		t.Fatal(err)
	}
	want := []ActionStatus{ActionDraft, ActionConfirmed, ActionExecuting, ActionExecuted}
	if len(h) != len(want) {
		t.Fatalf("状态历史条数错误: %+v", h)
	}
	for i, sc := range h {
		if sc.To != want[i] {
			t.Fatalf("第 %d 条应为 %s: %+v", i, want[i], sc)
		}
	}
}
