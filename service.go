package fundoperations

import (
	"sync"
	"time"
)

// Service 开放式基金申购受理与估值日批处理服务。
// 所有导出方法均加全局互斥锁，保证批次关闭、净值确认与
// 重复请求并发时，一笔申请只得到一个最终结果。
type Service struct {
	mu    sync.Mutex
	state *State
	clock Clock
	store Persister
}

// NewService 构造内存态 Service。clock 为 nil 时使用系统时钟，
// p 为 nil 时不持久化。
func NewService(clock Clock, p Persister) *Service {
	if p == nil {
		p = NoopPersister{}
	}
	return newService(newState(), clock, p)
}

func newService(state *State, clock Clock, p Persister) *Service {
	if clock == nil {
		clock = SystemClock{}
	}
	state.normalize()
	return &Service{state: state, clock: clock, store: p}
}

func (s *Service) persist() error { return s.store.Save(s.state) }

// RegisterFund 登记基金产品。
func (s *Service) RegisterFund(f Fund) error {
	if f.ID == "" || f.Currency == "" || f.Calendar == nil || f.CutoffTime == "" {
		return ErrInvalidRequest
	}
	if _, err := time.Parse("15:04", f.CutoffTime); err != nil {
		return ErrInvalidRequest
	}
	if f.Status == "" {
		f.Status = FundStatusTrading
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Funds[f.ID] = &f
	return s.persist()
}

// SetFundStatus 切换基金交易状态（正常/暂停）。
func (s *Service) SetFundStatus(fundID string, status FundStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.state.Funds[fundID]
	if !ok {
		return ErrFundNotFound
	}
	f.Status = status
	return s.persist()
}

// PublishNAV 发布某估值日净值，返回新版本号；净值更正会产生递增的新版本。
func (s *Service) PublishNAV(fundID, date string, value int64) (NAV, error) {
	if value <= 0 {
		return NAV{}, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Funds[fundID]; !ok {
		return NAV{}, ErrFundNotFound
	}
	versions := s.state.NAVs[fundID]
	nav := NAV{
		FundID:      fundID,
		Date:        date,
		Value:       value,
		Version:     len(versions[date]) + 1,
		PublishedAt: s.clock.Now(),
	}
	if versions == nil {
		versions = make(map[string][]NAV)
		s.state.NAVs[fundID] = versions
	}
	versions[date] = append(versions[date], nav)
	return nav, s.persist()
}

// Subscribe 受理申购申请。校验交易状态、币种与申购起点，
// 按统一时钟与每日截止时间把申请归入对应估值日批次。
// 相同外部申请号重复提交：内容一致返回原申请（含已确认结果），
// 内容不一致返回 ErrConflict。
func (s *Service) Subscribe(req SubscriptionRequest) (Application, error) {
	if req.FundID == "" || req.ExternalID == "" || req.InvestorID == "" || req.Amount <= 0 {
		return Application{}, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.state.Funds[req.FundID]
	if !ok {
		return Application{}, ErrFundNotFound
	}

	// 幂等：外部申请号已存在时，内容一致返回原申请，否则冲突。
	if app, ok := s.state.Applications[req.FundID][req.ExternalID]; ok {
		if app.InvestorID == req.InvestorID && app.Amount == req.Amount && app.Currency == req.Currency {
			return *app, nil
		}
		return Application{}, ErrConflict
	}

	if f.Status != FundStatusTrading {
		return Application{}, ErrFundSuspended
	}
	if req.Currency != f.Currency {
		return Application{}, ErrCurrencyMismatch
	}
	if req.Amount < f.MinSubscription {
		return Application{}, ErrBelowMinimum
	}

	submitted := req.SubmittedAt
	if submitted.IsZero() {
		submitted = s.clock.Now()
	}
	date, err := valuationDate(f, submitted)
	if err != nil {
		return Application{}, err
	}

	batch := s.getOrCreateBatch(f.ID, date)
	if batch.Status != BatchOpen {
		// 已关闭的批次不能被迟到申请重新打开。
		return Application{}, ErrBatchClosed
	}

	app := &Application{
		FundID:        f.ID,
		ExternalID:    req.ExternalID,
		InvestorID:    req.InvestorID,
		Amount:        req.Amount,
		Currency:      req.Currency,
		SubmittedAt:   submitted,
		ValuationDate: date,
		BatchID:       batch.ID,
		Status:        StatusPending,
	}
	apps := s.state.Applications[f.ID]
	if apps == nil {
		apps = make(map[string]*Application)
		s.state.Applications[f.ID] = apps
	}
	apps[app.ExternalID] = app
	batch.ExternalIDs = append(batch.ExternalIDs, app.ExternalID)
	batch.TotalAmount += app.Amount
	return *app, s.persist()
}

// CloseBatch 关闭批次：冻结申请集合、估值日与当前最新净值版本。
// 重复关闭是幂等的，返回既有批次快照。
func (s *Service) CloseBatch(fundID, date string) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	batch, err := s.findBatch(fundID, date)
	if err != nil {
		return Batch{}, err
	}
	if batch.Status != BatchOpen {
		return copyBatch(batch), nil
	}
	nav, err := s.latestNAV(fundID, date)
	if err != nil {
		return Batch{}, err
	}
	batch.Status = BatchClosed
	batch.NAV = nav.Value
	batch.NAVVersion = nav.Version
	batch.ClosedAt = s.clock.Now()
	return copyBatch(batch), s.persist()
}

// ConfirmBatch 按批次冻结的净值确认份额。金额精确到分、净值精确到
// 0.0001 元、份额精确到 0.01 份，一律向下舍入，舍入差额留存基金资产
// 并逐笔记录。重复确认是幂等的；净值后续更正不改写已确认结果。
func (s *Service) ConfirmBatch(fundID, date string) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	batch, err := s.findBatch(fundID, date)
	if err != nil {
		return Batch{}, err
	}
	switch batch.Status {
	case BatchConfirmed:
		return copyBatch(batch), nil
	case BatchOpen:
		return Batch{}, ErrBatchNotClosed
	}

	apps := s.state.Applications[fundID]
	var totalShares, totalConfirmed, totalRemainder int64
	for _, id := range batch.ExternalIDs {
		app := apps[id]
		if app == nil || app.Status == StatusConfirmed {
			continue
		}
		shares := app.Amount * NAVScale / batch.NAV
		confirmed := shares * batch.NAV / NAVScale
		app.Shares = shares
		app.ConfirmedAmount = confirmed
		app.RoundingRemainder = app.Amount - confirmed
		app.NAVVersion = batch.NAVVersion
		app.Status = StatusConfirmed
		totalShares += shares
		totalConfirmed += confirmed
		totalRemainder += app.RoundingRemainder
	}
	batch.Status = BatchConfirmed
	batch.TotalShares = totalShares
	batch.TotalConfirmedAmount = totalConfirmed
	batch.TotalRemainder = totalRemainder
	batch.ConfirmedAt = s.clock.Now()
	return copyBatch(batch), s.persist()
}

// GetApplication 按外部申请号查询申请及其确认结果。
func (s *Service) GetApplication(fundID, externalID string) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if app, ok := s.state.Applications[fundID][externalID]; ok {
		return *app, nil
	}
	return Application{}, ErrApplicationNotFound
}

// GetBatch 查询某估值日的批次快照。
func (s *Service) GetBatch(fundID, date string) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, err := s.findBatch(fundID, date)
	if err != nil {
		return Batch{}, err
	}
	return copyBatch(batch), nil
}

func (s *Service) findBatch(fundID, date string) (*Batch, error) {
	if _, ok := s.state.Funds[fundID]; !ok {
		return nil, ErrFundNotFound
	}
	if batch, ok := s.state.Batches[fundID][date]; ok {
		return batch, nil
	}
	return nil, ErrBatchNotFound
}

func (s *Service) getOrCreateBatch(fundID, date string) *Batch {
	batches := s.state.Batches[fundID]
	if batches == nil {
		batches = make(map[string]*Batch)
		s.state.Batches[fundID] = batches
	}
	if batch, ok := batches[date]; ok {
		return batch
	}
	batch := &Batch{
		ID:            fundID + "/" + date,
		FundID:        fundID,
		ValuationDate: date,
		Status:        BatchOpen,
	}
	batches[date] = batch
	return batch
}

func (s *Service) latestNAV(fundID, date string) (NAV, error) {
	versions := s.state.NAVs[fundID][date]
	if len(versions) == 0 {
		return NAV{}, ErrNAVNotFound
	}
	return versions[len(versions)-1], nil
}

func copyBatch(b *Batch) Batch {
	c := *b
	c.ExternalIDs = append([]string(nil), b.ExternalIDs...)
	return c
}

// valuationDate 按基金时区与每日截止时间计算申请归入的估值日：
// 提交时间所在日期是交易日且不晚于当日截止时间，归入当日；
// 否则归入下一个交易日。
func valuationDate(f *Fund, submitted time.Time) (string, error) {
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := submitted.In(loc)
	cutoff, err := time.Parse("15:04", f.CutoffTime)
	if err != nil {
		return "", ErrInvalidRequest
	}
	date := local.Format(DateLayout)
	if f.Calendar.IsTradingDay(date) {
		edge := time.Date(local.Year(), local.Month(), local.Day(),
			cutoff.Hour(), cutoff.Minute(), 0, 0, loc)
		if !local.After(edge) {
			return date, nil
		}
	}
	return f.Calendar.NextTradingDay(local)
}
