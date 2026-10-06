package corporateaction

import (
	"fmt"
	"sync"
	"time"
)

// Service 提供公司行动登记、确认、执行、取消与持有人转换结果查询。
//
// 并发模型:行动级状态迁移(登记/确认/取消/执行启动与收尾)由服务锁串行化;
// 执行扫描逐条转换入账,入账键幂等。因此执行、取消与持仓变动并发时,
// 同一持有人只会生成一份最终转换结果,重复扫描不会再次增减份额。
type Service struct {
	store *Store

	mu  sync.Mutex // 行动状态迁移串行化
	now func() time.Time

	// applyHook 测试注入:执行扫描中每应用一条转换前回调,
	// 返回错误即模拟执行中断(行动停留在 EXECUTING)。
	applyHook func(actionID string, appliedInScan int) error
}

func NewService() *Service {
	return &Service{store: NewStore(), now: time.Now}
}

// NewServiceWithStore 允许替换持久化实现。
func NewServiceWithStore(st *Store) *Service {
	return &Service{store: st, now: time.Now}
}

// ---------------------------------------------------------------------------
// 持仓交易与份额冻结
// ---------------------------------------------------------------------------

// TradeCmd 申购(Shares 为正)/赎回(Shares 为负)交易。
type TradeCmd struct {
	TradeID    string
	FundID     string
	ShareClass string
	HolderID   string
	TradeDate  Date // 生效日,用于登记日快照归属判断
	Shares     Shares
}

// PostTrade 登记一笔申购/赎回交易。若该基金同一份额类型的某个行动
// 已冻结登记日,则拒绝生效日早于或等于该登记日的补登交易,保证快照
// 不被事后改变。
func (s *Service) PostTrade(cmd TradeCmd) error {
	if cmd.TradeID == "" || cmd.FundID == "" || cmd.ShareClass == "" ||
		cmd.HolderID == "" || cmd.Shares == 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store.hasPosting("trade:" + cmd.TradeID) {
		return ErrDuplicateTrade
	}
	if err := s.ensureNotBackdated(cmd.FundID, cmd.ShareClass, cmd.TradeDate); err != nil {
		return err
	}
	if cmd.Shares < 0 {
		total, _ := s.store.currentHoldings(cmd.FundID, cmd.ShareClass, cmd.HolderID)
		if total+cmd.Shares < 0 {
			return ErrInsufficientShares
		}
	}
	s.store.addPosting(Posting{
		Key: "trade:" + cmd.TradeID, FundID: cmd.FundID, ShareClass: cmd.ShareClass,
		HolderID: cmd.HolderID, Date: cmd.TradeDate,
		ShareDelta: cmd.Shares, Kind: PostingTrade,
	})
	return nil
}

// FreezeCmd 份额冻结(Delta 为正)/解冻(Delta 为负)。
// 冻结只改变冻结额,不改变持有总份额。
type FreezeCmd struct {
	RequestID  string
	FundID     string
	ShareClass string
	HolderID   string
	Date       Date // 生效日,用于登记日快照归属判断
	Delta      Shares
}

// PostFreeze 登记份额冻结/解冻。生效日归属与补登限制与交易相同。
func (s *Service) PostFreeze(cmd FreezeCmd) error {
	if cmd.RequestID == "" || cmd.FundID == "" || cmd.ShareClass == "" ||
		cmd.HolderID == "" || cmd.Delta == 0 {
		return ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store.hasPosting("freeze:" + cmd.RequestID) {
		return ErrDuplicateFreeze
	}
	if err := s.ensureNotBackdated(cmd.FundID, cmd.ShareClass, cmd.Date); err != nil {
		return err
	}
	total, frozen := s.store.currentHoldings(cmd.FundID, cmd.ShareClass, cmd.HolderID)
	if cmd.Delta > 0 && frozen+cmd.Delta > total {
		return ErrInsufficientShares
	}
	if cmd.Delta < 0 && frozen+cmd.Delta < 0 {
		return ErrInsufficientFreeze
	}
	s.store.addPosting(Posting{
		Key: "freeze:" + cmd.RequestID, FundID: cmd.FundID, ShareClass: cmd.ShareClass,
		HolderID: cmd.HolderID, Date: cmd.Date,
		FrozenDelta: cmd.Delta, Kind: PostingFreeze,
	})
	return nil
}

// ensureNotBackdated 拒绝补登生效日早于或等于任一已冻结登记日的流水
// (服务锁内调用)。
func (s *Service) ensureNotBackdated(fundID, shareClass string, date Date) error {
	for _, a := range s.store.listActions() {
		if a.FundID != fundID || a.ShareClass != shareClass ||
			a.Status == ActionDraft || a.Status == ActionCancelled {
			continue
		}
		if !date.After(a.RecordDate) {
			return fmt.Errorf("%w: 行动 %s 登记日 %s", ErrBackdatedEntry, a.ActionID, a.RecordDate)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 公司行动生命周期
// ---------------------------------------------------------------------------

// RegisterCmd 登记公司行动的参数。
type RegisterCmd struct {
	ActionID      string
	FundID        string
	ShareClass    string
	RecordDate    Date
	EffectiveDate Date
	Ratio         Ratio
}

// Register 登记公司行动(DRAFT)。
//   - 行动号重复但内容(基金、份额类型、登记日、生效日、比例)完全一致:
//     幂等返回已登记行动(比例按 GCD 归一化,如 2/1 与 4/2 视为相同);
//   - 行动号重复但任一关键内容不同:返回 ErrActionConflict;
//   - 同一基金同一份额类型在 [登记日, 生效日] 重叠期间已存在未取消行动:
//     返回 ErrOverlappingAction(登记日(含)前冻结资格、生效日转换份额,
//     窗口重叠意味着同一批份额可能被重复转换)。
func (s *Service) Register(cmd RegisterCmd) (*Action, error) {
	if cmd.ActionID == "" || cmd.FundID == "" || cmd.ShareClass == "" {
		return nil, ErrInvalidAmount
	}
	ratio, err := cmd.Ratio.Normalize()
	if err != nil {
		return nil, err
	}
	if cmd.EffectiveDate.Before(cmd.RecordDate) {
		return nil, ErrInvalidDate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.store.getAction(cmd.ActionID); ok {
		if sameContent(existing, cmd.FundID, cmd.ShareClass, cmd.RecordDate, cmd.EffectiveDate, ratio) {
			return existing, nil
		}
		return nil, fmt.Errorf("%w: 行动 %s 已存在但内容不一致", ErrActionConflict, cmd.ActionID)
	}
	for _, a := range s.store.listActions() {
		if a.Status == ActionCancelled {
			continue
		}
		if a.FundID == cmd.FundID && a.ShareClass == cmd.ShareClass &&
			windowsOverlap(a.RecordDate, a.EffectiveDate, cmd.RecordDate, cmd.EffectiveDate) {
			return nil, fmt.Errorf("%w: 与行动 %s(%s~%s)重叠",
				ErrOverlappingAction, a.ActionID, a.RecordDate, a.EffectiveDate)
		}
	}
	now := s.now()
	action := &Action{
		ActionID: cmd.ActionID, FundID: cmd.FundID, ShareClass: cmd.ShareClass,
		RecordDate: cmd.RecordDate, EffectiveDate: cmd.EffectiveDate,
		Ratio: ratio, Version: 1, Status: ActionDraft,
		CreatedAt: now, UpdatedAt: now,
	}
	s.store.putAction(action)
	s.store.addHistory(StatusChange{
		ActionID: action.ActionID, From: "", To: ActionDraft, Reason: "登记行动", At: now,
	})
	cp := *action
	return &cp, nil
}

func sameContent(a *Action, fundID, shareClass string, record, effective Date, ratio Ratio) bool {
	return a.FundID == fundID && a.ShareClass == shareClass &&
		a.RecordDate.Equal(record) && a.EffectiveDate.Equal(effective) &&
		a.Ratio == ratio
}

// windowsOverlap 判断两个闭区间 [s1,e1] 与 [s2,e2] 是否重叠。
func windowsOverlap(s1, e1, s2, e2 Date) bool {
	return !(e1.Before(s2) || e2.Before(s1))
}

// ConfirmAction 确认行动并冻结登记日日终持有人份额快照(含冻结份额)。
// 冻结后:
//   - 生效日早于或等于登记日的申购/赎回/冻结一律纳入快照;
//   - 生效日晚于登记日的交易(即使在转换生效日之前)不纳入,
//     其份额既不在本次注销范围,执行后也不会被重复转换;
//   - 补登生效日早于或等于登记日的流水被拒绝。
func (s *Service) ConfirmAction(actionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.store.getAction(actionID)
	if !ok {
		return ErrActionNotFound
	}
	if action.Status != ActionDraft {
		return fmt.Errorf("%w: 当前状态 %s", ErrInvalidStatus, action.Status)
	}
	totals := map[string]Shares{}
	frozens := map[string]Shares{}
	for _, p := range s.store.allPostings() {
		if p.FundID == action.FundID && p.ShareClass == action.ShareClass &&
			!p.Date.After(action.RecordDate) {
			totals[p.HolderID] += p.ShareDelta
			frozens[p.HolderID] += p.FrozenDelta
		}
	}
	now := s.now()
	var rows []SnapshotRow
	for holder, total := range totals {
		if total <= 0 {
			continue
		}
		rows = append(rows, SnapshotRow{
			ActionID: actionID, FundID: action.FundID, ShareClass: action.ShareClass,
			HolderID: holder, TotalShares: total, FrozenShares: frozens[holder],
			FrozenAt: now,
		})
	}
	s.store.saveSnapshot(actionID, rows)
	s.transition(action, ActionConfirmed, "确认行动,冻结登记日快照")
	return nil
}

// ExecuteRequest 执行请求。行动号相同且参数一致时幂等;
// 基金、份额类型、登记日、生效日或比例任一不一致返回 ErrExecutionConflict。
type ExecuteRequest struct {
	ActionID      string
	FundID        string
	ShareClass    string
	RecordDate    Date
	EffectiveDate Date
	Ratio         Ratio
}

func matchAction(a *Action, req ExecuteRequest) bool {
	r, err := req.Ratio.Normalize()
	if err != nil {
		return false
	}
	return a.FundID == req.FundID && a.ShareClass == req.ShareClass &&
		a.RecordDate.Equal(req.RecordDate) && a.EffectiveDate.Equal(req.EffectiveDate) &&
		a.Ratio == r
}

// Execute 执行公司行动。
//   - 已执行:返回原执行结果(幂等),不重复注销或发放;
//   - 执行中(曾中断):只补齐未完成转换,不重复入账;
//   - 已确认:按冻结快照生成全部转换明细后逐条入账。
func (s *Service) Execute(req ExecuteRequest) (*ExecutionResult, error) {
	s.mu.Lock()
	action, ok := s.store.getAction(req.ActionID)
	if !ok {
		s.mu.Unlock()
		return nil, ErrActionNotFound
	}
	if !matchAction(action, req) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: 行动 %s", ErrExecutionConflict, req.ActionID)
	}
	switch action.Status {
	case ActionDraft:
		s.mu.Unlock()
		return nil, ErrActionNotConfirmed
	case ActionCancelled:
		s.mu.Unlock()
		return nil, ErrActionCancelled
	case ActionExecuted:
		res, _ := s.store.executionOf(req.ActionID)
		s.mu.Unlock()
		return res, nil
	case ActionConfirmed:
		convs := s.buildConversions(action)
		s.store.saveConversions(convs)
		s.transition(action, ActionExecuting, "开始执行,生成转换明细")
	case ActionExecuting:
		// 中断恢复:沿用既有明细继续扫描。
	}
	s.mu.Unlock()

	// 逐条应用未完成转换;入账键幂等,允许并发/重复扫描。
	for i, holderID := range s.store.pendingHolders(req.ActionID) {
		if s.applyHook != nil {
			if err := s.applyHook(req.ActionID, i); err != nil {
				return nil, err
			}
		}
		conv := s.conversionFor(req.ActionID, holderID)
		if conv == nil {
			continue
		}
		s.store.applyConversion(conv, action.EffectiveDate, s.now())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	action, _ = s.store.getAction(req.ActionID)
	if action.Status == ActionExecuted {
		res, _ := s.store.executionOf(req.ActionID)
		return res, nil
	}
	res := s.summarize(action)
	s.store.saveExecution(res)
	s.transition(action, ActionExecuted, "执行完成")
	return res, nil
}

// conversionFor 取某持有人的转换明细副本(供逐条入账使用)。
func (s *Service) conversionFor(actionID, holderID string) *Conversion {
	for _, c := range s.store.conversionsOf(actionID) {
		if c.HolderID == holderID {
			cp := c
			return &cp
		}
	}
	return nil
}

// buildConversions 依据冻结快照与行动版本生成转换明细(服务锁内调用)。
func (s *Service) buildConversions(action *Action) []*Conversion {
	var out []*Conversion
	for _, row := range s.store.snapshot(action.ActionID) {
		after, fractional := convertShares(row.TotalShares, action.Ratio)
		afterFrozen := convertFrozen(row.FrozenShares, action.Ratio)
		if afterFrozen > after {
			afterFrozen = after
		}
		out = append(out, &Conversion{
			ActionID: action.ActionID, FundID: action.FundID, ShareClass: action.ShareClass,
			HolderID: row.HolderID, ActionVersion: action.Version,
			BeforeShares: row.TotalShares, BeforeFrozen: row.FrozenShares,
			AfterShares:      after,
			AfterFrozen:      afterFrozen,
			FractionalShares: fractional,
			CashInLieu:       cashInLieuFor(fractional),
			Status:           ConversionPending,
		})
	}
	return out
}

// summarize 汇总执行结果(服务锁内调用)。
func (s *Service) summarize(action *Action) *ExecutionResult {
	res := &ExecutionResult{
		ActionID: action.ActionID, Status: ActionExecuted,
		Direction: action.Ratio.Direction(), FinishedAt: s.now(),
	}
	for _, c := range s.store.conversionsOf(action.ActionID) {
		res.HolderCount++
		res.TotalBefore += c.BeforeShares
		res.TotalAfter += c.AfterShares
		res.TotalFraction += c.FractionalShares
		res.TotalCashInLieu += c.CashInLieu
	}
	return res
}

// CancelAction 取消行动。仅允许在执行开始前(DRAFT/CONFIRMED)取消;
// 取消后登记快照仍可查询,重复取消幂等返回成功。
func (s *Service) CancelAction(actionID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.store.getAction(actionID)
	if !ok {
		return ErrActionNotFound
	}
	switch action.Status {
	case ActionCancelled:
		return nil
	case ActionDraft, ActionConfirmed:
		s.transition(action, ActionCancelled, "取消行动: "+reason)
		return nil
	default:
		return fmt.Errorf("%w: 当前状态 %s", ErrNotCancellable, action.Status)
	}
}

// transition 迁移行动状态并持久化状态变化(服务锁内调用)。
func (s *Service) transition(action *Action, to ActionStatus, reason string) {
	from := action.Status
	action.Status = to
	action.UpdatedAt = s.now()
	s.store.putAction(action)
	s.store.addHistory(StatusChange{
		ActionID: action.ActionID, From: from, To: to, Reason: reason, At: action.UpdatedAt,
	})
}

// ---------------------------------------------------------------------------
// 查询接口
// ---------------------------------------------------------------------------

// GetAction 查询行动定义。
func (s *Service) GetAction(actionID string) (*Action, error) {
	action, ok := s.store.getAction(actionID)
	if !ok {
		return nil, ErrActionNotFound
	}
	return action, nil
}

// Snapshot 查询登记日冻结的持有人份额快照。
func (s *Service) Snapshot(actionID string) ([]SnapshotRow, error) {
	if !s.store.actionExists(actionID) {
		return nil, ErrActionNotFound
	}
	return s.store.snapshot(actionID), nil
}

// Conversions 查询行动的全部持有人转换明细;
// 每条明细可追溯原持有人、转换前数量、零碎差额与行动版本。
func (s *Service) Conversions(actionID string) ([]Conversion, error) {
	if !s.store.actionExists(actionID) {
		return nil, ErrActionNotFound
	}
	return s.store.conversionsOf(actionID), nil
}

// HolderConversion 查询单个持有人在某行动下的转换结果。
func (s *Service) HolderConversion(actionID, holderID string) (*Conversion, error) {
	if !s.store.actionExists(actionID) {
		return nil, ErrActionNotFound
	}
	if c := s.conversionFor(actionID, holderID); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("%w: 持有人 %s 无转换明细", ErrActionNotFound, holderID)
}

// Execution 查询执行结果;行动未执行完成时返回 ErrInvalidStatus。
func (s *Service) Execution(actionID string) (*ExecutionResult, error) {
	if !s.store.actionExists(actionID) {
		return nil, ErrActionNotFound
	}
	res, ok := s.store.executionOf(actionID)
	if !ok {
		return nil, fmt.Errorf("%w: 行动尚未执行完成", ErrInvalidStatus)
	}
	return res, nil
}

// StatusHistory 查询行动状态变化历史。
func (s *Service) StatusHistory(actionID string) ([]StatusChange, error) {
	if !s.store.actionExists(actionID) {
		return nil, ErrActionNotFound
	}
	return s.store.historyOf(actionID), nil
}

// Holdings 查询持有人截至某日(含)的总份额与冻结份额。
func (s *Service) Holdings(fundID, shareClass, holderID string, asOf Date) (total, frozen Shares) {
	return s.store.holdings(fundID, shareClass, holderID, asOf)
}

// CashBalance 查询持有人零碎份额现金替代余额。
func (s *Service) CashBalance(fundID, shareClass, holderID string) Money {
	return s.store.cashBalance(fundID, shareClass, holderID)
}
