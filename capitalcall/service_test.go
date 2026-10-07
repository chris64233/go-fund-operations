package capitalcall

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustCommit(t *testing.T, s *Service, investor, currency string, total Money) *Commitment {
	t.Helper()
	c, err := s.RegisterCommitment(investor, currency, total)
	if err != nil {
		t.Fatalf("register commitment: %v", err)
	}
	return c
}

func mustNotice(t *testing.T, s *Service, target Money) (*Notice, []Allocation) {
	t.Helper()
	n, allocs, err := s.CreateNotice("proj", "USD", target, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("create notice: %v", err)
	}
	return n, allocs
}

func sumAllocs(allocs []Allocation) Money {
	var sum Money
	for _, a := range allocs {
		sum += a.Amount
	}
	return sum
}

// 比例舍入：分配之和必须等于目标总额，且单项不超过剩余额度。
func TestProportionalRounding(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 100)
	mustCommit(t, s, "B", "USD", 100)
	mustCommit(t, s, "C", "USD", 100)

	n, allocs := mustNotice(t, s, 100) // 每人 33.33…，舍入后须凑齐 100
	if got := sumAllocs(allocs); got != n.Target {
		t.Fatalf("sum = %d, want %d", got, n.Target)
	}
	for _, a := range allocs {
		if a.Amount > 100 {
			t.Fatalf("allocation %v exceeds remaining", a)
		}
	}
	// 33/33/34 的某种排列
	counts := map[Money]int{}
	for _, a := range allocs {
		counts[a.Amount]++
	}
	if counts[33] != 2 || counts[34] != 1 {
		t.Fatalf("unexpected split: %v", counts)
	}
}

// 额度不足时整份通知失败，且不得冻结任何额度。
func TestNoticeFailsWhenInsufficient(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 100)
	mustCommit(t, s, "B", "USD", 50)

	if _, _, err := s.CreateNotice("proj", "USD", 200, time.Now()); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
	pos, _ := s.Position("A")
	if pos.Uncalled != 100 || pos.Called != 0 {
		t.Fatalf("failed notice must not freeze quota: %+v", pos)
	}
}

// 部分缴款：分次确认，累计不得超过应缴。
func TestPartialPayments(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	_, allocs := mustNotice(t, s, 500)
	a := allocs[0]

	if _, err := s.ConfirmPayment(a.ID, "ref-1", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmPayment(a.ID, "ref-2", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmPayment(a.ID, "ref-3", 200); !errors.Is(err, ErrOverpay) {
		t.Fatalf("want ErrOverpay, got %v", err)
	}
	if _, err := s.ConfirmPayment(a.ID, "ref-3", 100); err != nil {
		t.Fatal(err)
	}
	pos, _ := s.Position("A")
	if pos.Paid != 500 || pos.Notified != 0 || pos.Uncalled != 500 {
		t.Fatalf("bad position: %+v", pos)
	}
}

// 幂等：重复银行流水不重复增加实缴。
func TestIdempotentPayment(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	_, allocs := mustNotice(t, s, 500)
	a := allocs[0]

	p1, err := s.ConfirmPayment(a.ID, "bank-ref-x", 300)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.ConfirmPayment(a.ID, "bank-ref-x", 300)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("duplicate bank ref created new payment: %s vs %s", p1.ID, p2.ID)
	}
	pos, _ := s.Position("A")
	if pos.Paid != 300 {
		t.Fatalf("paid = %d, want 300", pos.Paid)
	}
}

// 并发占用：两份通知并发生成，总额不得超过承诺额度。
func TestConcurrentNoticesNoOvercommit(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	mustCommit(t, s, "B", "USD", 1000)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded Money
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.CreateNotice("p", "USD", 500, time.Now()); err == nil {
				mu.Lock()
				succeeded += 500
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if succeeded > 2000 {
		t.Fatalf("over-committed: %d > 2000", succeeded)
	}
	posA, _ := s.Position("A")
	posB, _ := s.Position("B")
	if posA.Called > 1000 || posB.Called > 1000 {
		t.Fatalf("called exceeds total: A=%d B=%d", posA.Called, posB.Called)
	}
	if posA.Called+posB.Called != succeeded {
		t.Fatalf("called %d != succeeded %d", posA.Called+posB.Called, succeeded)
	}
}

// 取消竞态：取消与缴款并发，结果必须一致——要么缴款成功且通知未释放
// 已缴部分，要么取消成功且未缴额度全部释放。
func TestCancelRaceConsistency(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewService()
		mustCommit(t, s, "A", "USD", 1000)
		n, allocs := mustNotice(t, s, 600)
		a := allocs[0]

		var wg sync.WaitGroup
		var payErr, cancelErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, payErr = s.ConfirmPayment(a.ID, "ref", 600) }()
		go func() { defer wg.Done(); cancelErr = s.CancelNotice(n.ID) }()
		wg.Wait()

		pos, _ := s.Position("A")
		switch {
		case payErr == nil && cancelErr != nil:
			if pos.Paid != 600 || pos.Called != 600 || pos.Uncalled != 400 {
				t.Fatalf("pay-won inconsistent: %+v", pos)
			}
		case payErr != nil && cancelErr == nil:
			if pos.Paid != 0 || pos.Called != 0 || pos.Uncalled != 1000 {
				t.Fatalf("cancel-won inconsistent: %+v", pos)
			}
		default:
			t.Fatalf("both succeeded or both failed: pay=%v cancel=%v", payErr, cancelErr)
		}
	}
}

// 取消后部分已缴：已缴保持已调用，未缴释放。
func TestCancelReleasesOnlyUnpaid(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	n, allocs := mustNotice(t, s, 600)
	if _, err := s.ConfirmPayment(allocs[0].ID, "ref", 200); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelNotice(n.ID); err != nil {
		t.Fatal(err)
	}
	pos, _ := s.Position("A")
	if pos.Paid != 200 || pos.Called != 200 || pos.Uncalled != 800 || pos.Notified != 0 {
		t.Fatalf("bad position after cancel: %+v", pos)
	}
	// 取消后不允许再缴款
	if _, err := s.ConfirmPayment(allocs[0].ID, "ref2", 100); !errors.Is(err, ErrNoticeNotOpen) {
		t.Fatalf("want ErrNoticeNotOpen, got %v", err)
	}
}

// 逾期查询。
func TestOverdueNotices(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	past := time.Now().Add(-time.Hour)
	if _, _, err := s.CreateNotice("late", "USD", 100, past); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateNotice("future", "USD", 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	overdue := s.OverdueNotices(time.Now())
	if len(overdue) != 1 || overdue[0].Purpose != "late" {
		t.Fatalf("overdue = %v", overdue)
	}
}

// 无效承诺不参与分配；多投资者按比例分配。
func TestInactiveCommitmentExcluded(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 300)
	c2 := mustCommit(t, s, "B", "USD", 100)
	if err := s.DeactivateCommitment(c2.ID); err != nil {
		t.Fatal(err)
	}
	_, allocs := mustNotice(t, s, 150)
	if len(allocs) != 1 || allocs[0].Investor != "A" || allocs[0].Amount != 150 {
		t.Fatalf("allocs = %v", allocs)
	}
}

// 并发缴款同一分配，总额不得超过应缴。
func TestConcurrentPaymentsNoOverpay(t *testing.T) {
	s := NewService()
	mustCommit(t, s, "A", "USD", 1000)
	_, allocs := mustNotice(t, s, 500)
	a := allocs[0]

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.ConfirmPayment(a.ID, fmt.Sprintf("ref-%d", i), 100)
		}(i)
	}
	wg.Wait()
	pos, _ := s.Position("A")
	if pos.Paid > 500 {
		t.Fatalf("overpaid: %d", pos.Paid)
	}
}
