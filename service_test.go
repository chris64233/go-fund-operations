package fundoperations

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var testDate = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func mustPublish(t *testing.T, s *Service, fund string, nav NAV, basis string) *NAVRecord {
	t.Helper()
	rec, err := s.PublishNAV(fund, testDate, nav, basis)
	if err != nil {
		t.Fatalf("PublishNAV: %v", err)
	}
	return rec
}

// 重复发布相同内容返回原版本。
func TestDuplicatePublishReturnsOriginal(t *testing.T) {
	s := NewService()
	first := mustPublish(t, s, "F001", 10000, "basis-1")
	second, err := s.PublishNAV("F001", testDate, 10000, "basis-1")
	if err != nil {
		t.Fatalf("duplicate publish should succeed: %v", err)
	}
	if second.ID != first.ID || second.Version != 1 {
		t.Fatalf("expected original version, got %+v", second)
	}
}

// 内容变化时发布必须返回版本冲突。
func TestPublishConflictOnChangedContent(t *testing.T) {
	s := NewService()
	mustPublish(t, s, "F001", 10000, "basis-1")
	if _, err := s.PublishNAV("F001", testDate, 10001, "basis-1"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	if _, err := s.PublishNAV("F001", testDate, 10000, "basis-2"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	cur, err := s.CurrentNAV("F001", testDate)
	if err != nil {
		t.Fatalf("CurrentNAV: %v", err)
	}
	if cur.NAV != 10000 || cur.Version != 1 {
		t.Fatalf("current version must stay unchanged, got %+v", cur)
	}
}

// 未被引用的草稿可以撤回；已发布的版本不能按草稿撤回。
func TestWithdrawUnusedDraft(t *testing.T) {
	s := NewService()
	draft, err := s.SaveDraft("F001", testDate, 10000, "basis-1")
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.WithdrawDraft(draft.ID); err != nil {
		t.Fatalf("WithdrawDraft: %v", err)
	}
	rec, err := s.GetNAV(draft.ID)
	if err != nil {
		t.Fatalf("GetNAV: %v", err)
	}
	if rec.Status != NAVStatusWithdrawn {
		t.Fatalf("expected WITHDRAWN, got %s", rec.Status)
	}
	published := mustPublish(t, s, "F002", 10000, "basis-1")
	if err := s.WithdrawDraft(published.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
}

// 草稿不能被交易确认引用。
func TestConfirmRejectsDraft(t *testing.T) {
	s := NewService()
	draft, err := s.SaveDraft("F001", testDate, 10000, "basis-1")
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := s.ConfirmTrade("F001", testDate, draft.ID, TradeSubscribe, 10000); !errors.Is(err, ErrNAVNotPublished) {
		t.Fatalf("expected ErrNAVNotPublished, got %v", err)
	}
}

// 已引用版本被更正：原版本冻结、引用关系不变、差额逐笔生成。
func TestCorrectReferencedVersion(t *testing.T) {
	s := NewService()
	v1 := mustPublish(t, s, "F001", 10000, "basis-1")
	conf, err := s.ConfirmTrade("F001", testDate, v1.ID, TradeSubscribe, 10000) // 100.00 份
	if err != nil {
		t.Fatalf("ConfirmTrade: %v", err)
	}
	if conf.Amount != 10000 { // 100.00 * 1.0000 = 100.00 元
		t.Fatalf("unexpected confirmed amount %d", conf.Amount)
	}
	res, err := s.CorrectNAV("F001", testDate, 10050, "basis-2", "估值表取数错误", "operator-a")
	if err != nil {
		t.Fatalf("CorrectNAV: %v", err)
	}
	if res.NewVersion.Version != 2 || res.NewVersion.CorrectionOf != v1.ID {
		t.Fatalf("new version must reference original: %+v", res.NewVersion)
	}
	if res.NewVersion.Reason != "估值表取数错误" || res.NewVersion.CorrectedBy != "operator-a" {
		t.Fatalf("correction metadata missing: %+v", res.NewVersion)
	}
	old, err := s.GetNAV(v1.ID)
	if err != nil {
		t.Fatalf("GetNAV: %v", err)
	}
	if old.Status != NAVStatusSuperseded || old.NAV != 10000 {
		t.Fatalf("original record must stay frozen, got %+v", old)
	}
	frozen, err := s.GetConfirmation(conf.ID)
	if err != nil {
		t.Fatalf("GetConfirmation: %v", err)
	}
	if frozen.Amount != 10000 || frozen.NAVVersionID != v1.ID {
		t.Fatalf("original confirmation must not change, got %+v", frozen)
	}
	// 差额逐笔关联原确认：100.00 * 1.0050 = 100.50，应补收 50 分。
	if len(res.Adjustments) != 1 {
		t.Fatalf("expected 1 adjustment, got %d", len(res.Adjustments))
	}
	adj := res.Adjustments[0]
	if adj.ConfirmationID != conf.ID || adj.FromVersionID != v1.ID || adj.ToVersionID != res.NewVersion.ID {
		t.Fatalf("adjustment must link original confirmation and versions: %+v", adj)
	}
	if adj.OldAmount != 10000 || adj.NewAmount != 10050 || adj.Delta != 50 {
		t.Fatalf("unexpected delta: %+v", adj)
	}
	if adj.Status != AdjustmentPending {
		t.Fatalf("adjustment must start pending, got %s", adj.Status)
	}
	// 从当前版本可回溯到原版本。
	history, err := s.NAVHistory("F001", testDate)
	if err != nil {
		t.Fatalf("NAVHistory: %v", err)
	}
	if len(history) != 2 || history[0].ID != res.NewVersion.ID || history[1].ID != v1.ID {
		t.Fatalf("unexpected history: %+v", history)
	}
	// 受影响交易与处理状态可查。
	byConf := s.AdjustmentsByConfirmation(conf.ID)
	if len(byConf) != 1 || byConf[0].ID != adj.ID {
		t.Fatalf("AdjustmentsByConfirmation: %+v", byConf)
	}
	processed, err := s.ProcessAdjustment(adj.ID)
	if err != nil {
		t.Fatalf("ProcessAdjustment: %v", err)
	}
	if processed.Status != AdjustmentProcessed {
		t.Fatalf("expected PROCESSED, got %s", processed.Status)
	}
}

// 基于旧版本的迟到确认返回明确冲突，不能把新版本降回旧版本。
func TestStaleConfirmationRejected(t *testing.T) {
	s := NewService()
	v1 := mustPublish(t, s, "F001", 10000, "basis-1")
	if _, err := s.CorrectNAV("F001", testDate, 10050, "basis-2", "修正", "op"); err != nil {
		t.Fatalf("CorrectNAV: %v", err)
	}
	if _, err := s.ConfirmTrade("F001", testDate, v1.ID, TradeRedeem, 10000); !errors.Is(err, ErrStaleNAVVersion) {
		t.Fatalf("expected ErrStaleNAVVersion, got %v", err)
	}
	cur, err := s.CurrentNAV("F001", testDate)
	if err != nil {
		t.Fatalf("CurrentNAV: %v", err)
	}
	if cur.NAV != 10050 || cur.Version != 2 {
		t.Fatalf("current version must stay at v2, got %+v", cur)
	}
	if got := s.ConfirmationsByNAV(v1.ID); len(got) != 0 {
		t.Fatalf("stale confirmation must not be recorded, got %+v", got)
	}
}

// 并发发布：只允许一个版本成为当前有效版本。
func TestConcurrentPublishSingleWinner(t *testing.T) {
	s := NewService()
	const workers = 16
	var wg sync.WaitGroup
	ids := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := s.PublishNAV("F001", testDate, NAV(10000+i), "basis-1")
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = rec.ID
		}(i)
	}
	wg.Wait()
	winner := ""
	conflicts := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			if !errors.Is(errs[i], ErrVersionConflict) {
				t.Fatalf("unexpected error: %v", errs[i])
			}
			conflicts++
			continue
		}
		if winner == "" {
			winner = ids[i]
		} else if ids[i] != winner {
			t.Fatalf("multiple current versions: %s vs %s", winner, ids[i])
		}
	}
	if winner == "" {
		t.Fatal("no publish succeeded")
	}
	if conflicts == 0 {
		t.Fatal("expected at least one version conflict")
	}
	cur, err := s.CurrentNAV("F001", testDate)
	if err != nil {
		t.Fatalf("CurrentNAV: %v", err)
	}
	if cur.ID != winner || cur.Version != 1 {
		t.Fatalf("exactly one current version expected, got %+v", cur)
	}
}

// 确认与更正并发：确认要么落在旧版本并产生差额，要么被明确拒绝；
// 任何情况下当前版本都是新版本，且不存在孤立的差额记录。
func TestConcurrentConfirmAndCorrect(t *testing.T) {
	for run := 0; run < 50; run++ {
		s := NewService()
		v1 := mustPublish(t, s, "F001", 10000, "basis-1")
		var wg sync.WaitGroup
		var conf *TradeConfirmation
		var confErr error
		var res *CorrectionResult
		var corrErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			conf, confErr = s.ConfirmTrade("F001", testDate, v1.ID, TradeSubscribe, 10000)
		}()
		go func() {
			defer wg.Done()
			res, corrErr = s.CorrectNAV("F001", testDate, 10050, "basis-2", "修正", "op")
		}()
		wg.Wait()
		if corrErr != nil {
			t.Fatalf("run %d: CorrectNAV: %v", run, corrErr)
		}
		cur, err := s.CurrentNAV("F001", testDate)
		if err != nil {
			t.Fatalf("run %d: CurrentNAV: %v", run, err)
		}
		if cur.ID != res.NewVersion.ID {
			t.Fatalf("run %d: current version regressed", run)
		}
		switch {
		case confErr == nil:
			// 确认先于更正生效：必须产生一笔关联该确认的差额。
			adjs := s.AdjustmentsByConfirmation(conf.ID)
			if len(adjs) != 1 || adjs[0].Delta != 50 {
				t.Fatalf("run %d: expected one delta=50 adjustment, got %+v", run, adjs)
			}
		case errors.Is(confErr, ErrStaleNAVVersion):
			// 确认迟到：不得留下任何确认或差额记录。
			if got := s.ConfirmationsByNAV(v1.ID); len(got) != 0 {
				t.Fatalf("run %d: stale confirmation recorded: %+v", run, got)
			}
			if got := s.AdjustmentsByCorrection(res.Correction.ID); len(got) != 0 {
				t.Fatalf("run %d: orphan adjustments: %+v", run, got)
			}
		default:
			t.Fatalf("run %d: unexpected confirm error: %v", run, confErr)
		}
	}
}

// 金额与差额按统一精度四舍五入到分。
func TestAmountRounding(t *testing.T) {
	cases := []struct {
		shares Shares
		nav    NAV
		want   Amount
	}{
		{10000, 10000, 10000}, // 100.00 * 1.0000 = 100.00
		{1, 5000, 1},          // 0.01 * 0.5000 = 0.005 -> 0.01（half-up）
		{1, 4999, 0},          // 0.01 * 0.4999 = 0.004999 -> 0.00
		{5000, 10005, 5003},   // 50.00 * 1.0005 = 50.025 -> 50.03
		{333, 12345, 411},     // 3.33 * 1.2345 = 4.110885 -> 4.11
	}
	for _, c := range cases {
		if got := AmountFor(c.shares, c.nav); got != c.want {
			t.Fatalf("AmountFor(%d, %d) = %d, want %d", c.shares, c.nav, got, c.want)
		}
	}
}

// 更正差额同样按统一精度计算，逐笔独立舍入。
func TestCorrectionDeltaRounding(t *testing.T) {
	s := NewService()
	v1 := mustPublish(t, s, "F001", 10000, "basis-1")
	conf, err := s.ConfirmTrade("F001", testDate, v1.ID, TradeSubscribe, 5000) // 50.00 份
	if err != nil {
		t.Fatalf("ConfirmTrade: %v", err)
	}
	if conf.Amount != 5000 {
		t.Fatalf("unexpected amount %d", conf.Amount)
	}
	// 50.00 * 1.0005 = 50.025 -> 50.03，差额 3 分。
	res, err := s.CorrectNAV("F001", testDate, 10005, "basis-2", "精度修正", "op")
	if err != nil {
		t.Fatalf("CorrectNAV: %v", err)
	}
	if len(res.Adjustments) != 1 {
		t.Fatalf("expected 1 adjustment, got %d", len(res.Adjustments))
	}
	adj := res.Adjustments[0]
	if adj.NewAmount != 5003 || adj.Delta != 3 {
		t.Fatalf("unexpected rounded delta: %+v", adj)
	}
}

// 更正内容与原版本完全一致时拒绝登记。
func TestCorrectWithoutChangeRejected(t *testing.T) {
	s := NewService()
	mustPublish(t, s, "F001", 10000, "basis-1")
	if _, err := s.CorrectNAV("F001", testDate, 10000, "basis-1", "无变化", "op"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("expected ErrNoChange, got %v", err)
	}
}
