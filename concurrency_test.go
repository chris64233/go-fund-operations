package fundoperations

import (
	"errors"
	"sync"
	"testing"
)

func TestConcurrentPublishOnlyOneCurrentVersion(t *testing.T) {
	svc := NewService()

	mkDraft := func(value, basis string) string {
		_, id, err := svc.CreateDraft(DraftInput{FundID: fund, ValueDate: date, UnitNAV: nav(t, value), Basis: basis})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	d1 := mkDraft("1.0000", "b1")
	d2 := mkDraft("1.1000", "b2")
	d3 := mkDraft("1.2000", "b3")

	var wg sync.WaitGroup
	results := make(chan error, 3)
	for _, id := range []string{d1, d2, d3} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := svc.PublishDraft(fund, date, id)
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)

	ok, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	if ok != 1 || conflicts != 2 {
		t.Fatalf("want 1 success 2 conflicts, got %d/%d", ok, conflicts)
	}
	if len(svc.ListNAVVersions(fund, date)) != 1 {
		t.Fatal("exactly one version may exist")
	}
	cur, _ := svc.CurrentNAV(fund, date)
	if cur.Version != 1 {
		t.Fatal("single current version invariant violated")
	}
}

func TestConcurrentConfirmAndCorrectNoOrphans(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")

	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})

	// 大量确认与一次更正并发。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _ = svc.CorrectNAV(CorrectionInput{
			FundID: fund, ValueDate: date, BasedOn: 1,
			NewUnitNAV: nav(t, "1.1000"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob",
		})
	}()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _ = svc.ConfirmTx(ConfirmTxInput{
				ConfirmID: "C" + itoa(i+1), FundID: fund, ValueDate: date,
				TxType: TxSubscription, Shares: shares(t, "10"),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	cur, err := svc.CurrentNAV(fund, date)
	if err != nil || cur.Version != 2 {
		t.Fatalf("correction must win exactly once, current=%+v err=%v", cur, err)
	}

	confirms := svc.ListConfirmations(fund, date)
	if len(confirms) != n {
		t.Fatalf("all %d confirms recorded, got %d", n, len(confirms))
	}

	onV1, onV2 := 0, 0
	seen := map[string]bool{}
	for _, c := range confirms {
		switch c.NAVVersion {
		case 1:
			onV1++
		case 2:
			onV2++
		default:
			t.Fatalf("confirm on unexpected version: %d", c.NAVVersion)
		}
		seen[c.ConfirmID] = true
	}

	adjs := svc.ListAdjustments(fund, date, AdjustmentFilter{})
	if len(adjs) != onV1 {
		t.Fatalf("adjustments=%d must equal v1 confirms=%d (no orphans, no missing)", len(adjs), onV1)
	}
	adjConfirm := map[string]bool{}
	for _, a := range adjs {
		if a.FromNAVVersion != 1 || a.ToNAVVersion != 2 || !seen[a.ConfirmID] {
			t.Fatalf("adjustment not linked to a real v1 confirmation: %+v", a)
		}
		if adjConfirm[a.ConfirmID] {
			t.Fatalf("duplicate adjustment for %s", a.ConfirmID)
		}
		if a.Amount.String() != "1.00" {
			t.Fatalf("10 shares * 0.1 = 1.00, got %s", a.Amount)
		}
		adjConfirm[a.ConfirmID] = true
	}
	if onV1+onV2 != n || onV1 == 0 || onV2 == 0 {
		t.Fatalf("expected mix of v1/v2 confirms, got %d/%d", onV1, onV2)
	}
}

func TestConcurrentCorrectionsExactlyOneWins(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	wins, conflicts := int32(0), int32(0)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.CorrectNAV(CorrectionInput{
				FundID: fund, ValueDate: date, BasedOn: 1,
				NewUnitNAV: nav(t, "1."+pad2(i+1)+"00"), NewBasis: "b" + itoa(i),
				Reason: "r", CorrectedBy: "bob",
			})
			switch {
			case err == nil:
				atomicAdd(&wins)
			case errors.Is(err, ErrVersionConflict):
				atomicAdd(&conflicts)
			default:
				t.Errorf("unexpected correction error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 || int(wins)+int(conflicts) != n {
		t.Fatalf("want exactly 1 correction, got wins=%d conflicts=%d", wins, conflicts)
	}
	cur, _ := svc.CurrentNAV(fund, date)
	if cur.Version != 2 {
		t.Fatalf("current must be v2, got %d", cur.Version)
	}
	if len(svc.ListAdjustments(fund, date, AdjustmentFilter{})) != 1 {
		t.Fatal("winning correction registers exactly one adjustment")
	}
}

func TestConcurrentSettleExactlyOne(t *testing.T) {
	svc := NewService()
	publish(t, svc, "1.0000", "b")
	svc.ConfirmTx(ConfirmTxInput{ConfirmID: "C1", FundID: fund, ValueDate: date, TxType: TxSubscription, Shares: shares(t, "100")})
	r, _ := svc.CorrectNAV(CorrectionInput{FundID: fund, ValueDate: date, BasedOn: 1,
		NewUnitNAV: nav(t, "1.0200"), NewBasis: "b2", Reason: "r", CorrectedBy: "bob"})
	id := r.Adjustments[0].ID

	var wg sync.WaitGroup
	start := make(chan struct{})
	success := int32(0)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.SettleAdjustment(fund, date, id, "PAY"); err == nil {
				atomicAdd(&success)
			}
		}()
	}
	close(start)
	wg.Wait()
	if success != 1 {
		t.Fatalf("exactly one settlement, got %d", success)
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
