package sidepocket

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrPlanNotFound     = errors.New("sidepocket: plan not found")
	ErrPlanNotDraft     = errors.New("sidepocket: plan is not in draft status")
	ErrPlanNotConfirmed = errors.New("sidepocket: plan is not confirmed")
	ErrRecoveryNotFound = errors.New("sidepocket: recovery not found")
	ErrInvalidRatio     = errors.New("sidepocket: invalid freeze ratio")
	ErrInvalidAmount    = errors.New("sidepocket: amount must be positive")
	ErrInsufficient     = errors.New("sidepocket: insufficient balance")
)

// Service 提供侧袋方案、权益、回收与分配的并发安全操作。
// 所有状态变更在同一把互斥锁下完成，保证权益转移、回收确认
// 与分配扫描并发时，每一份权益只归入一名最终持有人。
type Service struct {
	mu            sync.Mutex
	plans         map[string]*Plan
	holdings      map[string]map[string]*Holding // fundID -> investorID
	entitlements  map[string]map[string]int64    // planID -> investorID -> units
	recoveries    map[string]*Recovery
	distributions map[string][]DistributionDetail // recoveryID -> details
}

func NewService() *Service {
	return &Service{
		plans:         make(map[string]*Plan),
		holdings:      make(map[string]map[string]*Holding),
		entitlements:  make(map[string]map[string]int64),
		recoveries:    make(map[string]*Recovery),
		distributions: make(map[string][]DistributionDetail),
	}
}

// CreatePlan 创建 DRAFT 状态的侧袋方案。
func (s *Service) CreatePlan(id, fundID, assetID string, recordDate time.Time, valuation, ratioNum, ratioDen int64, version int) (*Plan, error) {
	if ratioNum <= 0 || ratioDen <= 0 || ratioNum > ratioDen {
		return nil, ErrInvalidRatio
	}
	if valuation <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.plans[id]; exists {
		return nil, fmt.Errorf("sidepocket: plan %s already exists", id)
	}
	p := &Plan{
		ID:         id,
		FundID:     fundID,
		AssetID:    assetID,
		RecordDate: recordDate,
		Valuation:  valuation,
		RatioNum:   ratioNum,
		RatioDen:   ratioDen,
		Version:    version,
		Status:     PlanStatusDraft,
	}
	s.plans[id] = p
	return p, nil
}

// Subscribe 登记投资者的主基金份额申购，份额自 effectiveDate 起生效。
func (s *Service) Subscribe(fundID, investorID string, shares int64, effectiveDate time.Time) error {
	if shares <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fund := s.holdings[fundID]
	if fund == nil {
		fund = make(map[string]*Holding)
		s.holdings[fundID] = fund
	}
	h := fund[investorID]
	if h == nil {
		fund[investorID] = &Holding{FundID: fundID, InvestorID: investorID, Shares: shares, EffectiveDate: effectiveDate}
		return nil
	}
	if effectiveDate.Before(h.EffectiveDate) {
		h.EffectiveDate = effectiveDate
	}
	h.Shares += shares
	return nil
}

// Redeem 赎回主基金份额。侧袋权益不受赎回影响，继续保留在投资者名下。
func (s *Service) Redeem(fundID, investorID string, shares int64) error {
	if shares <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holdings[fundID][investorID]
	if h == nil || h.Shares < shares {
		return ErrInsufficient
	}
	h.Shares -= shares
	return nil
}

// MainShares 查询投资者当前主基金份额（可赎回数量只按主份额计算）。
func (s *Service) MainShares(fundID, investorID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holdings[fundID][investorID]
	if h == nil {
		return 0
	}
	return h.Shares
}

// ConfirmPlan 确认方案：以登记日（含）前生效的主份额为基数，按冻结比例
// 生成侧袋权益。所有投资者权益合计通过最大余数法调整，必须等于方案总量。
func (s *Service) ConfirmPlan(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[planID]
	if p == nil {
		return ErrPlanNotFound
	}
	if p.Status != PlanStatusDraft {
		return ErrPlanNotDraft
	}

	type frozen struct {
		investorID string
		shares     int64
	}
	var frozenList []frozen
	var totalFrozen int64
	for investorID, h := range s.holdings[p.FundID] {
		// 登记日边界：生效日期不晚于登记日的份额参与划分。
		if h.EffectiveDate.After(p.RecordDate) || h.Shares <= 0 {
			continue
		}
		fs := h.Shares * p.RatioNum / p.RatioDen
		if fs <= 0 {
			continue
		}
		frozenList = append(frozenList, frozen{investorID: investorID, shares: fs})
		totalFrozen += fs
	}
	sort.Slice(frozenList, func(i, j int) bool { return frozenList[i].investorID < frozenList[j].investorID })

	ents := make(map[string]int64)
	if totalFrozen > 0 {
		type remainder struct {
			investorID string
			rem        int64
		}
		var rems []remainder
		var allocated int64
		for _, f := range frozenList {
			exact := f.shares * p.Valuation
			units := exact / totalFrozen
			ents[f.investorID] = units
			allocated += units
			rems = append(rems, remainder{investorID: f.investorID, rem: exact % totalFrozen})
		}
		// 最大余数法补齐差额，保证合计与方案总量一致。
		sort.Slice(rems, func(i, j int) bool {
			if rems[i].rem != rems[j].rem {
				return rems[i].rem > rems[j].rem
			}
			return rems[i].investorID < rems[j].investorID
		})
		for i := int64(0); i < p.Valuation-allocated; i++ {
			ents[rems[i%int64(len(rems))].investorID]++
		}
	}

	s.entitlements[planID] = ents
	p.Status = PlanStatusConfirmed
	var total int64
	for _, u := range ents {
		total += u
	}
	p.TotalEntitlement = total
	if total != p.Valuation && totalFrozen > 0 {
		return fmt.Errorf("sidepocket: entitlement total %d does not match plan valuation %d", total, p.Valuation)
	}
	return nil
}

// EntitlementOf 查询投资者在某方案下的侧袋权益。
func (s *Service) EntitlementOf(planID, investorID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entitlements[planID][investorID]
}

// TransferEntitlement 在投资者账户之间转移侧袋权益。
// 转移在锁内原子完成，并发转移下总量守恒且不会出现负权益。
func (s *Service) TransferEntitlement(planID, fromInvestorID, toInvestorID string, units int64) error {
	if units <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[planID]
	if p == nil {
		return ErrPlanNotFound
	}
	if p.Status != PlanStatusConfirmed {
		return ErrPlanNotConfirmed
	}
	ents := s.entitlements[planID]
	if ents[fromInvestorID] < units {
		return ErrInsufficient
	}
	ents[fromInvestorID] -= units
	ents[toInvestorID] += units
	return nil
}

// RecoverCash 登记目标资产的一次现金回收。
func (s *Service) RecoverCash(planID, recoveryID string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[planID]
	if p == nil {
		return ErrPlanNotFound
	}
	if p.Status != PlanStatusConfirmed {
		return ErrPlanNotConfirmed
	}
	if _, exists := s.recoveries[recoveryID]; exists {
		return fmt.Errorf("sidepocket: recovery %s already exists", recoveryID)
	}
	s.recoveries[recoveryID] = &Recovery{ID: recoveryID, PlanID: planID, Amount: amount}
	return nil
}

// ScanDistributions 按当前有效侧袋权益为回收生成分配明细。
// 扫描幂等：并发或重复扫描返回同一份明细，不会重复分配。
func (s *Service) ScanDistributions(recoveryID string) ([]DistributionDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recoveries[recoveryID]
	if r == nil {
		return nil, ErrRecoveryNotFound
	}
	if r.Scanned {
		return append([]DistributionDetail(nil), s.distributions[recoveryID]...), nil
	}
	ents := s.entitlements[r.PlanID]
	var total int64
	for _, u := range ents {
		total += u
	}
	var details []DistributionDetail
	if total > 0 {
		investors := make([]string, 0, len(ents))
		for investorID, u := range ents {
			if u > 0 {
				investors = append(investors, investorID)
			}
		}
		sort.Strings(investors)
		type remainder struct {
			investorID string
			rem        int64
		}
		var rems []remainder
		var allocated int64
		amounts := make(map[string]int64, len(investors))
		for _, investorID := range investors {
			exact := ents[investorID] * r.Amount
			amt := exact / total
			amounts[investorID] = amt
			allocated += amt
			rems = append(rems, remainder{investorID: investorID, rem: exact % total})
		}
		sort.Slice(rems, func(i, j int) bool {
			if rems[i].rem != rems[j].rem {
				return rems[i].rem > rems[j].rem
			}
			return rems[i].investorID < rems[j].investorID
		})
		for i := int64(0); i < r.Amount-allocated; i++ {
			amounts[rems[i%int64(len(rems))].investorID]++
		}
		for _, investorID := range investors {
			details = append(details, DistributionDetail{
				RecoveryID: recoveryID,
				PlanID:     r.PlanID,
				InvestorID: investorID,
				Units:      ents[investorID],
				Amount:     amounts[investorID],
			})
		}
	}
	s.distributions[recoveryID] = details
	r.Scanned = true
	return append([]DistributionDetail(nil), details...), nil
}

// DistributionDetails 查询某次回收已生成的分配明细。
func (s *Service) DistributionDetails(recoveryID string) ([]DistributionDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recoveries[recoveryID]; !ok {
		return nil, ErrRecoveryNotFound
	}
	return append([]DistributionDetail(nil), s.distributions[recoveryID]...), nil
}

// Plan 查询方案。
func (s *Service) Plan(planID string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[planID]
	if p == nil {
		return nil, ErrPlanNotFound
	}
	cp := *p
	return &cp, nil
}
