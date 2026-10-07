package capitalcall

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service manages commitments, notices and payments. All mutating
// operations are serialised by a single mutex and validate before
// applying any change, so a failed operation never partially updates
// commitment usage, called amounts or cash records.
type Service struct {
	mu          sync.Mutex
	commitments map[string]*Commitment
	notices     map[string]*Notice
	payments    map[string]*Payment // by payment ID
	byBankRef   map[string]*Payment // by bank reference, for idempotency
}

// NewService returns an empty Service.
func NewService() *Service {
	return &Service{
		commitments: make(map[string]*Commitment),
		notices:     make(map[string]*Notice),
		payments:    make(map[string]*Payment),
		byBankRef:   make(map[string]*Payment),
	}
}

// RegisterCommitment registers an active commitment for an investor.
func (s *Service) RegisterCommitment(id, investorID, currency string, total int64) error {
	if total <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.commitments[id]; ok {
		return ErrCommitmentExists
	}
	s.commitments[id] = &Commitment{
		ID:         id,
		InvestorID: investorID,
		Currency:   currency,
		Total:      total,
		Active:     true,
	}
	return nil
}

// SetCommitmentActive activates or deactivates a commitment. Inactive
// commitments keep their called/paid history but are excluded from new
// notices.
func (s *Service) SetCommitmentActive(id string, active bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.commitments[id]
	if !ok {
		return ErrCommitmentNotFound
	}
	c.Active = active
	return nil
}

// GenerateNotice creates a capital call notice and freezes the remaining
// uncalled amount of every active commitment in the currency: the target
// is distributed pro rata to uncalled commitment, rounded so allocations
// sum exactly to the target and never exceed any investor's uncalled
// amount. If total uncalled commitment is below the target the whole
// notice fails and nothing is reserved.
func (s *Service) GenerateNotice(id, purpose, currency string, target int64, dueDate, now time.Time) (*Notice, error) {
	if target <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.notices[id]; ok {
		return nil, ErrNoticeExists
	}

	var eligible []*Commitment
	var totalUncalled int64
	for _, c := range s.commitments {
		if c.Active && c.Currency == currency && c.Uncalled() > 0 {
			eligible = append(eligible, c)
			totalUncalled += c.Uncalled()
		}
	}
	if totalUncalled < target {
		return nil, fmt.Errorf("%w: need %d %s, only %d uncalled", ErrInsufficient, target, currency, totalUncalled)
	}

	amounts := allocate(target, eligible)

	notice := &Notice{
		ID:           id,
		Purpose:      purpose,
		Currency:     currency,
		TargetAmount: target,
		DueDate:      dueDate,
		Status:       NoticeActive,
		CreatedAt:    now,
	}
	for _, c := range eligible {
		amt := amounts[c.ID]
		if amt == 0 {
			continue
		}
		notice.Allocations = append(notice.Allocations, Allocation{CommitmentID: c.ID, InvestorID: c.InvestorID, Amount: amt})
		c.Called += amt
	}
	sort.Slice(notice.Allocations, func(i, j int) bool {
		return notice.Allocations[i].CommitmentID < notice.Allocations[j].CommitmentID
	})
	s.notices[id] = notice
	cp := *notice
	cp.Allocations = append([]Allocation(nil), notice.Allocations...)
	return &cp, nil
}

// allocate distributes target pro rata to each commitment's uncalled
// amount using largest-remainder rounding, capping every share at the
// commitment's uncalled amount and redistributing capped remainders.
// The result always sums exactly to target.
func allocate(target int64, eligible []*Commitment) map[string]int64 {
	remaining := make(map[string]int64, len(eligible))
	var total int64
	for _, c := range eligible {
		remaining[c.ID] = c.Uncalled()
		total += c.Uncalled()
	}
	out := make(map[string]int64, len(eligible))
	left := target
	for left > 0 {
		type frac struct {
			id  string
			rem int64 // numerator of the fractional part, denominator is total
		}
		var fracs []frac
		var distributed int64
		for _, c := range eligible {
			if remaining[c.ID] == 0 {
				continue
			}
			exactNum := left * remaining[c.ID] // exact share = exactNum / total
			share := exactNum / total
			if share > remaining[c.ID] {
				share = remaining[c.ID]
			}
			out[c.ID] += share
			remaining[c.ID] -= share
			distributed += share
			fracs = append(fracs, frac{id: c.ID, rem: exactNum % total})
		}
		left -= distributed
		if left == 0 {
			break
		}
		// Hand out the remaining units one by one, largest fractional
		// remainder first, skipping commitments already at their cap.
		sort.Slice(fracs, func(i, j int) bool {
			if fracs[i].rem != fracs[j].rem {
				return fracs[i].rem > fracs[j].rem
			}
			return fracs[i].id < fracs[j].id
		})
		progress := false
		for _, f := range fracs {
			if left == 0 {
				break
			}
			if remaining[f.id] > 0 {
				out[f.id]++
				remaining[f.id]--
				left--
				progress = true
			}
		}
		if !progress {
			// Unreachable when total uncalled >= target.
			break
		}
	}
	return out
}

// ConfirmPayment confirms a payment of amount against the investor's
// allocation on the notice. The bank reference is idempotent: confirming
// an already-seen reference returns the original payment without
// increasing paid amounts. Cumulative confirmed payments for an
// allocation may not exceed the allocated amount; on violation nothing
// is recorded.
func (s *Service) ConfirmPayment(paymentID, noticeID, investorID, bankReference string, amount int64, now time.Time) (*Payment, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byBankRef[bankReference]; ok {
		cp := *existing
		return &cp, nil
	}
	if _, ok := s.payments[paymentID]; ok {
		return nil, fmt.Errorf("capitalcall: payment %q already exists", paymentID)
	}
	notice, ok := s.notices[noticeID]
	if !ok {
		return nil, ErrNoticeNotFound
	}
	if notice.Status != NoticeActive {
		return nil, ErrNoticeNotActive
	}
	allocs := findAllocations(notice, investorID)
	if len(allocs) == 0 {
		return nil, ErrNoAllocation
	}
	var outstanding int64
	for _, a := range allocs {
		outstanding += a.Outstanding()
	}
	if amount > outstanding {
		return nil, fmt.Errorf("%w: allocation %d, paid %d, attempted %d",
			ErrOverpayment, outstanding+paidOf(allocs), paidOf(allocs), amount)
	}
	payment := &Payment{
		ID:            paymentID,
		NoticeID:      noticeID,
		InvestorID:    investorID,
		BankReference: bankReference,
		Amount:        amount,
		ConfirmedAt:   now,
	}
	// Apply all updates together: cash record, allocation paid amount
	// and commitment paid amount.
	s.payments[paymentID] = payment
	s.byBankRef[bankReference] = payment
	left := amount
	for _, a := range allocs {
		if left == 0 {
			break
		}
		take := a.Outstanding()
		if take > left {
			take = left
		}
		a.PaidAmount += take
		s.commitments[a.CommitmentID].Paid += take
		left -= take
	}
	cp := *payment
	return &cp, nil
}

func paidOf(allocs []*Allocation) int64 {
	var sum int64
	for _, a := range allocs {
		sum += a.PaidAmount
	}
	return sum
}

// CancelNotice cancels an active notice and releases the unpaid part of
// every allocation back to uncalled commitment. Confirmed payments stay
// called and paid.
func (s *Service) CancelNotice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	notice, ok := s.notices[id]
	if !ok {
		return ErrNoticeNotFound
	}
	if notice.Status != NoticeActive {
		return ErrNoticeNotActive
	}
	notice.Status = NoticeCancelled
	for i := range notice.Allocations {
		alloc := &notice.Allocations[i]
		release := alloc.Outstanding()
		if release == 0 {
			continue
		}
		s.commitments[alloc.CommitmentID].Called -= release
	}
	return nil
}

// GetNotice returns a copy of a notice.
func (s *Service) GetNotice(id string) (*Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notice, ok := s.notices[id]
	if !ok {
		return nil, ErrNoticeNotFound
	}
	cp := *notice
	cp.Allocations = append([]Allocation(nil), notice.Allocations...)
	return &cp, nil
}

// GetAllocation returns the investor's aggregated allocation on a notice.
func (s *Service) GetAllocation(noticeID, investorID string) (*Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notice, ok := s.notices[noticeID]
	if !ok {
		return nil, ErrNoticeNotFound
	}
	allocs := findAllocations(notice, investorID)
	if len(allocs) == 0 {
		return nil, ErrNoAllocation
	}
	agg := Allocation{InvestorID: investorID}
	for _, a := range allocs {
		agg.Amount += a.Amount
		agg.PaidAmount += a.PaidAmount
	}
	return &agg, nil
}

// GetPayment returns a payment by ID.
func (s *Service) GetPayment(id string) (*Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[id]
	if !ok {
		return nil, fmt.Errorf("capitalcall: payment %q not found", id)
	}
	cp := *p
	return &cp, nil
}

// Position returns the investor's aggregated position in a currency,
// split into uncalled, notified-but-unpaid and paid amounts.
func (s *Service) Position(investorID, currency string) Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := Position{InvestorID: investorID, Currency: currency}
	for _, c := range s.commitments {
		if c.InvestorID != investorID || c.Currency != currency {
			continue
		}
		pos.Total += c.Total
		pos.Uncalled += c.Uncalled()
		pos.NotifiedUnpaid += c.NotifiedUnpaid()
		pos.Paid += c.Paid
	}
	return pos
}

// OverdueNotices returns active notices whose due date is before now and
// which still have unpaid allocations.
func (s *Service) OverdueNotices(now time.Time) []Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Notice
	for _, n := range s.notices {
		if n.Status != NoticeActive || !n.DueDate.Before(now) {
			continue
		}
		if n.PaidTotal() < n.TargetAmount {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func findAllocations(n *Notice, investorID string) []*Allocation {
	var out []*Allocation
	for i := range n.Allocations {
		if n.Allocations[i].InvestorID == investorID {
			out = append(out, &n.Allocations[i])
		}
	}
	return out
}
