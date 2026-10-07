package omnibus

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func newOrder(t *testing.T, s *Service, total int64) *Order {
	t.Helper()
	o, err := s.CreateOrder("INST1", "FUND1", DirectionSubscribe, total, "2026-10-08", "EXT-1")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return o
}

func mustUpsert(t *testing.T, s *Service, orderID, customer string, qty, ver int64) {
	t.Helper()
	if err := s.UpsertAllocation(orderID, customer, qty, ver); err != nil {
		t.Fatalf("UpsertAllocation(%s): %v", customer, err)
	}
}

func TestFreezeRequiresExactMatch_Gap(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 1000)
	mustUpsert(t, s, o.ID, "C1", 600, o.Version)

	if _, err := s.Freeze(o.ID, o.Version+1); !errors.Is(err, ErrAllocationMismatch) {
		t.Fatalf("expected mismatch for gap, got %v", err)
	}
	_, _, gap, err := s.Gap(o.ID)
	if err != nil || gap != 400 {
		t.Fatalf("gap = %d, want 400 (err %v)", gap, err)
	}
	// 缺口补齐后同一版本裁决：版本已推进，需要最新版本
	cur, _ := s.GetOrder(o.ID)
	mustUpsert(t, s, o.ID, "C2", 400, cur.Version)
	cur, _ = s.GetOrder(o.ID)
	rec, err := s.Freeze(o.ID, cur.Version)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if rec.Allocated != 1000 || len(rec.Snapshot) != 2 {
		t.Fatalf("unexpected freeze record %+v", rec)
	}
}

func TestFreezeRejectsOverAllocation(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 1000)
	mustUpsert(t, s, o.ID, "C1", 700, o.Version)
	mustUpsert(t, s, o.ID, "C2", 700, o.Version+1)

	if _, err := s.Freeze(o.ID, o.Version+2); !errors.Is(err, ErrAllocationMismatch) {
		t.Fatalf("expected mismatch for over-allocation, got %v", err)
	}
	_, _, gap, _ := s.Gap(o.ID)
	if gap != -400 {
		t.Fatalf("gap = %d, want -400", gap)
	}
}

func TestNoVirtualCustomerForRemainder(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 1000)
	mustUpsert(t, s, o.ID, "C1", 1000, o.Version)
	// 空客户号、非正数量均为不合格客户，不能用于补齐差额
	if err := s.UpsertAllocation(o.ID, "", 1, o.Version+1); !errors.Is(err, ErrInvalidCustomer) {
		t.Fatalf("expected invalid customer, got %v", err)
	}
	if err := s.UpsertAllocation(o.ID, "VIRTUAL", 0, o.Version+1); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("expected invalid quantity, got %v", err)
	}
}

func TestDuplicateCustomerNoDoubleCount(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 500)
	mustUpsert(t, s, o.ID, "C1", 300, o.Version)
	// 同一客户修改未冻结明细：覆盖而非累加
	mustUpsert(t, s, o.ID, "C1", 500, o.Version+1)
	// 相同内容重复提交幂等，不推进版本
	if err := s.UpsertAllocation(o.ID, "C1", 500, o.Version+2); err != nil {
		t.Fatalf("idempotent upsert: %v", err)
	}
	cur, _ := s.GetOrder(o.ID)
	if cur.Version != o.Version+2 {
		t.Fatalf("idempotent upsert bumped version to %d", cur.Version)
	}
	rec, err := s.Freeze(o.ID, cur.Version)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if len(rec.Snapshot) != 1 || rec.Snapshot[0].Quantity != 500 {
		t.Fatalf("duplicate effective allocation: %+v", rec.Snapshot)
	}
}

func TestFreezeSnapshotStableOrderingAndOrderIndependent(t *testing.T) {
	build := func(order []string) *FreezeRecord {
		s := NewService()
		o, err := s.CreateOrder("INST1", "FUND1", DirectionSubscribe, 600, "2026-10-08", "EXT-X")
		if err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}
		ver := o.Version
		for _, c := range order {
			mustUpsert(t, s, o.ID, c, 200, ver)
			ver++
		}
		rec, err := s.Freeze(o.ID, ver)
		if err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		return rec
	}
	r1 := build([]string{"C3", "C1", "C2"})
	r2 := build([]string{"C1", "C2", "C3"})
	r3 := build([]string{"C2", "C3", "C1"})
	for i, want := range []string{"C1", "C2", "C3"} {
		if r1.Snapshot[i].CustomerID != want ||
			r2.Snapshot[i].CustomerID != want ||
			r3.Snapshot[i].CustomerID != want {
			t.Fatalf("snapshot not stably sorted: %v %v %v",
				r1.Snapshot, r2.Snapshot, r3.Snapshot)
		}
	}
}

func TestConcurrentModificationArbitratedByVersion(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 1000)
	mustUpsert(t, s, o.ID, "C1", 1000, o.Version)
	cur, _ := s.GetOrder(o.ID)

	// 并发：一个 goroutine 冻结，其余尝试用同一旧版本修改明细
	const writers = 8
	var wg sync.WaitGroup
	freezeOK := make(chan bool, 1)
	modResults := make(chan error, writers)
	wg.Add(writers + 1)
	go func() {
		defer wg.Done()
		_, err := s.Freeze(o.ID, cur.Version)
		freezeOK <- err == nil
	}()
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			modResults <- s.UpsertAllocation(o.ID, fmt.Sprintf("LATE%d", i), 1, cur.Version)
		}(i)
	}
	wg.Wait()
	frozen := <-freezeOK
	var conflicts, notOpen, won int
	for i := 0; i < writers; i++ {
		err := <-modResults
		switch {
		case err == nil:
			won++ // 修改先于冻结获胜，随后冻结应因版本冲突失败
		case errors.Is(err, ErrVersionConflict):
			conflicts++
		case errors.Is(err, ErrOrderNotOpen):
			notOpen++
		default:
			t.Fatalf("unexpected modification result: %v", err)
		}
	}
	if frozen && notOpen == 0 {
		t.Fatalf("freeze won but no late modification was rejected")
	}
	if !frozen && won == 0 {
		t.Fatalf("freeze lost but no modification won")
	}
	// 无论谁赢，最终状态必须自洽：要么已冻结且总额一致，要么仍可冻结
	final, _ := s.GetOrder(o.ID)
	if final.Status == StatusFrozen {
		rec, _ := s.FreezeRecordOf(o.ID)
		if rec.Allocated != rec.Total {
			t.Fatalf("frozen with mismatch: %+v", rec)
		}
	}
}

func TestLateModificationAfterFreezeCannotChangeHoldings(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 1000)
	mustUpsert(t, s, o.ID, "C1", 400, o.Version)
	mustUpsert(t, s, o.ID, "C2", 600, o.Version+1)
	rec, err := s.Freeze(o.ID, o.Version+2)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := s.Confirm(o.ID, rec.Version, true); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	// 冻结后迟到修改被拒绝
	if err := s.UpsertAllocation(o.ID, "C1", 999, rec.Version); !errors.Is(err, ErrOrderNotOpen) {
		t.Fatalf("late modification accepted: %v", err)
	}
	h1, _ := s.Holding(o.ID, "C1")
	h2, _ := s.Holding(o.ID, "C2")
	if h1 != 400 || h2 != 600 {
		t.Fatalf("holdings changed after freeze: %d %d", h1, h2)
	}
}

func TestFailedOrderKeepsAllocationsButNoHoldings(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 800)
	mustUpsert(t, s, o.ID, "C1", 800, o.Version)
	rec, err := s.Freeze(o.ID, o.Version+1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := s.Confirm(o.ID, rec.Version, false); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	final, _ := s.GetOrder(o.ID)
	if final.Status != StatusFailed {
		t.Fatalf("status = %s, want FAILED", final.Status)
	}
	allocs, err := s.Allocations(o.ID)
	if err != nil || len(allocs) != 1 || allocs[0].Quantity != 800 {
		t.Fatalf("allocations lost after failure: %v %v", allocs, err)
	}
	h, _ := s.Holding(o.ID, "C1")
	if h != 0 {
		t.Fatalf("failed order generated holding %d", h)
	}
}

func TestCreateOrderIdempotentByExternalRef(t *testing.T) {
	s := NewService()
	o1 := newOrder(t, s, 1000)
	o2, err := s.CreateOrder("INST1", "FUND1", DirectionSubscribe, 1000, "2026-10-08", "EXT-1")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if o1.ID != o2.ID {
		t.Fatalf("duplicate external ref created new order %s", o2.ID)
	}
}

func TestRedeemOrderUsesShares(t *testing.T) {
	s := NewService()
	o, err := s.CreateOrder("INST1", "FUND1", DirectionRedeem, 250, "2026-10-08", "EXT-R")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if o.TotalShares != 250 || o.TotalAmount != 0 {
		t.Fatalf("redeem order totals wrong: %+v", o)
	}
	mustUpsert(t, s, o.ID, "C1", 250, o.Version)
	if _, err := s.Freeze(o.ID, o.Version+1); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
}

func TestVersionConflictOnStaleWrite(t *testing.T) {
	s := NewService()
	o := newOrder(t, s, 100)
	mustUpsert(t, s, o.ID, "C1", 100, o.Version)
	if err := s.UpsertAllocation(o.ID, "C2", 1, o.Version); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version write accepted: %v", err)
	}
}
