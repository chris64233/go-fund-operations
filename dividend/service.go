package dividend

import (
	"fmt"
	"sync"
	"time"
)

// Service 提供分红方案创建、持有人选择、方案确认、执行、取消、
// 更正与查询的完整接口。
//
// 并发模型:方案级状态迁移(确认/取消/执行启动与收尾)由服务锁串行化;
// 执行扫描逐条明细入账,入账键幂等,因此方案操作、持仓变更与执行扫描
// 并发时,每名持有人只会生成并应用一份最终分配。
type Service struct {
	store *Store

	mu  sync.Mutex // 方案状态迁移串行化
	now func() time.Time

	// applyHook 测试注入:执行扫描中每应用一条明细前回调,
	// 返回错误即模拟执行中断(方案停留在 EXECUTING)。
	applyHook func(planID string, appliedInScan int) error
}

// NewService 创建使用内存存储的服务。
func NewService() *Service {
	return &Service{store: NewStore(), now: time.Now}
}

// NewServiceWithStore 允许替换持久化实现。
func NewServiceWithStore(st *Store) *Service {
	return &Service{store: st, now: time.Now}
}

// ---------------------------------------------------------------------------
// 持仓交易
// ---------------------------------------------------------------------------

// TradeCmd 申购(正)/赎回(负)交易。
type TradeCmd struct {
	TradeID   string
	FundID    string
	HolderID  string
	TradeDate Date
	Shares    Shares
}

// PostTrade 登记一笔持仓交易。若该基金的某个未取消方案已冻结登记日,
// 则拒绝交易日早于等于该登记日的补登交易,保证快照不被事后改变。
func (s *Service) PostTrade(cmd TradeCmd) error {
	if cmd.TradeID == "" || cmd.FundID == "" || cmd.HolderID == "" || cmd.Shares == 0 {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store.hasPosting("trade:" + cmd.TradeID) {
		return ErrDuplicateTrade
	}
	for _, p := range s.store.listPlans() {
		if p.FundID != cmd.FundID || p.Status == PlanDraft || p.Status == PlanCancelled {
			continue
		}
		if !cmd.TradeDate.After(p.RecordDate) {
			return fmt.Errorf("%w: 方案 %s 登记日 %s", ErrBackdatedTrade, p.PlanID, p.RecordDate)
		}
	}
	if cmd.Shares < 0 && s.store.totalHoldings(cmd.FundID, cmd.HolderID)+cmd.Shares < 0 {
		return ErrInsufficientShares
	}
	s.store.addPosting(Posting{
		Key: "trade:" + cmd.TradeID, FundID: cmd.FundID, HolderID: cmd.HolderID,
		Date: cmd.TradeDate, ShareDelta: cmd.Shares, Kind: PostingTrade, Ref: cmd.TradeID,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 方案生命周期
// ---------------------------------------------------------------------------

// CreatePlanCmd 创建分红方案的参数。
type CreatePlanCmd struct {
	PlanID           string
	FundID           string
	RecordDate       Date
	ExDate           Date
	DividendPerShare UnitAmount
	ExNav            UnitAmount
}

// CreatePlan 创建分红方案(DRAFT)。方案号重复时返回 ErrPlanExists。
func (s *Service) CreatePlan(cmd CreatePlanCmd) (*Plan, error) {
	if cmd.PlanID == "" || cmd.FundID == "" || cmd.DividendPerShare <= 0 || cmd.ExNav <= 0 {
		return nil, ErrInvalidArgument
	}
	if cmd.ExDate.Before(cmd.RecordDate) {
		return nil, fmt.Errorf("%w: 除息日早于登记日", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.planExists(cmd.PlanID) {
		return nil, ErrPlanExists
	}
	now := s.now()
	plan := Plan{
		PlanID: cmd.PlanID, FundID: cmd.FundID,
		RecordDate: cmd.RecordDate, ExDate: cmd.ExDate,
		DividendPerShare: cmd.DividendPerShare, ExNav: cmd.ExNav,
		Status: PlanDraft, CreatedAt: now, UpdatedAt: now,
	}
	s.store.putPlan(plan)
	s.store.addHistory(StatusChange{PlanID: plan.PlanID, From: "", To: PlanDraft, Reason: "创建方案", At: now})
	return &plan, nil
}

// SetChoice 设置/修改持有人的领取方式。仅允许在方案执行开始前
// (DRAFT/CONFIRMED)修改;执行开始后返回 ErrChoiceLocked。
func (s *Service) SetChoice(planID, holderID string, method PayoutMethod) error {
	if holderID == "" || (method != PayoutCash && method != PayoutReinvest) {
		return ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.store.getPlan(planID)
	if !ok {
		return ErrPlanNotFound
	}
	if plan.Status != PlanDraft && plan.Status != PlanConfirmed {
		return ErrChoiceLocked
	}
	s.store.setChoice(Choice{PlanID: planID, HolderID: holderID, Method: method, UpdatedAt: s.now()})
	return nil
}

// ConfirmPlan 确认方案并冻结登记日持有人份额快照。
// 冻结后,登记日之后的交易不影响本次分红资格,
// 登记日及之前的补登交易被拒绝。
func (s *Service) ConfirmPlan(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.store.getPlan(planID)
	if !ok {
		return ErrPlanNotFound
	}
	if plan.Status != PlanDraft {
		return fmt.Errorf("%w: 当前状态 %s", ErrInvalidStatus, plan.Status)
	}
	// 按交易日 <= 登记日聚合持有人份额。
	totals := map[string]Shares{}
	for _, posting := range s.store.allPostings() {
		if posting.FundID == plan.FundID && !posting.Date.After(plan.RecordDate) {
			totals[posting.HolderID] += posting.ShareDelta
		}
	}
	now := s.now()
	var rows []SnapshotRow
	for holderID, shares := range totals {
		if shares > 0 {
			rows = append(rows, SnapshotRow{
				PlanID: planID, FundID: plan.FundID, HolderID: holderID,
				Shares: shares, FrozenAt: now,
			})
		}
	}
	s.store.saveSnapshot(planID, rows)
	s.transition(&plan, PlanConfirmed, "确认方案,冻结登记日快照")
	return nil
}

// ExecuteRequest 执行请求。方案号相同且参数一致时幂等;
// 基金、登记日、每份分红或除息净值任一不一致时返回 ErrExecutionConflict。
type ExecuteRequest struct {
	PlanID           string
	FundID           string
	RecordDate       Date
	DividendPerShare UnitAmount
	ExNav            UnitAmount
}

func matchPlan(plan Plan, req ExecuteRequest) bool {
	return plan.FundID == req.FundID &&
		plan.RecordDate.Equal(req.RecordDate) &&
		plan.DividendPerShare == req.DividendPerShare &&
		plan.ExNav == req.ExNav
}

// Execute 执行分红方案。
//   - 已执行:返回原执行结果(幂等);
//   - 执行中(曾中断):只补齐未完成明细,不重复发放;
//   - 已确认:生成全部分配明细后逐条入账。
func (s *Service) Execute(req ExecuteRequest) (*ExecutionResult, error) {
	s.mu.Lock()
	plan, ok := s.store.getPlan(req.PlanID)
	if !ok {
		s.mu.Unlock()
		return nil, ErrPlanNotFound
	}
	if !matchPlan(plan, req) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: 方案 %s", ErrExecutionConflict, req.PlanID)
	}
	switch plan.Status {
	case PlanDraft:
		s.mu.Unlock()
		return nil, ErrPlanNotConfirmed
	case PlanCancelled:
		s.mu.Unlock()
		return nil, ErrPlanCancelled
	case PlanExecuted:
		res, _ := s.store.executionOf(req.PlanID)
		s.mu.Unlock()
		return &res, nil
	case PlanConfirmed:
		// 生成每名持有人的唯一分配明细,随后进入 EXECUTING。
		s.store.saveAllocations(s.buildAllocations(plan))
		s.transition(&plan, PlanExecuting, "开始执行,生成分配明细")
	case PlanExecuting:
		// 中断恢复:沿用既有明细继续扫描。
	}
	exDate := plan.ExDate
	s.mu.Unlock()

	// 逐条应用未完成明细;入账幂等,允许并发扫描。
	for i, holderID := range s.store.pendingAllocations(req.PlanID) {
		if s.applyHook != nil {
			if err := s.applyHook(req.PlanID, i); err != nil {
				return nil, err
			}
		}
		s.store.applyAllocation(req.PlanID, holderID, exDate, s.now())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok = s.store.getPlan(req.PlanID)
	if !ok {
		return nil, ErrPlanNotFound
	}
	if plan.Status == PlanExecuted {
		// 与本次扫描并发的另一轮执行已收尾,返回其结果。
		res, _ := s.store.executionOf(req.PlanID)
		return &res, nil
	}
	res := s.summarize(req.PlanID)
	s.store.saveExecution(res)
	s.transition(&plan, PlanExecuted, "执行完成")
	return &res, nil
}

// buildAllocations 依据冻结快照与领取选择生成明细(服务锁内调用)。
func (s *Service) buildAllocations(plan Plan) []Allocation {
	var out []Allocation
	for _, row := range s.store.snapshot(plan.PlanID) {
		method := PayoutCash
		if c, ok := s.store.choiceOf(plan.PlanID, row.HolderID); ok {
			method = c.Method
		}
		entitlement := cashEntitlement(row.Shares, plan.DividendPerShare)
		a := Allocation{
			PlanID: plan.PlanID, FundID: plan.FundID, HolderID: row.HolderID,
			Method: method, BaseShares: row.Shares, Entitlement: entitlement,
			Status: AllocationPending,
		}
		if method == PayoutReinvest {
			a.ReinvestShares = reinvestShares(entitlement, plan.ExNav)
			// 折算差额以现金补齐,保证明细之和等于方案总额。
			a.CashAmount = entitlement - sharesValue(a.ReinvestShares, plan.ExNav)
		} else {
			a.CashAmount = entitlement
		}
		out = append(out, a)
	}
	return out
}

// summarize 汇总执行结果(服务锁内调用)。
func (s *Service) summarize(planID string) ExecutionResult {
	res := ExecutionResult{PlanID: planID, Status: PlanExecuted, FinishedAt: s.now()}
	for _, a := range s.store.allocationsOf(planID) {
		res.HolderCount++
		res.TotalEntitlement += a.Entitlement
		res.TotalCash += a.CashAmount
		res.TotalReinvestShares += a.ReinvestShares
	}
	return res
}

// CancelPlan 取消方案。仅允许在执行开始前(DRAFT/CONFIRMED)取消;
// 取消后登记快照与选择记录仍可查询。重复取消幂等返回成功。
func (s *Service) CancelPlan(planID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.store.getPlan(planID)
	if !ok {
		return ErrPlanNotFound
	}
	switch plan.Status {
	case PlanCancelled:
		return nil
	case PlanDraft, PlanConfirmed:
		s.transition(&plan, PlanCancelled, "取消方案: "+reason)
		return nil
	default:
		return fmt.Errorf("%w: 当前状态 %s", ErrNotCancellable, plan.Status)
	}
}

// PostCorrection 对已执行的方案登记独立更正记录并入账。
// 既有分配明细不被修改或删除。
func (s *Service) PostCorrection(c Correction) (*Correction, error) {
	if c.CorrectionID == "" || c.HolderID == "" {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.store.getPlan(c.PlanID)
	if !ok {
		return nil, ErrPlanNotFound
	}
	if plan.Status != PlanExecuted {
		return nil, fmt.Errorf("%w: 仅已执行方案可登记更正", ErrInvalidStatus)
	}
	if s.store.hasCorrection(c.CorrectionID) {
		return nil, ErrDuplicateCorrection
	}
	if c.ShareDelta < 0 && s.store.totalHoldings(plan.FundID, c.HolderID)+c.ShareDelta < 0 {
		return nil, ErrInsufficientShares
	}
	c.CreatedAt = s.now()
	s.store.addCorrection(c, Posting{
		Key: "corr:" + c.CorrectionID, FundID: plan.FundID, HolderID: c.HolderID,
		Date: plan.ExDate, ShareDelta: c.ShareDelta, CashDelta: c.CashDelta,
		Kind: PostingCorrection, Ref: c.PlanID,
	})
	return &c, nil
}

// transition 迁移方案状态并持久化状态变化(服务锁内调用)。
func (s *Service) transition(plan *Plan, to PlanStatus, reason string) {
	from := plan.Status
	plan.Status = to
	plan.UpdatedAt = s.now()
	s.store.putPlan(*plan)
	s.store.addHistory(StatusChange{PlanID: plan.PlanID, From: from, To: to, Reason: reason, At: plan.UpdatedAt})
}

// ---------------------------------------------------------------------------
// 查询接口
// ---------------------------------------------------------------------------

// GetPlan 查询方案。
func (s *Service) GetPlan(planID string) (*Plan, error) {
	plan, ok := s.store.getPlan(planID)
	if !ok {
		return nil, ErrPlanNotFound
	}
	return &plan, nil
}

// Snapshot 查询登记日冻结快照(取消后仍可查询)。
func (s *Service) Snapshot(planID string) ([]SnapshotRow, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	return s.store.snapshot(planID), nil
}

// Choices 查询持有人领取方式选择记录。
func (s *Service) Choices(planID string) ([]Choice, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	return s.store.choicesOf(planID), nil
}

// Allocations 查询分配明细。
func (s *Service) Allocations(planID string) ([]Allocation, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	return s.store.allocationsOf(planID), nil
}

// Execution 查询执行结果;方案尚未执行完成时返回 ErrInvalidStatus。
func (s *Service) Execution(planID string) (*ExecutionResult, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	res, ok := s.store.executionOf(planID)
	if !ok {
		return nil, fmt.Errorf("%w: 方案尚未执行完成", ErrInvalidStatus)
	}
	return &res, nil
}

// StatusHistory 查询方案状态变化历史。
func (s *Service) StatusHistory(planID string) ([]StatusChange, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	return s.store.historyOf(planID), nil
}

// Corrections 查询方案的更正记录。
func (s *Service) Corrections(planID string) ([]Correction, error) {
	if !s.store.planExists(planID) {
		return nil, ErrPlanNotFound
	}
	return s.store.correctionsOf(planID), nil
}

// Holdings 查询持有人截至某日(含)的份额。
func (s *Service) Holdings(fundID, holderID string, asOf Date) Shares {
	return s.store.holdings(fundID, holderID, asOf)
}

// CashBalance 查询持有人现金余额。
func (s *Service) CashBalance(fundID, holderID string) Money {
	return s.store.cashBalance(fundID, holderID)
}
