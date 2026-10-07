package liquidation

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrPlanNotFound     = errors.New("liquidation: plan not found")
	ErrBatchNotFound    = errors.New("liquidation: batch not found")
	ErrInvalidState     = errors.New("liquidation: invalid state transition")
	ErrInvalidInput     = errors.New("liquidation: invalid input")
	ErrTradingHalted    = errors.New("liquidation: share trading halted for fund")
	ErrNothingToRelease = errors.New("liquidation: no reserve available to release")
)

// Payer 执行单笔付款的通道接口。返回 nil 表示付款成功。
// 实现方必须按 IdempotencyKey 去重，保证重复调用不产生重复付款。
type Payer func(item PaymentItem) error

// ShareLedger 基金份额台账接口，由上层交易模块实现。
type ShareLedger interface {
	// HoldingsAt 返回指定登记日收盘后的持仓。
	HoldingsAt(fundID string, recordDate time.Time) []SnapshotEntry
}

// Service 清算分配服务。所有状态变更在互斥锁内完成，
// 保证取消、确认、释放、付款确认并发时产生唯一结果。
type Service struct {
	mu       sync.Mutex
	plans    map[string]*Plan
	batches  map[string]*Batch
	accounts map[string]AccountStatus // investorID -> 账户状态
	halted   map[string]bool          // fundID -> 是否停止份额交易
	ledger   ShareLedger
	payer    Payer
	seq      int
	now      func() time.Time
}

func NewService(ledger ShareLedger, payer Payer) *Service {
	if payer == nil {
		payer = func(PaymentItem) error { return nil }
	}
	return &Service{
		plans:    map[string]*Plan{},
		batches:  map[string]*Batch{},
		accounts: map[string]AccountStatus{},
		halted:   map[string]bool{},
		ledger:   ledger,
		payer:    payer,
		now:      time.Now,
	}
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%06d", prefix, s.seq)
}

// SetAccountStatus 设置投资者账户状态（模拟账户系统）。
func (s *Service) SetAccountStatus(investorID string, st AccountStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[investorID] = st
}

// CreatePlan 创建清算方案（草案）。
func (s *Service) CreatePlan(fundID string, recordDate time.Time, distributable, feeReserve, disputeReserve Money) (*Plan, error) {
	if fundID == "" || distributable < 0 || feeReserve < 0 || disputeReserve < 0 {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Plan{
		ID:             s.nextID("PLAN"),
		FundID:         fundID,
		RecordDate:     recordDate,
		Distributable:  distributable,
		FeeReserve:     feeReserve,
		DisputeReserve: disputeReserve,
		Status:         PlanDraft,
	}
	s.plans[p.ID] = p
	return p, nil
}

// ConfirmPlan 确认方案：冻结登记日份额快照，并停止该基金新的份额交易。
// 登记日之后的持仓变化不会进入快照。
func (s *Service) ConfirmPlan(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if p.Status != PlanDraft {
		return ErrInvalidState
	}
	snap := s.ledger.HoldingsAt(p.FundID, p.RecordDate)
	snap = append([]SnapshotEntry(nil), snap...)
	sort.Slice(snap, func(i, j int) bool { return snap[i].InvestorID < snap[j].InvestorID })
	var total int64
	for _, e := range snap {
		if e.Shares < 0 {
			return ErrInvalidInput
		}
		total += e.Shares
	}
	if total <= 0 {
		return ErrInvalidInput
	}
	p.Snapshot = snap
	p.TotalShares = total
	p.Status = PlanConfirmed
	s.halted[p.FundID] = true
	return nil
}

// CancelPlan 取消方案。仅草案或已确认且尚未生成任何批次时可取消；
// 与确认/释放/付款并发时由锁保证唯一结果。
func (s *Service) CancelPlan(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if p.Status != PlanDraft && p.Status != PlanConfirmed {
		return ErrInvalidState
	}
	for _, b := range s.batches {
		if b.PlanID == planID {
			return ErrInvalidState
		}
	}
	p.Status = PlanCancelled
	delete(s.halted, p.FundID)
	return nil
}

// RecordShareTrade 模拟份额交易入口：方案确认后该基金拒绝新的份额交易。
func (s *Service) RecordShareTrade(fundID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted[fundID] {
		return ErrTradingHalted
	}
	return nil
}

// allocate 按冻结份额做确定性舍入分配：
// 每项应付 = floor(pool * shares / totalShares)，尾差 = pool - Σ应付。
// 规则对相同输入恒定，尾差计入批次并汇总到方案。
func allocate(pool Money, snap []SnapshotEntry, totalShares int64) (amounts []Money, remainder Money) {
	amounts = make([]Money, len(snap))
	var sum Money
	for i, e := range snap {
		// 先乘后除，使用整数运算；pool 与 shares 均为非负。
		amt := pool * Money(e.Shares) / Money(totalShares)
		amounts[i] = amt
		sum += amt
	}
	return amounts, pool - sum
}

func (s *Service) newBatchLocked(p *Plan, kind BatchKind, pool Money) *Batch {
	amounts, remainder := allocate(pool, p.Snapshot, p.TotalShares)
	seq := 0
	for _, b := range s.batches {
		if b.PlanID == p.ID {
			seq++
		}
	}
	seq++
	b := &Batch{
		ID:        s.nextID("BATCH"),
		PlanID:    p.ID,
		Kind:      kind,
		Seq:       seq,
		Pool:      pool,
		Rounding:  remainder,
		Status:    BatchOpen,
		CreatedAt: s.now(),
	}
	for i, e := range p.Snapshot {
		item := &PaymentItem{
			ID:             s.nextID("PAY"),
			BatchID:        b.ID,
			InvestorID:     e.InvestorID,
			Shares:         e.Shares,
			Amount:         amounts[i],
			Status:         PayPending,
			IdempotencyKey: fmt.Sprintf("%s/%s", b.ID, e.InvestorID),
		}
		b.Items = append(b.Items, item)
	}
	s.batches[b.ID] = b
	p.DistributedTotal += pool - remainder
	p.RoundingRemainder += remainder
	if p.Status == PlanConfirmed {
		p.Status = PlanInProgress
	}
	return b
}

// CreateFirstDistribution 生成首笔分配批次，分配池为可分配现金。
func (s *Service) CreateFirstDistribution(planID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	if p.Status != PlanConfirmed {
		return nil, ErrInvalidState
	}
	return s.newBatchLocked(p, BatchFirst, p.Distributable), nil
}

// ReleaseReserve 释放准备金并形成补充分配批次。
// 补充分配仍按原冻结份额计算；账户异常的投资者不静默跳过，
// 其明细在执行时进入 ON_HOLD 待处理状态。
func (s *Service) ReleaseReserve(planID string, feeAmount, disputeAmount Money) (*Batch, error) {
	if feeAmount < 0 || disputeAmount < 0 || (feeAmount == 0 && disputeAmount == 0) {
		return nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	if p.Status != PlanConfirmed && p.Status != PlanInProgress {
		return nil, ErrInvalidState
	}
	if feeAmount > p.FeeReserve-p.ReleasedFee || disputeAmount > p.DisputeReserve-p.ReleasedDispute {
		return nil, ErrNothingToRelease
	}
	p.ReleasedFee += feeAmount
	p.ReleasedDispute += disputeAmount
	return s.newBatchLocked(p, BatchSupplemental, feeAmount+disputeAmount), nil
}

// ExecuteBatch 扫描批次明细并付款。只处理 PENDING/FAILED 明细，
// 中断后重试仅补齐未完成明细，幂等键保证不重复付款。
// maxItems > 0 时限制本次扫描处理条数，用于模拟中断。
func (s *Service) ExecuteBatch(batchID string, maxItems int) error {
	s.mu.Lock()
	b, ok := s.batches[batchID]
	if !ok {
		s.mu.Unlock()
		return ErrBatchNotFound
	}
	p := s.plans[b.PlanID]
	if p.Status == PlanCancelled {
		s.mu.Unlock()
		return ErrInvalidState
	}
	b.Status = BatchExecuting
	// 快照待处理明细，账户状态在锁内判定。
	type todo struct {
		item   *PaymentItem
		onHold bool
	}
	var todos []todo
	for _, it := range b.Items {
		if it.Status != PayPending && it.Status != PayFailed {
			continue
		}
		st := s.accounts[it.InvestorID]
		if st == "" {
			st = AccountActive
		}
		if st != AccountActive {
			// 已退出或账户状态变化：进入明确的待处理状态，不静默跳过。
			it.Status = PayOnHold
			continue
		}
		todos = append(todos, todo{item: it})
	}
	s.mu.Unlock()

	processed := 0
	for _, t := range todos {
		if maxItems > 0 && processed >= maxItems {
			break
		}
		processed++
		err := s.payer(*t.item)
		s.mu.Lock()
		t.item.Attempts++
		if err != nil {
			t.item.Status = PayFailed
		} else {
			now := s.now()
			t.item.Status = PayPaid
			t.item.PaidAt = &now
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	done := true
	for _, it := range b.Items {
		if it.Status == PayPending || it.Status == PayFailed {
			done = false
			break
		}
	}
	if done {
		b.Status = BatchDone
	} else {
		b.Status = BatchOpen
	}
	s.maybeCompleteLocked(p)
	return nil
}

func (s *Service) maybeCompleteLocked(p *Plan) {
	if p.Status != PlanInProgress {
		return
	}
	if p.FeeReserve != p.ReleasedFee || p.DisputeReserve != p.ReleasedDispute {
		return
	}
	for _, b := range s.batches {
		if b.PlanID == p.ID && b.Status != BatchDone {
			return
		}
	}
	p.Status = PlanCompleted
}

// ResolveOnHold 人工处理 ON_HOLD 明细：账户恢复后置回 PENDING 等待重试。
func (s *Service) ResolveOnHold(batchID, investorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return ErrBatchNotFound
	}
	for _, it := range b.Items {
		if it.InvestorID == investorID && it.Status == PayOnHold {
			it.Status = PayPending
			b.Status = BatchOpen
			return nil
		}
	}
	return ErrInvalidState
}

// ---- 查询接口 ----

func clonePlan(p *Plan) *Plan {
	cp := *p
	cp.Snapshot = append([]SnapshotEntry(nil), p.Snapshot...)
	return &cp
}

// GetPlan 查询清算方案（含份额快照）。
func (s *Service) GetPlan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	return clonePlan(p), nil
}

// GetSnapshot 查询份额快照。
func (s *Service) GetSnapshot(planID string) ([]SnapshotEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	return append([]SnapshotEntry(nil), p.Snapshot...), nil
}

func cloneBatch(b *Batch) *Batch {
	cb := *b
	cb.Items = make([]*PaymentItem, len(b.Items))
	for i, it := range b.Items {
		ci := *it
		cb.Items[i] = &ci
	}
	return &cb
}

// ListBatches 查询方案下全部分配批次。
func (s *Service) ListBatches(planID string) ([]*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[planID]; !ok {
		return nil, ErrPlanNotFound
	}
	var out []*Batch
	for _, b := range s.batches {
		if b.PlanID == planID {
			out = append(out, cloneBatch(b))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// GetBatchProgress 查询批次付款进度。
func (s *Service) GetBatchProgress(batchID string) (*BatchProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	pr := &BatchProgress{BatchID: b.ID, Kind: b.Kind, Status: b.Status, Total: len(b.Items)}
	for _, it := range b.Items {
		pr.TotalAmount += it.Amount
		switch it.Status {
		case PayPaid:
			pr.Paid++
			pr.PaidAmount += it.Amount
		case PayOnHold:
			pr.OnHold++
		case PayFailed:
			pr.Failed++
		default:
			pr.Pending++
		}
	}
	return pr, nil
}

// GetRemainingAssets 查询剩余资产（未分配准备金 + 尾差）。
func (s *Service) GetRemainingAssets(planID string) (Money, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return 0, ErrPlanNotFound
	}
	return p.RemainingAssets(), nil
}
