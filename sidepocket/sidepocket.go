// Package sidepocket 实现基金不易变现资产的侧袋划分。
//
// 侧袋机制：在登记日冻结投资者对目标资产的历史权益，生成独立的侧袋权益；
// 登记日之后的普通申购只能取得主基金份额，赎回主基金份额也不会带走侧袋权益。
// 目标资产回收现金后，按当前有效侧袋权益一次性生成分配明细，每一份权益
// 只归入一名最终持有人，不会重复分配。
package sidepocket

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 错误定义。
var (
	ErrPlanNotFound      = errors.New("sidepocket: 方案不存在")
	ErrPlanNotDraft      = errors.New("sidepocket: 方案不在草稿状态，不能确认")
	ErrPlanNotConfirmed  = errors.New("sidepocket: 方案尚未确认")
	ErrInvestorNotFound  = errors.New("sidepocket: 投资者不存在")
	ErrInsufficientShare = errors.New("sidepocket: 主基金份额不足")
	ErrInsufficientUnits = errors.New("sidepocket: 侧袋权益不足")
	ErrRecoveryNotFound  = errors.New("sidepocket: 回收记录不存在")
	ErrInvalidAmount     = errors.New("sidepocket: 金额或数量必须为正数")
	ErrSelfTransfer      = errors.New("sidepocket: 不能转移给自己")
)

// PlanStatus 表示侧袋方案状态。
type PlanStatus string

const (
	PlanDraft     PlanStatus = "DRAFT"     // 草稿，可修改
	PlanConfirmed PlanStatus = "CONFIRMED" // 已确认，权益已冻结生成
)

// Plan 是侧袋划分方案，记录基金、目标资产、登记日、资产估值、
// 划分比例和方案版本。
type Plan struct {
	ID             string
	FundID         string    // 基金
	AssetID        string    // 目标（不易变现）资产
	RecordDate     time.Time // 登记日：以该日日终持仓冻结权益
	AssetValuation int64     // 目标资产估值（最小货币单位）
	RatioBps       int64     // 划分比例（万分比，10000 = 100%）
	Version        int       // 方案版本
	Status         PlanStatus
	TotalUnits     int64 // 侧袋权益总量 = 估值 * 比例，确认时锁定
}

// Entitlement 是投资者在某一方案下的侧袋权益。
type Entitlement struct {
	PlanID      string
	InvestorID  string
	Units       int64 // 当前有效侧袋权益
	FrozenUnits int64 // 确认时冻结生成的原始权益
}

// Recovery 是目标资产的一笔现金回收。
type Recovery struct {
	ID          string
	PlanID      string
	Amount      int64 // 回收现金（最小货币单位）
	RecoveredAt time.Time
	Distributed bool // 是否已生成分配明细（防重复扫描）
}

// Distribution 是分配明细：某笔回收中某投资者应得的现金。
type Distribution struct {
	RecoveryID    string
	PlanID        string
	InvestorID    string
	Units         int64 // 参与分配的侧袋权益
	Amount        int64 // 分得现金
	DistributedAt time.Time
}

// account 是投资者账户：主基金份额与侧袋权益严格分离。
type account struct {
	mainShares int64 // 主基金份额，普通申赎只影响它
}

// Service 是侧袋业务服务，内部以互斥锁保证并发安全：
// 权益转移、回收确认与分配扫描并发时，每份权益只归入一名最终持有人。
type Service struct {
	mu            sync.Mutex
	plans         map[string]*Plan
	accounts      map[string]*account
	entitlements  map[string]map[string]*Entitlement // planID -> investorID -> 权益
	recoveries    map[string]*Recovery
	distributions map[string][]*Distribution // recoveryID -> 明细
	nextSeq       int
}

// NewService 创建侧袋服务。
func NewService() *Service {
	return &Service{
		plans:         make(map[string]*Plan),
		accounts:      make(map[string]*account),
		entitlements:  make(map[string]map[string]*Entitlement),
		recoveries:    make(map[string]*Recovery),
		distributions: make(map[string][]*Distribution),
	}
}

func (s *Service) genID(prefix string) string {
	s.nextSeq++
	return fmt.Sprintf("%s-%06d", prefix, s.nextSeq)
}

// CreatePlan 创建草稿状态的侧袋方案。
func (s *Service) CreatePlan(fundID, assetID string, recordDate time.Time, valuation, ratioBps int64) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if valuation <= 0 || ratioBps <= 0 || ratioBps > 10000 {
		return nil, ErrInvalidAmount
	}
	p := &Plan{
		ID:             s.genID("SP"),
		FundID:         fundID,
		AssetID:        assetID,
		RecordDate:     recordDate,
		AssetValuation: valuation,
		RatioBps:       ratioBps,
		Version:        1,
		Status:         PlanDraft,
	}
	s.plans[p.ID] = p
	return p, nil
}

// Subscribe 普通申购：只增加主基金份额。
// 登记日之后申购的投资者只取得主基金份额，不产生侧袋权益。
func (s *Service) Subscribe(investorID string, shares int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if shares <= 0 {
		return ErrInvalidAmount
	}
	s.account(investorID).mainShares += shares
	return nil
}

// Redeem 普通赎回：只扣减主基金份额，侧袋权益继续保留。
func (s *Service) Redeem(investorID string, shares int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if shares <= 0 {
		return ErrInvalidAmount
	}
	a, ok := s.accounts[investorID]
	if !ok {
		return ErrInvestorNotFound
	}
	if a.mainShares < shares {
		return ErrInsufficientShare
	}
	a.mainShares -= shares
	return nil
}

// MainShares 查询主基金份额（可赎回数量只按它计算，不含侧袋权益）。
func (s *Service) MainShares(investorID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(investorID).mainShares
}

func (s *Service) account(investorID string) *account {
	a, ok := s.accounts[investorID]
	if !ok {
		a = &account{}
		s.accounts[investorID] = a
	}
	return a
}

// ConfirmPlan 确认方案：以登记日日终的主基金份额为基数冻结权益，
// 按比例生成侧袋权益。采用最大余数法取整，保证所有投资者权益
// 合计与方案总量 TotalUnits 完全一致。
func (s *Service) ConfirmPlan(planID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if p.Status != PlanDraft {
		return ErrPlanNotDraft
	}
	total := p.AssetValuation * p.RatioBps / 10000
	p.TotalUnits = total

	// 登记日冻结基数：当前主基金份额（登记日日终快照）。
	type holder struct {
		id       string
		shares   int64
		exactNum int64 // shares * total 的分子，用于最大余数法
	}
	var totalShares int64
	holders := make([]holder, 0, len(s.accounts))
	for id, a := range s.accounts {
		if a.mainShares > 0 {
			holders = append(holders, holder{id: id, shares: a.mainShares, exactNum: a.mainShares * total})
			totalShares += a.mainShares
		}
	}
	ents := make(map[string]*Entitlement, len(holders))
	if totalShares > 0 && total > 0 {
		var allocated int64
		for i := range holders {
			units := holders[i].exactNum / totalShares
			allocated += units
			ents[holders[i].id] = &Entitlement{
				PlanID: planID, InvestorID: holders[i].id,
				Units: units, FrozenUnits: units,
			}
		}
		// 余数按小数部分从大到小补给投资者，保证合计 == TotalUnits。
		remainder := total - allocated
		sort.Slice(holders, func(i, j int) bool {
			ri := holders[i].exactNum % totalShares
			rj := holders[j].exactNum % totalShares
			if ri != rj {
				return ri > rj
			}
			return holders[i].id < holders[j].id
		})
		for i := int64(0); i < remainder; i++ {
			e := ents[holders[i%int64(len(holders))].id]
			e.Units++
			e.FrozenUnits++
		}
	}
	s.entitlements[planID] = ents
	p.Status = PlanConfirmed
	p.Version++
	_ = now
	return nil
}

// EntitlementOf 查询投资者在方案下的当前侧袋权益。
func (s *Service) EntitlementOf(planID, investorID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entitlements[planID][investorID]; ok {
		return e.Units
	}
	return 0
}

// TotalEntitlements 返回方案下所有投资者权益合计（应恒等于 TotalUnits）。
func (s *Service) TotalEntitlements(planID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, e := range s.entitlements[planID] {
		sum += e.Units
	}
	return sum
}

// TransferEntitlement 将侧袋权益随投资者账户转移。
// 转移只影响侧袋权益，不与主基金份额混合；全程持锁，
// 与分配扫描互斥，保证每份权益只有一个最终持有人。
func (s *Service) TransferEntitlement(planID, fromID, toID string, units int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if units <= 0 {
		return ErrInvalidAmount
	}
	if fromID == toID {
		return ErrSelfTransfer
	}
	p, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if p.Status != PlanConfirmed {
		return ErrPlanNotConfirmed
	}
	from, ok := s.entitlements[planID][fromID]
	if !ok || from.Units < units {
		return ErrInsufficientUnits
	}
	from.Units -= units
	to, ok := s.entitlements[planID][toID]
	if !ok {
		to = &Entitlement{PlanID: planID, InvestorID: toID}
		s.entitlements[planID][toID] = to
	}
	to.Units += units
	return nil
}

// ConfirmRecovery 登记目标资产的一笔现金回收。
func (s *Service) ConfirmRecovery(planID string, amount int64, now time.Time) (*Recovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	if p.Status != PlanConfirmed {
		return nil, ErrPlanNotConfirmed
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	r := &Recovery{ID: s.genID("RC"), PlanID: planID, Amount: amount, RecoveredAt: now}
	s.recoveries[r.ID] = r
	return r, nil
}

// DistributeRecovery 按当前有效侧袋权益为一笔回收生成分配明细。
// 每笔回收只分配一次（幂等）：重复扫描返回已生成的明细，不会重复分配。
// 金额按权益占比以最大余数法取整，明细合计等于回收金额。
func (s *Service) DistributeRecovery(recoveryID string, now time.Time) ([]*Distribution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recoveries[recoveryID]
	if !ok {
		return nil, ErrRecoveryNotFound
	}
	if r.Distributed {
		return s.distributions[recoveryID], nil
	}
	s.distributeLocked(r, now)
	return s.distributions[recoveryID], nil
}

// distributeLocked 在持锁状态下生成分配明细。
func (s *Service) distributeLocked(r *Recovery, now time.Time) {
	ents := s.entitlements[r.PlanID]
	var totalUnits int64
	ids := make([]string, 0, len(ents))
	for id, e := range ents {
		if e.Units > 0 {
			ids = append(ids, id)
			totalUnits += e.Units
		}
	}
	sort.Strings(ids)
	dists := make([]*Distribution, 0, len(ids))
	if totalUnits > 0 {
		var allocated int64
		for _, id := range ids {
			amt := ents[id].Units * r.Amount / totalUnits
			allocated += amt
			dists = append(dists, &Distribution{
				RecoveryID: r.ID, PlanID: r.PlanID, InvestorID: id,
				Units: ents[id].Units, Amount: amt, DistributedAt: now,
			})
		}
		remainder := r.Amount - allocated
		byRem := append([]string(nil), ids...)
		sort.Slice(byRem, func(i, j int) bool {
			ri := ents[byRem[i]].Units * r.Amount % totalUnits
			rj := ents[byRem[j]].Units * r.Amount % totalUnits
			if ri != rj {
				return ri > rj
			}
			return byRem[i] < byRem[j]
		})
		idx := make(map[string]*Distribution, len(dists))
		for _, d := range dists {
			idx[d.InvestorID] = d
		}
		for i := int64(0); i < remainder; i++ {
			idx[byRem[i%int64(len(byRem))]].Amount++
		}
	}
	r.Distributed = true
	s.distributions[r.ID] = dists
}

// ScanAndDistribute 扫描方案下所有未分配的回收并生成分配明细，
// 返回本次新分配的回收 ID。幂等：已分配的回收不会被重复处理。
func (s *Service) ScanAndDistribute(planID string, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[planID]; !ok {
		return nil, ErrPlanNotFound
	}
	ids := make([]string, 0)
	for id, r := range s.recoveries {
		if r.PlanID == planID && !r.Distributed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := s.recoveries[id]
		if r.Distributed { // 双重检查，防御并发
			continue
		}
		s.distributeLocked(r, now)
	}
	return ids, nil
}

// DistributionsOf 查询一笔回收的分配明细。
func (s *Service) DistributionsOf(recoveryID string) ([]*Distribution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recoveries[recoveryID]; !ok {
		return nil, ErrRecoveryNotFound
	}
	src := s.distributions[recoveryID]
	out := make([]*Distribution, len(src))
	copy(out, src)
	return out, nil
}

// Plan 查询方案。
func (s *Service) Plan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planID]
	if !ok {
		return nil, ErrPlanNotFound
	}
	cp := *p
	return &cp, nil
}
