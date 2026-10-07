package capitalcall

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service 出资承诺与缴款通知服务。所有状态变更在单把互斥锁内完成，
// 保证承诺占用、已调用金额与现金记录的一致性。
type Service struct {
	mu          sync.Mutex
	commitments map[string]*Commitment
	notices     map[string]*Notice
	allocations map[string]*Allocation
	payments    map[string]*Payment
	bankRefs    map[string]string // bankRef -> paymentID
	seq         int
	now         func() time.Time
}

func NewService() *Service {
	return &Service{
		commitments: make(map[string]*Commitment),
		notices:     make(map[string]*Notice),
		allocations: make(map[string]*Allocation),
		payments:    make(map[string]*Payment),
		bankRefs:    make(map[string]string),
		now:         time.Now,
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// RegisterCommitment 登记投资者承诺。
func (s *Service) RegisterCommitment(investor, currency string, total Money) (*Commitment, error) {
	if total <= 0 || investor == "" || currency == "" {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Commitment{
		ID:        s.nextID("cmt"),
		Investor:  investor,
		Currency:  currency,
		Total:     total,
		Active:    true,
		CreatedAt: s.now(),
	}
	s.commitments[c.ID] = c
	cp := *c
	return &cp, nil
}

// DeactivateCommitment 将承诺置为无效，不再参与新通知分配。
func (s *Service) DeactivateCommitment(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.commitments[id]
	if !ok {
		return ErrCommitmentNotFound
	}
	c.Active = false
	return nil
}

// CreateNotice 生成缴款通知：按未调用承诺比例分配目标总额，
// 冻结全部有效承诺的剩余额度。任一投资者额度不足则整份失败。
func (s *Service) CreateNotice(purpose, currency string, target Money, dueDate time.Time) (*Notice, []Allocation, error) {
	if target <= 0 {
		return nil, nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var eligible []*Commitment
	var totalRemaining Money
	for _, c := range s.commitments {
		if !c.Active || c.Currency != currency {
			continue
		}
		eligible = append(eligible, c)
		totalRemaining += c.Remaining()
	}
	if len(eligible) == 0 || totalRemaining < target {
		return nil, nil, ErrInsufficient
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })

	shares, err := allocate(target, eligible)
	if err != nil {
		return nil, nil, err
	}

	n := &Notice{
		ID:        s.nextID("ntc"),
		Purpose:   purpose,
		Currency:  currency,
		Target:    target,
		DueDate:   dueDate,
		Status:    NoticeOpen,
		CreatedAt: s.now(),
	}
	allocs := make([]Allocation, 0, len(eligible))
	for i, c := range eligible {
		if shares[i] == 0 {
			continue
		}
		a := &Allocation{
			ID:         s.nextID("alc"),
			NoticeID:   n.ID,
			Investor:   c.Investor,
			Amount:     shares[i],
			Commitment: c.ID,
		}
		c.Called += shares[i] // 冻结剩余额度
		s.allocations[a.ID] = a
		allocs = append(allocs, *a)
	}
	s.notices[n.ID] = n
	cp := *n
	return &cp, allocs, nil
}

// allocate 按剩余额度比例分配 target，向下取整后按小数部分从大到小
// 补齐余数；任何份额不得超过对应承诺的剩余额度，否则整体失败。
func allocate(target Money, commitments []*Commitment) ([]Money, error) {
	var totalRemaining Money
	for _, c := range commitments {
		totalRemaining += c.Remaining()
	}
	shares := make([]Money, len(commitments))
	type frac struct {
		idx int
		rem Money // target*remaining 的小数余数
	}
	fracts := make([]frac, 0, len(commitments))
	var assigned Money
	for i, c := range commitments {
		r := c.Remaining()
		prod := int64(target) * int64(r)
		share := Money(prod / int64(totalRemaining))
		if share > r {
			return nil, ErrInsufficient
		}
		shares[i] = share
		assigned += share
		fracts = append(fracts, frac{idx: i, rem: Money(prod % int64(totalRemaining))})
	}
	sort.Slice(fracts, func(i, j int) bool {
		if fracts[i].rem != fracts[j].rem {
			return fracts[i].rem > fracts[j].rem
		}
		return commitments[fracts[i].idx].ID < commitments[fracts[j].idx].ID
	})
	leftover := target - assigned
	for _, f := range fracts {
		if leftover == 0 {
			break
		}
		if shares[f.idx] < commitments[f.idx].Remaining() {
			shares[f.idx]++
			leftover--
		}
	}
	if leftover > 0 {
		return nil, ErrInsufficient
	}
	return shares, nil
}

// Allocations 查询通知下的投资者分配。
func (s *Service) Allocations(noticeID string) ([]Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.notices[noticeID]; !ok {
		return nil, ErrNoticeNotFound
	}
	var out []Allocation
	for _, a := range s.allocations {
		if a.NoticeID == noticeID {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ConfirmPayment 确认一笔缴款。同一银行流水号重复提交不会重复增加实缴。
// 累计实缴不得超过该分配的应缴金额。
func (s *Service) ConfirmPayment(allocationID, bankRef string, amount Money) (*Payment, error) {
	if amount <= 0 || bankRef == "" {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if pid, ok := s.bankRefs[bankRef]; ok {
		p := s.payments[pid]
		cp := *p
		return &cp, nil // 幂等：返回首次确认的结果
	}
	a, ok := s.allocations[allocationID]
	if !ok {
		return nil, ErrAllocationNotFound
	}
	n := s.notices[a.NoticeID]
	if n.Status != NoticeOpen {
		return nil, ErrNoticeNotOpen
	}
	if a.Paid+amount > a.Amount {
		return nil, ErrOverpay
	}
	p := &Payment{
		ID:           s.nextID("pay"),
		NoticeID:     a.NoticeID,
		AllocationID: a.ID,
		Investor:     a.Investor,
		BankRef:      bankRef,
		Amount:       amount,
		ConfirmedAt:  s.now(),
	}
	s.payments[p.ID] = p
	s.bankRefs[bankRef] = p.ID
	a.Paid += amount

	// 全部应缴完成则通知完结。
	done := true
	for _, other := range s.allocations {
		if other.NoticeID == n.ID && other.Paid < other.Amount {
			done = false
			break
		}
	}
	if done {
		n.Status = NoticeCompleted
	}
	cp := *p
	return &cp, nil
}

// CancelNotice 取消通知并释放未缴部分的承诺占用；
// 已实缴部分保持已调用。与缴款确认互斥，不会出现半更新。
func (s *Service) CancelNotice(noticeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[noticeID]
	if !ok {
		return ErrNoticeNotFound
	}
	if n.Status != NoticeOpen {
		return ErrNoticeNotOpen
	}
	n.Status = NoticeCancelled
	for _, a := range s.allocations {
		if a.NoticeID != noticeID {
			continue
		}
		c := s.commitments[a.Commitment]
		c.Called -= a.Outstanding() // 仅释放未缴部分
	}
	return nil
}

// OverdueNotices 返回截至 asOf 已逾期且未完成的通知。
func (s *Service) OverdueNotices(asOf time.Time) []Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Notice
	for _, n := range s.notices {
		if n.Status == NoticeOpen && n.DueDate.Before(asOf) {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Position 查询投资者额度：未调用、已通知未缴、已实缴。
func (s *Service) Position(investor string) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := Position{Investor: investor}
	found := false
	for _, c := range s.commitments {
		if c.Investor != investor {
			continue
		}
		found = true
		pos.Currency = c.Currency
		pos.Total += c.Total
		pos.Called += c.Called
	}
	if !found {
		return pos, ErrCommitmentNotFound
	}
	for _, a := range s.allocations {
		if a.Investor != investor {
			continue
		}
		n := s.notices[a.NoticeID]
		pos.Paid += a.Paid
		if n.Status == NoticeOpen {
			pos.Notified += a.Outstanding()
		}
	}
	pos.Uncalled = pos.Total - pos.Called
	pos.Remaining = pos.Uncalled
	return pos, nil
}
