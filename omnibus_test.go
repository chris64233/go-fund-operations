package fundoperations

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var testNavDate = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

func newServiceWithClients(clients ...string) *OmnibusService {
	s := NewOmnibusService()
	for _, c := range clients {
		s.RegisterEligibleClient(c)
	}
	return s
}

func mustSubmit(t *testing.T, s *OmnibusService, total int64) string {
	t.Helper()
	id, err := s.SubmitOrder("INST1", "FUND1", DirectionSubscribe, total, testNavDate, "EXT-1")
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	return id
}

func mustUpsert(t *testing.T, s *OmnibusService, orderID, clientID string, qty int64) {
	t.Helper()
	order, err := s.GetOrder(orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := s.UpsertAllocation(orderID, clientID, qty, order.Version); err != nil {
		t.Fatalf("UpsertAllocation(%s): %v", clientID, err)
	}
}

func mustFreeze(t *testing.T, s *OmnibusService, orderID string) *AllocationSnapshot {
	t.Helper()
	order, _ := s.GetOrder(orderID)
	snap, err := s.Freeze(orderID, order.Version)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snap
}

func TestSubmitOrderRecordsFields(t *testing.T) {
	s := NewOmnibusService()
	id, err := s.SubmitOrder("INST1", "FUND1", DirectionRedeem, 500, testNavDate, "EXT-9")
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	order, err := s.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.InstitutionID != "INST1" || order.FundID != "FUND1" ||
		order.Direction != DirectionRedeem || order.TotalShares != 500 ||
		order.ExternalRef != "EXT-9" || !order.NavDate.Equal(testNavDate) ||
		order.Status != OrderStatusOpen {
		t.Fatalf("unexpected order: %+v", order)
	}
	if _, err := s.SubmitOrder("I", "F", DirectionSubscribe, 0, testNavDate, "X"); !errors.Is(err, ErrInvalidTotal) {
		t.Fatalf("expected ErrInvalidTotal, got %v", err)
	}
}

func TestUpsertSameClientUpdatesWithoutDuplicate(t *testing.T) {
	s := newServiceWithClients("C1")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 60)
	mustUpsert(t, s, id, "C1", 100)
	allocs, err := s.ListAllocations(id)
	if err != nil {
		t.Fatalf("ListAllocations: %v", err)
	}
	if len(allocs) != 1 || allocs[0].Quantity != 100 {
		t.Fatalf("expected single effective allocation of 100, got %+v", allocs)
	}
}

func TestFreezeRejectsGapAndOverage(t *testing.T) {
	s := newServiceWithClients("C1", "C2")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 60)
	order, _ := s.GetOrder(id)
	if _, err := s.Freeze(id, order.Version); !errors.Is(err, ErrAllocationMismatch) {
		t.Fatalf("gap: expected ErrAllocationMismatch, got %v", err)
	}
	mustUpsert(t, s, id, "C2", 50)
	order, _ = s.GetOrder(id)
	if _, err := s.Freeze(id, order.Version); !errors.Is(err, ErrAllocationMismatch) {
		t.Fatalf("overage: expected ErrAllocationMismatch, got %v", err)
	}
	mustUpsert(t, s, id, "C2", 40)
	snap := mustFreeze(t, s, id)
	if len(snap.Allocations) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestFreezeRejectsIneligibleClient(t *testing.T) {
	s := newServiceWithClients("C1")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 100)
	order, _ := s.GetOrder(id)
	if err := s.UpsertAllocation(id, "GHOST", 1, order.Version); !errors.Is(err, ErrIneligibleClient) {
		t.Fatalf("expected ErrIneligibleClient, got %v", err)
	}
}

func TestFreezeSnapshotStablySorted(t *testing.T) {
	s := newServiceWithClients("C3", "C1", "C2")
	id := mustSubmit(t, s, 60)
	mustUpsert(t, s, id, "C3", 10)
	mustUpsert(t, s, id, "C1", 30)
	mustUpsert(t, s, id, "C2", 20)
	snap := mustFreeze(t, s, id)
	want := []Allocation{{"C1", 30}, {"C2", 20}, {"C3", 10}}
	if fmt.Sprintf("%v", snap.Allocations) != fmt.Sprintf("%v", want) {
		t.Fatalf("snapshot not sorted: got %v want %v", snap.Allocations, want)
	}
}

func TestFreezeIdempotentRegardlessOfInsertionOrder(t *testing.T) {
	build := func(order ...string) *AllocationSnapshot {
		s := newServiceWithClients("C1", "C2", "C3")
		id := mustSubmit(t, s, 60)
		qty := map[string]int64{"C1": 30, "C2": 20, "C3": 10}
		for _, c := range order {
			mustUpsert(t, s, id, c, qty[c])
		}
		return mustFreeze(t, s, id)
	}
	a := build("C1", "C2", "C3")
	b := build("C3", "C1", "C2")
	if fmt.Sprintf("%v", a.Allocations) != fmt.Sprintf("%v", b.Allocations) {
		t.Fatalf("snapshots differ by insertion order: %v vs %v", a.Allocations, b.Allocations)
	}
	s := newServiceWithClients("C1")
	id := mustSubmit(t, s, 10)
	mustUpsert(t, s, id, "C1", 10)
	snap := mustFreeze(t, s, id)
	again, err := s.Freeze(id, snap.Version)
	if err != nil || fmt.Sprintf("%v", again.Allocations) != fmt.Sprintf("%v", snap.Allocations) {
		t.Fatalf("repeat freeze not idempotent: %v", err)
	}
}

func TestLateModificationAfterFreezeRejected(t *testing.T) {
	s := newServiceWithClients("C1", "C2")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 100)
	snap := mustFreeze(t, s, id)
	order, _ := s.GetOrder(id)
	if err := s.UpsertAllocation(id, "C2", 1, order.Version); !errors.Is(err, ErrOrderNotOpen) {
		t.Fatalf("expected ErrOrderNotOpen, got %v", err)
	}
	if err := s.RemoveAllocation(id, "C1", order.Version); !errors.Is(err, ErrOrderNotOpen) {
		t.Fatalf("expected ErrOrderNotOpen, got %v", err)
	}
	order, _ = s.GetOrder(id)
	positions, err := s.Confirm(id, order.Version)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if len(positions) != 1 || positions[0].ClientID != "C1" || positions[0].Shares != 100 {
		t.Fatalf("unexpected positions: %+v", positions)
	}
	got, _ := s.GetSnapshot(id)
	if fmt.Sprintf("%v", got.Allocations) != fmt.Sprintf("%v", snap.Allocations) {
		t.Fatalf("snapshot changed after confirm")
	}
}

func TestVersionConflictOnConcurrentModification(t *testing.T) {
	s := newServiceWithClients("C1", "C2")
	id := mustSubmit(t, s, 100)
	order, _ := s.GetOrder(id)
	if err := s.UpsertAllocation(id, "C1", 100, order.Version); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.UpsertAllocation(id, "C2", 1, order.Version); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	order, _ = s.GetOrder(id)
	if _, err := s.Freeze(id, order.Version+1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("freeze: expected ErrVersionConflict, got %v", err)
	}
}

func TestConcurrentUpsertsExactlyOneWins(t *testing.T) {
	s := newServiceWithClients("C1", "C2")
	id := mustSubmit(t, s, 100)
	order, _ := s.GetOrder(id)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	clients := []string{"C1", "C2"}
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			errs[i] = s.UpsertAllocation(id, c, 100, order.Version)
		}(i, c)
	}
	wg.Wait()
	var successes int
	for _, err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one winner, got %d", successes)
	}
}

func TestFailedOrderKeepsAllocationsButNoPositions(t *testing.T) {
	s := newServiceWithClients("C1")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 100)
	mustFreeze(t, s, id)
	order, _ := s.GetOrder(id)
	if err := s.Fail(id, order.Version); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	allocs, err := s.ListAllocations(id)
	if err != nil || len(allocs) != 1 || allocs[0].Quantity != 100 {
		t.Fatalf("allocations should remain queryable: %+v err=%v", allocs, err)
	}
	if got := s.Positions(id); len(got) != 0 {
		t.Fatalf("failed order must not generate positions: %+v", got)
	}
	order, _ = s.GetOrder(id)
	if _, err := s.Confirm(id, order.Version); !errors.Is(err, ErrOrderNotFrozen) {
		t.Fatalf("confirm after fail: expected ErrOrderNotFrozen, got %v", err)
	}
}

func TestConfirmIdempotent(t *testing.T) {
	s := newServiceWithClients("C1")
	id := mustSubmit(t, s, 50)
	mustUpsert(t, s, id, "C1", 50)
	mustFreeze(t, s, id)
	order, _ := s.GetOrder(id)
	p1, err := s.Confirm(id, order.Version)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	order, _ = s.GetOrder(id)
	p2, err := s.Confirm(id, order.Version)
	if err != nil {
		t.Fatalf("repeat Confirm: %v", err)
	}
	if fmt.Sprintf("%v", p1) != fmt.Sprintf("%v", p2) {
		t.Fatalf("confirm not idempotent: %v vs %v", p1, p2)
	}
}

func TestGapReport(t *testing.T) {
	s := newServiceWithClients("C1", "C2")
	id := mustSubmit(t, s, 100)
	mustUpsert(t, s, id, "C1", 40)
	report, err := s.GapReport(id)
	if err != nil {
		t.Fatalf("GapReport: %v", err)
	}
	if report.OrderTotal != 100 || report.Allocated != 40 || report.Gap != 60 || report.ClientCount != 1 {
		t.Fatalf("unexpected gap report: %+v", report)
	}
	mustUpsert(t, s, id, "C2", 70)
	report, _ = s.GapReport(id)
	if report.Gap != -10 {
		t.Fatalf("expected overage gap -10, got %+v", report)
	}
}

func TestRedeemOrderUsesShares(t *testing.T) {
	s := newServiceWithClients("C1")
	id, err := s.SubmitOrder("INST1", "FUND1", DirectionRedeem, 200, testNavDate, "EXT-R")
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	mustUpsert(t, s, id, "C1", 200)
	snap := mustFreeze(t, s, id)
	if snap.Allocations[0].Quantity != 200 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}
