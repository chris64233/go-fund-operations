package liquidation

import "fmt"

// PaymentGateway executes a single payout. Implementations must be
// idempotent per (batchID, investorID) so a retried scan never pays twice.
type PaymentGateway interface {
	Pay(batchID, investorID string, amount Money) error
}

// GatewayFunc adapts a function to PaymentGateway.
type GatewayFunc func(batchID, investorID string, amount Money) error

// Pay implements PaymentGateway.
func (f GatewayFunc) Pay(batchID, investorID string, amount Money) error {
	return f(batchID, investorID, amount)
}

// ExecuteResult summarizes one execution scan.
type ExecuteResult struct {
	BatchID     string `json:"batch_id"`
	PaidCount   int    `json:"paid_count"`
	OnHoldCount int    `json:"on_hold_count"`
	Interrupted bool   `json:"interrupted"`
	Err         error  `json:"-"`
}

// ExecuteBatch scans a batch and pays every pending item. Items whose
// investor has exited or whose account is abnormal are moved to ON_HOLD
// (explicit pending-handling state) instead of being silently skipped.
//
// The scan is resumable: if the gateway fails mid-scan, ExecuteBatch
// returns the error and a later retry continues with the remaining
// PENDING items only — already PAID items are never paid again.
func (s *Service) ExecuteBatch(planID, batchID string, gw PaymentGateway) (*ExecuteResult, error) {
	s.mu.Lock()
	p, err := s.get(planID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := checkExecutable(p); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	b, err := findBatch(p, batchID)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if b.Status == BatchCancelled {
		s.mu.Unlock()
		return nil, ErrPlanCancelled
	}
	res := &ExecuteResult{BatchID: b.ID}
	for _, it := range b.Items {
		if it.Status != ItemPending {
			continue
		}
		switch st := s.accounts(it.InvestorID); st {
		case AccountActive:
			// pay below
		case AccountExited:
			it.Status = ItemOnHold
			it.Reason = "investor exited before payment"
			res.OnHoldCount++
			continue
		default:
			it.Status = ItemOnHold
			it.Reason = fmt.Sprintf("account status %s", st)
			res.OnHoldCount++
			continue
		}
		// Mark as paid only after the gateway succeeds; a failure
		// leaves the item PENDING so a retry can resume it.
		if err := gw.Pay(b.ID, it.InvestorID, it.Amount); err != nil {
			res.Interrupted = true
			res.Err = err
			s.mu.Unlock()
			return res, fmt.Errorf("liquidation: payment interrupted at investor %s: %w", it.InvestorID, err)
		}
		now := s.now()
		it.Status = ItemPaid
		it.PaidAt = &now
		res.PaidCount++
	}
	s.finishBatchLocked(p, b)
	s.mu.Unlock()
	return res, nil
}

// finishBatchLocked updates batch status and completes the plan when
// every batch is done and both reserves are fully released.
func (s *Service) finishBatchLocked(p *Plan, b *Batch) {
	onHold, pending := 0, 0
	for _, it := range b.Items {
		switch it.Status {
		case ItemOnHold:
			onHold++
		case ItemPending:
			pending++
		}
	}
	switch {
	case pending > 0:
		// interrupted; stays OPEN for retry
	case onHold > 0:
		b.Status = BatchPartial
	default:
		b.Status = BatchDone
	}
	allDone := true
	for _, x := range p.Batches {
		if x.Status != BatchDone {
			allDone = false
			break
		}
	}
	if allDone && p.FeeRemaining == 0 && p.DisputeRemain == 0 {
		p.Status = PlanCompleted
	}
}

// ConfirmPayment confirms a single item as paid (e.g. after an external
// channel callback). Concurrent with cancellation or reserve release it
// is serialized by the service mutex, yielding one unique outcome.
func (s *Service) ConfirmPayment(planID, batchID, investorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(planID)
	if err != nil {
		return err
	}
	if err := checkExecutable(p); err != nil {
		return err
	}
	b, err := findBatch(p, batchID)
	if err != nil {
		return err
	}
	for _, it := range b.Items {
		if it.InvestorID != investorID {
			continue
		}
		if it.Status == ItemPaid {
			return nil // idempotent
		}
		if it.Status != ItemPending {
			return fmt.Errorf("liquidation: item of investor %s is %s, cannot confirm payment", investorID, it.Status)
		}
		now := s.now()
		it.Status = ItemPaid
		it.PaidAt = &now
		s.finishBatchLocked(p, b)
		return nil
	}
	return fmt.Errorf("liquidation: investor %s not found in batch %s", investorID, batchID)
}
