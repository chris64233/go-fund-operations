package fundoperations

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }

// 2026-10-09 周五、2026-10-12 周一、2026-10-13 周二为交易日。
func newTestService(t *testing.T) *Service {
	t.Helper()
	svc := NewService(&testClock{t: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}, nil)
	err := svc.RegisterFund(Fund{
		ID:              "F001",
		Name:            "测试基金",
		Status:          FundStatusTrading,
		Currency:        "CNY",
		MinSubscription: 1000, // 10.00 元
		Calendar:        NewCalendar("2026-10-09", "2026-10-12", "2026-10-13"),
		CutoffTime:      "15:00",
		Timezone:        "UTC",
	})
	if err != nil {
		t.Fatalf("RegisterFund: %v", err)
	}
	return svc
}

func submitAt(fundID, extID string, amount int64, ts time.Time) SubscriptionRequest {
	return SubscriptionRequest{
		FundID:      fundID,
		ExternalID:  extID,
		InvestorID:  "INV-1",
		Amount:      amount,
		Currency:    "CNY",
		SubmittedAt: ts,
	}
}

func TestSubscribeValidation(t *testing.T) {
	svc := newTestService(t)
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	// 金额低于起点
	if _, err := svc.Subscribe(submitAt("F001", "E-low", 999, ts)); !errors.Is(err, ErrBelowMinimum) {
		t.Fatalf("低于起点: got %v", err)
	}
	// 币种不符
	bad := submitAt("F001", "E-ccy", 1000, ts)
	bad.Currency = "USD"
	if _, err := svc.Subscribe(bad); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("币种不符: got %v", err)
	}
	// 暂停交易
	if err := svc.SetFundStatus("F001", FundStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Subscribe(submitAt("F001", "E-susp", 1000, ts)); !errors.Is(err, ErrFundSuspended) {
		t.Fatalf("暂停交易: got %v", err)
	}
	// 被拒申请不得进入待处理批次
	for _, id := range []string{"E-low", "E-ccy", "E-susp"} {
		if _, err := svc.GetApplication("F001", id); !errors.Is(err, ErrApplicationNotFound) {
			t.Fatalf("%s 不应存在: %v", id, err)
		}
	}
	if _, err := svc.GetBatch("F001", "2026-10-09"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("不应产生批次: %v", err)
	}
}

func TestCutoffBoundary(t *testing.T) {
	svc := newTestService(t)
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		extID string
		at    time.Time
		want  string
	}{
		{"E1", day.Add(14*time.Hour + 59*time.Minute + 59*time.Second), "2026-10-09"},
		{"E2", day.Add(15 * time.Hour), "2026-10-09"},                        // 截止时刻归入当日
		{"E3", day.Add(15*time.Hour + time.Second), "2026-10-12"},            // 截止后进入下一交易日
		{"E4", time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC), "2026-10-12"}, // 非交易日顺延
	}
	for _, c := range cases {
		app, err := svc.Subscribe(submitAt("F001", c.extID, 100000, c.at))
		if err != nil {
			t.Fatalf("%s: %v", c.extID, err)
		}
		if app.ValuationDate != c.want {
			t.Fatalf("%s 估值日 = %s, want %s", c.extID, app.ValuationDate, c.want)
		}
	}
	// 截止前后分别进入两个不同批次
	b1, err := svc.GetBatch("F001", "2026-10-09")
	if err != nil || len(b1.ExternalIDs) != 2 {
		t.Fatalf("当日批次: %+v, %v", b1, err)
	}
	b2, err := svc.GetBatch("F001", "2026-10-12")
	if err != nil || len(b2.ExternalIDs) != 2 {
		t.Fatalf("次日批次: %+v, %v", b2, err)
	}
}

func TestLateApplicationRejectedAfterClose(t *testing.T) {
	svc := newTestService(t)
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if _, err := svc.Subscribe(submitAt("F001", "E1", 100000, ts)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishNAV("F001", "2026-10-09", 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	// 迟到申请不能重新打开已关闭批次
	if _, err := svc.Subscribe(submitAt("F001", "E-late", 100000, ts)); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("迟到申请: got %v", err)
	}
	b, err := svc.GetBatch("F001", "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BatchClosed || len(b.ExternalIDs) != 1 {
		t.Fatalf("批次被迟到申请改变: %+v", b)
	}
}

func TestConfirmRoundingAndNAVCorrection(t *testing.T) {
	svc := newTestService(t)
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if _, err := svc.Subscribe(submitAt("F001", "E1", 1000000, ts)); err != nil { // 10000.00 元
		t.Fatal(err)
	}
	// 批次未关闭不能确认
	if _, err := svc.ConfirmBatch("F001", "2026-10-09"); !errors.Is(err, ErrBatchNotClosed) {
		t.Fatalf("未关闭确认: got %v", err)
	}
	if _, err := svc.PublishNAV("F001", "2026-10-09", 12345); err != nil { // 1.2345 元
		t.Fatal(err)
	}
	if _, err := svc.CloseBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	b, err := svc.ConfirmBatch("F001", "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	// 份额 = floor(1000000 * 10000 / 12345) = 810044（8100.44 份）
	// 确认金额 = floor(810044 * 12345 / 10000) = 999999（9999.99 元），差额 1 分留存
	app, err := svc.GetApplication("F001", "E1")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != StatusConfirmed || app.Shares != 810044 || app.ConfirmedAmount != 999999 ||
		app.RoundingRemainder != 1 || app.NAVVersion != 1 || app.BatchID != "F001/2026-10-09" {
		t.Fatalf("确认结果错误: %+v", app)
	}
	if b.TotalShares != 810044 || b.TotalConfirmedAmount != 999999 || b.TotalRemainder != 1 || b.NAVVersion != 1 {
		t.Fatalf("批次快照错误: %+v", b)
	}

	// 净值更正产生新版本，但已确认结果不被改写
	if _, err := svc.PublishNAV("F001", "2026-10-09", 13000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	app, _ = svc.GetApplication("F001", "E1")
	if app.Shares != 810044 || app.ConfirmedAmount != 999999 || app.NAVVersion != 1 {
		t.Fatalf("净值更正改写了已确认结果: %+v", app)
	}
}

func TestDuplicateApplication(t *testing.T) {
	svc := newTestService(t)
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	req := submitAt("F001", "E1", 100000, ts)
	first, err := svc.Subscribe(req)
	if err != nil {
		t.Fatal(err)
	}
	// 相同内容重复提交返回原申请
	again, err := svc.Subscribe(req)
	if err != nil || again != first {
		t.Fatalf("幂等重提: %+v, %v", again, err)
	}
	// 内容变化返回冲突
	changed := req
	changed.Amount = 200000
	if _, err := svc.Subscribe(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("内容冲突: got %v", err)
	}
	// 确认后重复提交返回原确认结果
	if _, err := svc.PublishNAV("F001", "2026-10-09", 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Subscribe(req)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusConfirmed || after.Shares != 100000 {
		t.Fatalf("确认后重提: %+v", after)
	}
}

func TestConcurrentCloseConfirmAndDuplicateSubmit(t *testing.T) {
	svc := newTestService(t)
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	req := submitAt("F001", "E1", 500000, ts)
	if _, err := svc.Subscribe(req); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishNAV("F001", "2026-10-09", 10000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// 并发重复提交（相同内容）
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Subscribe(req); err != nil {
				t.Errorf("重复提交: %v", err)
			}
		}()
	}
	// 并发冲突提交（相同申请号、不同内容）
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := req
			c.Amount = 999999
			if _, err := svc.Subscribe(c); !errors.Is(err, ErrConflict) {
				t.Errorf("冲突提交: %v", err)
			}
		}()
	}
	// 并发关闭
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.CloseBatch("F001", "2026-10-09"); err != nil {
				t.Errorf("关闭批次: %v", err)
			}
		}()
	}
	// 并发确认（未关闭时重试）
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := svc.ConfirmBatch("F001", "2026-10-09")
				if errors.Is(err, ErrBatchNotClosed) {
					continue
				}
				if err != nil {
					t.Errorf("确认批次: %v", err)
				}
				return
			}
		}()
	}
	wg.Wait()

	// 一笔申请只有一个最终结果，无重复份额、无半笔确认
	app, err := svc.GetApplication("F001", "E1")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != StatusConfirmed || app.Shares != 500000 || app.ConfirmedAmount != 500000 {
		t.Fatalf("最终结果错误: %+v", app)
	}
	b, err := svc.GetBatch("F001", "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BatchConfirmed || len(b.ExternalIDs) != 1 ||
		b.TotalShares != app.Shares || b.TotalConfirmedAmount != app.ConfirmedAmount {
		t.Fatalf("批次快照不一致: %+v", b)
	}
}

func TestFilePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	svc, err := NewFileService(path, &testClock{t: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterFund(Fund{
		ID: "F001", Currency: "CNY", MinSubscription: 1000,
		Calendar: NewCalendar("2026-10-09"), CutoffTime: "15:00", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if _, err := svc.Subscribe(submitAt("F001", "E1", 100000, ts)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishNAV("F001", "2026-10-09", 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmBatch("F001", "2026-10-09"); err != nil {
		t.Fatal(err)
	}

	// 重新加载后批次快照与申请状态完整保留
	reloaded, err := NewFileService(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	app, err := reloaded.GetApplication("F001", "E1")
	if err != nil || app.Status != StatusConfirmed || app.Shares != 100000 {
		t.Fatalf("重载申请: %+v, %v", app, err)
	}
	b, err := reloaded.GetBatch("F001", "2026-10-09")
	if err != nil || b.Status != BatchConfirmed || b.NAVVersion != 1 || b.TotalShares != 100000 {
		t.Fatalf("重载批次: %+v, %v", b, err)
	}
}
