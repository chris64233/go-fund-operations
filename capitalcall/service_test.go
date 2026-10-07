package capitalcall

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newServiceWithCommitments(t *testing.T, totals ...int64) *Service {
	t.Helper()
	s := NewService()
	for i, total := range totals {
		id := fmt.Sprintf("c%d", i)
		if err := s.RegisterCommitment(id, fmt.Sprintf("inv%d", i), "USD", total); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return s
}

func TestGenerateNoticeProportionalRounding(t *testing.T) {
	// Uncalled 100 each, target 100: exact shares 33.33/33.33/33.33
	// must round to a distribution summing to exactly 100.
	s := newServiceWithCommitments(t, 100, 100, 100)
	n, err := s.GenerateNotice("n1", "project A", "USD", 100, testNow.Add(24*time.Hour), testNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var sum int64
	for _, a := range n.Allocations {
		sum += a.Amount
		if a.Amount > 100 {
			t.Fatalf("allocation %d exceeds uncalled 100", a.Amount)
		}
	}
	if sum != 100 {
		t.Fatalf("allocations sum %d, want 100", sum)
	}
	// Largest remainder ties are broken deterministically; each share
	// must be 33 or 34.
	for _, a := range n.Allocations {
		if a.Amount < 33 || a.Amount > 34 {
			t.Fatalf("allocation %d out of expected range", a.Amount)
		}
	}
}

func TestGenerateNoticeProRataToUncalled(t *testing.T) {
	s := newServiceWithCommitments(t, 300, 100)
	n, err := s.GenerateNotice("n1", "project A", "USD", 200, testNow.Add(24*time.Hour), testNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if n.Allocations[0].Amount != 150 || n.Allocations[1].Amount != 50 {
		t.Fatalf("allocations %+v, want 150/50", n.Allocations)
	}
	pos := s.Position("inv0", "USD")
	if pos.Uncalled != 150 || pos.NotifiedUnpaid != 150 || pos.Paid != 0 {
		t.Fatalf("position %+v", pos)
	}
}

func TestGenerateNoticeInsufficientFailsAtomically(t *testing.T) {
	s := newServiceWithCommitments(t, 100, 50)
	if _, err := s.GenerateNotice("n1", "p", "USD", 200, testNow, testNow); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
	// Nothing reserved: full amounts still uncalled.
	if pos := s.Position("inv0", "USD"); pos.Uncalled != 100 || pos.NotifiedUnpaid != 0 {
		t.Fatalf("position %+v after failed notice", pos)
	}
	if pos := s.Position("inv1", "USD"); pos.Uncalled != 50 {
		t.Fatalf("position %+v after failed notice", pos)
	}
}

func TestGenerateNoticeFreezesUncalled(t *testing.T) {
	s := newServiceWithCommitments(t, 100)
	if _, err := s.GenerateNotice("n1", "p", "USD", 80, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Only 20 left uncalled; a second notice for 30 must fail.
	if _, err := s.GenerateNotice("n2", "p", "USD", 30, testNow, testNow); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
	if _, err := s.GenerateNotice("n3", "p", "USD", 20, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
}

func TestConcurrentNoticesNoDoubleReservation(t *testing.T) {
	const total = 1000
	s := newServiceWithCommitments(t, total)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int
	var calledSum int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := s.GenerateNotice(fmt.Sprintf("n%d", i), "p", "USD", 100, testNow, testNow)
			if err == nil {
				mu.Lock()
				succeeded++
				calledSum += n.TargetAmount
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want 10", succeeded)
	}
	if pos := s.Position("inv0", "USD"); pos.NotifiedUnpaid != total || pos.Uncalled != 0 {
		t.Fatalf("position %+v, want fully called", pos)
	}
}

func TestPartialPaymentsAndOverpayment(t *testing.T) {
	s := newServiceWithCommitments(t, 100)
	if _, err := s.GenerateNotice("n1", "p", "USD", 60, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := s.ConfirmPayment("p1", "n1", "inv0", "ref-1", 20, testNow); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if _, err := s.ConfirmPayment("p2", "n1", "inv0", "ref-2", 30, testNow); err != nil {
		t.Fatalf("pay: %v", err)
	}
	// 50 of 60 paid; 20 more must fail and change nothing.
	if _, err := s.ConfirmPayment("p3", "n1", "inv0", "ref-3", 20, testNow); !errors.Is(err, ErrOverpayment) {
		t.Fatalf("err = %v, want ErrOverpayment", err)
	}
	alloc, err := s.GetAllocation("n1", "inv0")
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if alloc.Amount != 60 || alloc.PaidAmount != 50 {
		t.Fatalf("allocation %+v", alloc)
	}
	pos := s.Position("inv0", "USD")
	if pos.Paid != 50 || pos.NotifiedUnpaid != 10 || pos.Uncalled != 40 {
		t.Fatalf("position %+v", pos)
	}
}

func TestDuplicateBankReferenceIsIdempotent(t *testing.T) {
	s := newServiceWithCommitments(t, 100)
	if _, err := s.GenerateNotice("n1", "p", "USD", 60, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	p1, err := s.ConfirmPayment("p1", "n1", "inv0", "ref-1", 25, testNow)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	// Same bank reference, different payment ID: must not double count.
	p2, err := s.ConfirmPayment("p2", "n1", "inv0", "ref-1", 25, testNow)
	if err != nil {
		t.Fatalf("duplicate pay: %v", err)
	}
	if p2.ID != p1.ID {
		t.Fatalf("duplicate returned payment %q, want original %q", p2.ID, p1.ID)
	}
	alloc, _ := s.GetAllocation("n1", "inv0")
	if alloc.PaidAmount != 25 {
		t.Fatalf("paid %d, want 25", alloc.PaidAmount)
	}
	if pos := s.Position("inv0", "USD"); pos.Paid != 25 {
		t.Fatalf("position %+v", pos)
	}
}

func TestCancelNoticeReleasesUnpaid(t *testing.T) {
	s := newServiceWithCommitments(t, 100)
	if _, err := s.GenerateNotice("n1", "p", "USD", 60, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := s.ConfirmPayment("p1", "n1", "inv0", "ref-1", 20, testNow); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if err := s.CancelNotice("n1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Paid 20 stays called; unpaid 40 is released.
	pos := s.Position("inv0", "USD")
	if pos.Uncalled != 80 || pos.NotifiedUnpaid != 0 || pos.Paid != 20 {
		t.Fatalf("position %+v", pos)
	}
	// Cancelled notice rejects further payments and re-cancellation.
	if _, err := s.ConfirmPayment("p2", "n1", "inv0", "ref-2", 10, testNow); !errors.Is(err, ErrNoticeNotActive) {
		t.Fatalf("err = %v, want ErrNoticeNotActive", err)
	}
	if err := s.CancelNotice("n1"); !errors.Is(err, ErrNoticeNotActive) {
		t.Fatalf("err = %v, want ErrNoticeNotActive", err)
	}
}

func TestCancelPaymentRaceConsistency(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		s := newServiceWithCommitments(t, 100)
		if _, err := s.GenerateNotice("n1", "p", "USD", 60, testNow, testNow); err != nil {
			t.Fatalf("generate: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.CancelNotice("n1")
		}()
		go func() {
			defer wg.Done()
			s.ConfirmPayment("p1", "n1", "inv0", "ref-1", 60, testNow)
		}()
		wg.Wait()
		// Whichever ran first, the books must balance:
		// uncalled + notifiedUnpaid + paid == total.
		pos := s.Position("inv0", "USD")
		if pos.Uncalled+pos.NotifiedUnpaid+pos.Paid != pos.Total {
			t.Fatalf("inconsistent position %+v", pos)
		}
		if pos.Paid != 0 && pos.Paid != 60 {
			t.Fatalf("paid %d, want 0 or 60", pos.Paid)
		}
		// If payment won, cancel released nothing extra.
		if pos.Paid == 60 && pos.NotifiedUnpaid != 0 {
			t.Fatalf("paid but still notified-unpaid: %+v", pos)
		}
	}
}

func TestConcurrentPaymentsNoOverpayment(t *testing.T) {
	s := newServiceWithCommitments(t, 100)
	if _, err := s.GenerateNotice("n1", "p", "USD", 50, testNow, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.ConfirmPayment(fmt.Sprintf("p%d", i), "n1", "inv0", fmt.Sprintf("ref-%d", i), 20, testNow)
		}(i)
	}
	wg.Wait()
	alloc, _ := s.GetAllocation("n1", "inv0")
	if alloc.PaidAmount > alloc.Amount {
		t.Fatalf("paid %d exceeds allocation %d", alloc.PaidAmount, alloc.Amount)
	}
	if alloc.PaidAmount != 40 {
		t.Fatalf("paid %d, want exactly 40 (two 20s accepted, rest rejected)", alloc.PaidAmount)
	}
}

func TestOverdueNotices(t *testing.T) {
	s := newServiceWithCommitments(t, 100, 100)
	due := testNow.Add(24 * time.Hour)
	if _, err := s.GenerateNotice("n1", "p", "USD", 50, due, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := s.GenerateNotice("n2", "p", "USD", 50, due, testNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Fully pay n1's allocations.
	n1, _ := s.GetNotice("n1")
	for i, a := range n1.Allocations {
		if _, err := s.ConfirmPayment(fmt.Sprintf("p%d", i), "n1", a.InvestorID, fmt.Sprintf("ref-%d", i), a.Amount, testNow); err != nil {
			t.Fatalf("pay: %v", err)
		}
	}
	overdue := s.OverdueNotices(due.Add(time.Second))
	if len(overdue) != 1 || overdue[0].ID != "n2" {
		t.Fatalf("overdue %+v, want only n2", overdue)
	}
	// Before the due date nothing is overdue.
	if got := s.OverdueNotices(testNow); len(got) != 0 {
		t.Fatalf("overdue before due date: %+v", got)
	}
}

func TestInactiveCommitmentExcluded(t *testing.T) {
	s := newServiceWithCommitments(t, 100, 100)
	if err := s.SetCommitmentActive("c1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	n, err := s.GenerateNotice("n1", "p", "USD", 100, testNow, testNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(n.Allocations) != 1 || n.Allocations[0].InvestorID != "inv0" {
		t.Fatalf("allocations %+v, want only inv0", n.Allocations)
	}
}

func TestMultipleCommitmentsPerInvestor(t *testing.T) {
	s := NewService()
	if err := s.RegisterCommitment("c1", "inv0", "USD", 60); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterCommitment("c2", "inv0", "USD", 40); err != nil {
		t.Fatal(err)
	}
	n, err := s.GenerateNotice("n1", "p", "USD", 50, testNow, testNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var sum int64
	for _, a := range n.Allocations {
		sum += a.Amount
	}
	if sum != 50 {
		t.Fatalf("sum %d, want 50", sum)
	}
	if _, err := s.ConfirmPayment("p1", "n1", "inv0", "ref-1", 50, testNow); err != nil {
		t.Fatalf("pay: %v", err)
	}
	pos := s.Position("inv0", "USD")
	if pos.Paid != 50 || pos.Uncalled != 50 || pos.NotifiedUnpaid != 0 {
		t.Fatalf("position %+v", pos)
	}
}
