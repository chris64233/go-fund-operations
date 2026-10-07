package liquidation

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// AccountStatus is the investor account state used at payment time.
type AccountStatus string

const (
	AccountActive AccountStatus = "ACTIVE"
	AccountExited AccountStatus = "EXITED"
	AccountFrozen AccountStatus = "FROZEN"
)

// AccountLookup resolves the current account status of an investor.
type AccountLookup func(investorID string) AccountStatus

// Clock supplies the current time; injectable for tests.
type Clock func() time.Time

// Service manages liquidation plans. All state transitions are
// serialized by a single mutex so concurrent cancel / payment /
// reserve-release operations produce one unique outcome.
type Service struct {
	mu       sync.Mutex
	plans    map[string]*Plan
	accounts AccountLookup
	now      Clock
	seq      int
}

// NewService creates a Service. accounts may be nil, meaning all
// investors are payable.
func NewService(accounts AccountLookup, now Clock) *Service {
	if now == nil {
		now = time.Now
	}
	if accounts == nil {
		accounts = func(string) AccountStatus { return AccountActive }
	}
	return &Service{plans: make(map[string]*Plan), accounts: accounts, now: now}
}

// CreatePlan creates a draft plan with a frozen snapshot taken at the
// record date. Holdings that change after the record date must not be
// included; callers pass the snapshot as of the record date.
func (s *Service) CreatePlan(id, fundID string, recordDate time.Time, totalAssets, feeReserve, disputeReserve Money, snapshot []SnapshotEntry) (*Plan, error) {
	if totalAssets <= 0 || feeReserve < 0 || disputeReserve < 0 {
		return nil, ErrInvalidAmount
	}
	if feeReserve+disputeReserve > totalAssets {
		return nil, ErrInvalidAmount
	}
	if len(snapshot) == 0 {
		return nil, ErrEmptySnapshot
	}
	var totalShares Shares
	seen := make(map[string]bool)
	for _, e := range snapshot {
		if e.Shares < 0 || seen[e.InvestorID] {
			return nil, ErrInvalidAmount
		}
		seen[e.InvestorID] = true
		totalShares += e.Shares
	}
	if totalShares <= 0 {
		return nil, ErrEmptySnapshot
	}
	snap := make([]SnapshotEntry, len(snapshot))
	copy(snap, snapshot)
	sort.Slice(snap, func(i, j int) bool { return snap[i].InvestorID < snap[j].InvestorID })

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[id]; ok {
		return nil, fmt.Errorf("liquidation: plan %s already exists", id)
	}
	p := &Plan{
		ID:             id,
		FundID:         fundID,
		RecordDate:     recordDate,
		TotalAssets:    totalAssets,
		FeeReserve:     feeReserve,
		DisputeReserve: disputeReserve,
		FeeRemaining:   feeReserve,
		DisputeRemain:  disputeReserve,
		Snapshot:       snap,
		TotalShares:    totalShares,
		Status:         PlanDraft,
		CreatedAt:      s.now(),
	}
	s.plans[id] = p
	return p, nil
}

// Confirm confirms the plan: the snapshot is frozen and new share
// trades for the fund are halted from this point on.
func (s *Service) Confirm(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return err
	}
	if p.Status != PlanDraft {
		return ErrPlanNotDraft
	}
	now := s.now()
	p.Status = PlanConfirmed
	p.ConfirmedAt = &now
	return nil
}

// CheckTradeAllowed reports whether a share trade may proceed. After
// confirmation, trading is halted; after the record date, holding
// changes cannot enter this liquidation anyway.
func (s *Service) CheckTradeAllowed(planID string, tradeDate time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return err
	}
	if p.Status == PlanConfirmed || p.Status == PlanCompleted {
		return ErrTradingHalted
	}
	if !tradeDate.Before(p.RecordDate) {
		return ErrRecordDatePassed
	}
	return nil
}

// Cancel cancels a plan that has not completed. Concurrent with
// payment or reserve release, the mutex guarantees a unique result:
// whichever operation wins the lock applies, the loser gets an error.
func (s *Service) Cancel(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return err
	}
	switch p.Status {
	case PlanCancelled:
		return ErrPlanCancelled
	case PlanCompleted:
		return ErrPlanCompleted
	}
	// 已有付款的方案不可取消：保证取消与付款并发时结果唯一。
	for _, b := range p.Batches {
		for _, it := range b.Items {
			if it.Status == ItemPaid {
				return fmt.Errorf("liquidation: plan %s has paid items, cannot cancel", p.ID)
			}
		}
	}
	p.Status = PlanCancelled
	for _, b := range p.Batches {
		if b.Status == BatchOpen || b.Status == BatchPartial {
			b.Status = BatchCancelled
		}
	}
	return nil
}

// CreateInitialBatch builds the first distribution batch from the
// frozen snapshot. Idempotent: calling it again returns the existing
// initial batch instead of duplicating payments.
func (s *Service) CreateInitialBatch(planID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	if err := checkExecutable(p); err != nil {
		return nil, err
	}
	for _, b := range p.Batches {
		if b.Source == SourceInitial {
			return b, nil
		}
	}
	return s.newBatchLocked(p, SourceInitial, p.Distributable()), nil
}

// ReleaseReserve releases part of a reserve (fee or dispute) and
// forms a supplementary distribution batch from the released amount,
// still allocated by the original frozen shares.
func (s *Service) ReleaseReserve(planID string, dispute bool, amount Money) (*Batch, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	if err := checkExecutable(p); err != nil {
		return nil, err
	}
	if dispute {
		if amount > p.DisputeRemain {
			return nil, ErrReserveExceed
		}
		p.DisputeRemain -= amount
	} else {
		if amount > p.FeeRemaining {
			return nil, ErrReserveExceed
		}
		p.FeeRemaining -= amount
	}
	return s.newBatchLocked(p, SourceReserveRelease, amount), nil
}

func (s *Service) newBatchLocked(p *Plan, src BatchSource, total Money) *Batch {
	amounts, dust := allocate(total, p.Snapshot, p.TotalShares)
	s.seq++
	b := &Batch{
		ID:        fmt.Sprintf("%s-B%d", p.ID, len(p.Batches)+1),
		Seq:       s.seq,
		Source:    src,
		Total:     total,
		Dust:      dust,
		Status:    BatchOpen,
		CreatedAt: s.now(),
	}
	for i, e := range p.Snapshot {
		b.Items = append(b.Items, &Item{
			InvestorID: e.InvestorID,
			Shares:     e.Shares,
			Amount:     amounts[i],
			Status:     ItemPending,
		})
	}
	p.Batches = append(p.Batches, b)
	return b
}

func checkExecutable(p *Plan) error {
	switch p.Status {
	case PlanCancelled:
		return ErrPlanCancelled
	case PlanCompleted:
		return ErrPlanCompleted
	case PlanDraft:
		return ErrPlanNotConfirmed
	}
	return nil
}

func (s *Service) get(planID string) (*Plan, error) {
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	return p, nil
}

// GetPlan returns the plan.
func (s *Service) GetPlan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(planID)
}

// GetSnapshot returns the frozen share snapshot of the plan.
func (s *Service) GetSnapshot(planID string) ([]SnapshotEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotEntry, len(p.Snapshot))
	copy(out, p.Snapshot)
	return out, nil
}

// ListBatches returns all distribution batches of the plan.
func (s *Service) ListBatches(planID string) ([]*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	out := make([]*Batch, len(p.Batches))
	copy(out, p.Batches)
	return out, nil
}

// PaymentProgress reports payment progress of one batch.
func (s *Service) PaymentProgress(planID, batchID string) (*Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	b, err := findBatch(p, batchID)
	if err != nil {
		return nil, err
	}
	pr := &Progress{BatchID: b.ID, Total: b.Total, Status: b.Status}
	for _, it := range b.Items {
		switch it.Status {
		case ItemPaid:
			pr.PaidCount++
			pr.PaidAmount += it.Amount
		case ItemPending:
			pr.PendingCount++
		case ItemOnHold:
			pr.OnHoldCount++
		}
	}
	return pr, nil
}

// RemainingAssets reports what is still held: unreleased reserves,
// unpaid (pending/on-hold) items and rounding dust.
func (s *Service) RemainingAssets(planID string) (*RemainingAssets, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return nil, err
	}
	r := &RemainingAssets{FeeReserve: p.FeeRemaining, DisputeReserve: p.DisputeRemain}
	for _, b := range p.Batches {
		if b.Status == BatchCancelled {
			continue
		}
		r.Dust += b.Dust
		for _, it := range b.Items {
			if it.Status != ItemPaid {
				r.UnpaidItems += it.Amount
			}
		}
	}
	r.Total = r.FeeReserve + r.DisputeReserve + r.UnpaidItems + r.Dust
	return r, nil
}

func findBatch(p *Plan, batchID string) (*Batch, error) {
	for _, b := range p.Batches {
		if b.ID == batchID {
			return b, nil
		}
	}
	return nil, ErrBatchNotFound
}
