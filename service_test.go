package fundoperations

import (
	"errors"
	"testing"
)

const (
	fund = "F001"
	date = "2026-09-30"
)

func nav(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s, navScale)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func shares(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s, shareScale)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// publish 草稿并发布，返回版本号。
func publish(t *testing.T, svc *Service, value, basis string) int {
	t.Helper()
	_, id, err := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, value), Basis: basis})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.PublishDraft(fund, date, id)
	if err != nil {
		t.Fatal(err)
	}
	return res.Version.Version
}

func TestDraftPublishAndCurrentVersion(t *testing.T) {
	svc := NewService()
	v1 := publish(t, svc, "1.0000", "basis-1")
	if v1 != 1 {
		t.Fatalf("first version = %d, want 1", v1)
	}
	cur, err := svc.CurrentNAV(fund, date)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Version != 1 || cur.Status != StatusPublished || cur.UnitNAV.String() != "1.0000" {
		t.Fatalf("unexpected current: %+v", cur)
	}
	if cur.PublishedAt.IsZero() {
		t.Fatal("publishedAt must be set")
	}
}

func TestRepublishSameContentReturnsOriginal(t *testing.T) {
	svc := NewService()
	v1 := publish(t, svc, "1.0000", "basis-1")

	_, id2, _ := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, "1.0000"), Basis: "basis-1"})
	res, err := svc.PublishDraft(fund, date, id2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SameContent || res.Version.Version != v1 {
		t.Fatalf("same content must return original v%d, got %+v", v1, res)
	}
	if len(svc.ListNAVVersions(fund, date)) != 1 {
		t.Fatal("identical republish must not create a version")
	}
}

func TestPublishDifferentContentConflicts(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "basis-1")

	_, id2, _ := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, "1.2000"), Basis: "basis-1"})
	_, err := svc.PublishDraft(fund, date, id2)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != 1 {
		t.Fatalf("conflict must carry current version: %v", err)
	}

	// 仅计算依据变化也视为内容变化。
	_, id3, _ := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, "1.0000"), Basis: "basis-2"})
	if _, err := svc.PublishDraft(fund, date, id3); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("basis change must conflict, got %v", err)
	}
	cur, _ := svc.CurrentNAV(fund, date)
	if cur.Version != 1 {
		t.Fatal("failed publish must not advance the current version")
	}
}

func TestWithdrawUnusedDraft(t *testing.T) {
	svc := NewService()
	_, id, err := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, "1.0000"), Basis: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WithdrawDraft(fund, date, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishDraft(fund, date, id); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("withdrawn draft cannot publish, got %v", err)
	}
	active := svc.ListDrafts(fund, date, false)
	if len(active) != 0 {
		t.Fatalf("withdrawn draft hidden by default, got %d", len(active))
	}
	all := svc.ListDrafts(fund, date, true)
	if len(all) != 1 || all[0].Status != StatusWithdrawn {
		t.Fatal("withdrawn draft should remain auditable")
	}
}

func TestConfirmFreezesReferencedVersion(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	c, err := svc.ConfirmTx(ConfirmTxInput{
		ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.NAVVersion != 1 || c.Amount.String() != "100.00" {
		t.Fatalf("unexpected confirmation: %+v", c)
	}
	// 已被引用的版本不能撤回；不同内容不能直接覆盖发布。
	_, id, _ := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, "0.9000"), Basis: "b2"})
	if err := svc.WithdrawDraft(fund, date, "DRF-1"); err == nil {
		t.Fatal("referenced published version must not be withdrawable via draft path")
	}
	if _, err := svc.PublishDraft(fund, date, id); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("overwrite blocked: %v", err)
	}
}

func TestCorrectionCreatesVersionAndPerConfirmationAdjustments(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")

	sub1, _ := svc.ConfirmTx(ConfirmTxInput{ConfirmID: "SUB1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})
	red1, _ := svc.ConfirmTx(ConfirmTxInput{ConfirmID: "RED1", FundID: fund, ValueDate: date, TxType: TxRedemption, Shares: shares(t, "200")})
	_, _ = svc.ConfirmTx(ConfirmTxInput{ConfirmID: "SUB2", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "300")})
	_ = red1

	res, err := svc.CorrectNAV(CorrectionInput{
		FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0200"), NewBasis: "b2",
		Reason: "估值差错", CorrectedBy: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version.Version != 2 || res.Version.BasedOn != 1 || res.Version.Reason != "估值差错" {
		t.Fatalf("unexpected correction version: %+v", res.Version)
	}
	// 原版本冻结为 superseded，数值不被修改。
	old, _ := svc.NAVVersion(fund, date, 1)
	if old.Status != StatusSuperseded || old.UnitNAV.String() != "1.0000" {
		t.Fatalf("original version frozen wrong: %+v", old)
	}
	cur, _ := svc.CurrentNAV(fund, date)
	if cur.Version != 2 {
		t.Fatal("new version must be current")
	}
	if len(res.Adjustments) != 3 {
		t.Fatalf("want 3 adjustments (one per confirm), got %d", len(res.Adjustments))
	}

	byConfirm := map[string]*Adjustment{}
	for _, a := range res.Adjustments {
		byConfirm[a.ConfirmID] = a
	}
	// 申购：净值上浮 → 应补收 100 * 0.02 = 2.00。
	if a := byConfirm["SUB1"]; a.Type != AdjustCollect || a.Amount.String() != "2.00" ||
		a.FromNAVVersion != 1 || a.ToNAVVersion != 2 || a.Status != AdjustPending {
		t.Fatalf("SUB1 adjustment wrong: %+v", a)
	}
	// 赎回：净值上浮方向相反 → 应退回 200 * 0.02 = 4.00（负数表示退）。
	if a := byConfirm["RED1"]; a.Type != AdjustRefund || a.Amount.String() != "-4.00" {
		t.Fatalf("RED1 adjustment wrong: %+v", a)
	}
	if a := byConfirm["SUB2"]; a.Amount.String() != "6.00" {
		t.Fatalf("SUB2 adjustment wrong: %+v", a)
	}

	// 原确认结果保持不变，不允许只改余额数字。
	if sub1.Amount.String() != "100.00" || sub1.NAVAtConfirm.String() != "1.0000" {
		t.Fatalf("original confirmation mutated: %+v", sub1)
	}
	got, _ := svc.Confirmation(fund, date, "SUB1")
	if got.Amount.String() != "100.00" || got.NAVVersion != 1 {
		t.Fatalf("stored confirmation mutated: %+v", got)
	}
}

func TestAdjustmentRoundingAtUnifiedPrecision(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	// 100 份 * 0.0005 = 0.05，精确到分。
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "A", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})
	res, err := svc.CorrectNAV(CorrectionInput{
		FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0005"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Adjustments[0].Amount.String() != "0.05" {
		t.Fatalf("want 0.05, got %s", res.Adjustments[0].Amount)
	}

	// 3 份 * 0.0005 = 0.0015 → 四舍五入到分 = 0.00（仍生成逐笔记录，金额为零）。
	// 改用新基金日避免相互影响。
	svc2 := NewService()
	_, id, _ := svc2.CreateDraft(DraftInput{FundID: "F2", ValueDate: date, UnitNAV: nav(t, "1.0000"), Basis: "b"})
	svc2.PublishDraft("F2", date, id)
	svc2.ConfirmTx(ConfirmTxInput{ConfirmID: "A", FundID: "F2", ValueDate: date, TxType: TxSubscription, Shares: shares(t, "3")})
	r2, err := svc2.CorrectNAV(CorrectionInput{
		FundID: "F2", ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0005"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Adjustments) != 1 || r2.Adjustments[0].Amount.String() != "0.00" {
		t.Fatalf("tiny delta still gets per-tx record rounded to 0.00, got %+v", r2.Adjustments)
	}
}

func TestStaleConfirmationRejectedAndNoOrphans(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "OLD", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})

	// 旧版本在交易采用后被更正。
	cr, err := svc.CorrectNAV(CorrectionInput{
		FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.1000"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 基于旧版本的迟到确认必须被拒绝，且不产生确认或差额。
	_, err = svc.ConfirmTx(ConfirmTxInput{
		ConfirmID: "LATE", FundID: fund, ValueDate: date, TxType: TxSubscription,
		Shares: shares(t, "50"), NAVVersion: 1,
	})
	if !errors.Is(err, ErrStaleNAVReference) {
		t.Fatalf("want ErrStaleNAVReference, got %v", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != 2 || ce.SeenVersion != 1 {
		t.Fatalf("conflict detail wrong: %v", err)
	}
	if _, err := svc.Confirmation(fund, date, "LATE"); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale confirm must not be stored")
	}
	if len(svc.ListConfirmations(fund, date)) != 1 {
		t.Fatal("no new confirmation may be created")
	}
	// 差额只来自更正时刻已存在的 OLD，绝无 LATE 的孤立差额。
	adjs := svc.ListAdjustments(fund, date, AdjustmentFilter{})
	if len(adjs) != 1 || adjs[0].ConfirmID != "OLD" || adjs[0].ToNAVVersion != cr.Version.Version {
		t.Fatalf("orphan adjustment check failed: %+v", adjs)
	}
}

func TestChainedCorrectionsOnlyAffectReferencedVersion(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})

	// v1 -> v2 (1.1000)，C1 引用 v1，产生 ADJ-1。
	r2, err := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.1000"), NewBasis: "b2", Reason: "r1", CorrectedBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	// C2 基于 v2 确认。
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C2", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})

	// v2 -> v3 (1.2000)：只影响引用 v2 的 C2，C1 不重复处理。
	r3, err := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 2,
		NewUnitNAV: nav(t, "1.2000"), NewBasis: "b3", Reason: "r2", CorrectedBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Version.Version != 3 || r3.Version.BasedOn != 2 {
		t.Fatalf("chain version wrong: %+v", r3.Version)
	}
	if len(r3.Adjustments) != 1 || r3.Adjustments[0].ConfirmID != "C2" {
		t.Fatalf("only C2 referencing v2 should be affected, got %+v", r3.Adjustments)
	}
	if r2.Adjustments[0].ConfirmID != "C1" {
		t.Fatal("first correction must only cover C1")
	}
	all := svc.ListAdjustments(fund, date, AdjustmentFilter{})
	if len(all) != 2 {
		t.Fatalf("total adjustments = %d, want 2", len(all))
	}
	// 基于已取代版本再更正必须冲突，不能把新版本降回旧版本。
	_, err = svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0500"), NewBasis: "bx", Reason: "late", CorrectedBy: "x"})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("correcting superseded version must conflict, got %v", err)
	}
	cur, _ := svc.CurrentNAV(fund, date)
	if cur.Version != 3 || cur.UnitNAV.String() != "1.2000" {
		t.Fatal("current must remain v3")
	}
}

func TestSettlementAndTrace(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})
	r2, _ := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0200"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob"})
	adjID := r2.Adjustments[0].ID

	settled, err := svc.SettleAdjustment(fund, date, adjID, "PAY-1")
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != AdjustSettled || settled.SettlementRef != "PAY-1" || settled.SettledAt.IsZero() {
		t.Fatalf("settle wrong: %+v", settled)
	}
	if _, err := svc.SettleAdjustment(fund, date, adjID, "PAY-2"); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("double settle rejected, got %v", err)
	}
	pending := svc.ListAdjustments(fund, date, AdjustmentFilter{Status: AdjustPending})
	if len(pending) != 0 {
		t.Fatal("no pending adjustments left")
	}

	trail, err := svc.TraceVersion(fund, date, 0)
	if err != nil {
		t.Fatal(err)
	}
	if trail.Current.Version != 2 {
		t.Fatalf("trace current = v%d, want v2", trail.Current.Version)
	}
	if len(trail.Chain) != 2 || trail.Chain[0].Version != 2 || trail.Chain[1].Version != 1 {
		t.Fatalf("chain must run current -> original: %+v", trail.Chain)
	}
	if trail.Chain[1].BasedOn != 0 {
		t.Fatal("original version has BasedOn=0")
	}
	if len(trail.Adjustments) != 1 || trail.Adjustments[0].ConfirmID != "C1" ||
		trail.Adjustments[0].Status != AdjustSettled {
		t.Fatalf("trace adjustments wrong: %+v", trail.Adjustments)
	}

	// 正向：原确认 -> 差额。
	perConfirm := svc.AdjustmentsForConfirmation(fund, date, "C1")
	if len(perConfirm) != 1 || perConfirm[0].ID != adjID {
		t.Fatalf("per-confirmation lookup wrong: %+v", perConfirm)
	}
}

func TestConfirmIdempotent(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	in := ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")}
	first, err := svc.ConfirmTx(in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.ConfirmTx(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfirmID != again.ConfirmID || again.Amount.String() != "100.00" {
		t.Fatal("repeat confirm must return the identical record")
	}
	if len(svc.ListConfirmations(fund, date)) != 1 {
		t.Fatal("duplicate confirm must not duplicate reference")
	}
}

func TestCorrectionRequiresReasonAndCannotRepeat(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	_, err := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0100"), NewBasis: "b2"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing reason/corrector rejected: %v", err)
	}
	r, err := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0000"), NewBasis: "b", Reason: "noop", CorrectedBy: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Version.Version != 1 || len(r.Adjustments) != 0 {
		t.Fatal("identical correction must be idempotent and create nothing")
	}
}

func TestCorrectionBasisOnlyCreatesNoAdjustments(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})
	r, err := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0000"), NewBasis: "b2-repriced", Reason: "依据重算", CorrectedBy: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Version.Version != 2 || len(r.Adjustments) != 0 {
		t.Fatalf("basis-only correction creates v2 but zero adjustments, got %+v", r)
	}
	trail, _ := svc.TraceVersion(fund, date, 0)
	if len(trail.Chain) != 2 || len(trail.Adjustments) != 0 {
		t.Fatalf("chain tracked but no money delta: %+v", trail)
	}
}

func TestValidationAndMissingEntities(t *testing.T) {
	svc := NewService()
	if _, _, err := svc.CreateDraft(DraftInput{ValueDate: date, UnitNAV: nav(t, "1"), Basis: "b"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing fund: %v", err)
	}
	if _, _, err := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: mustParseDecimal("0", navScale), Basis: "b"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero nav: %v", err)
	}
	if _, err := svc.CurrentNAV(fund, "2020-01-01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing current: %v", err)
	}
	if _, err := svc.Confirmation(fund, date, "X"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing confirmation: %v", err)
	}
	if _, err := svc.Adjustment(fund, date, "X"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing adjustment: %v", err)
	}
}
