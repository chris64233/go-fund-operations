package fundoperations

import (
	"fmt"
	"sync"
	"time"
)

// Service 提供净值草稿、发布、撤回、更正、交易确认与差额处理能力。
// 所有状态变更都在同一把互斥锁内完成，保证：
//   - 同一基金同一估值日任意时刻只有一个当前有效版本；
//   - 版本切换与受影响确认的差额记录生成是原子的，不会留下孤立的差额记录；
//   - 基于旧版本的迟到确认会被明确拒绝，而不会把新版本降回旧版本。
type Service struct {
	mu sync.Mutex

	navs             map[string]*NAVRecord
	current          map[string]string // fundID|date -> 当前有效已发布版本 ID
	confirmations    map[string]*TradeConfirmation
	confirmsByNAV    map[string][]string // navVersionID -> confirmation IDs
	corrections      map[string]*Correction
	adjustments      map[string]*Adjustment
	adjustsByConfirm map[string][]string // confirmationID -> adjustment IDs
	adjustsByCorrect map[string][]string // correctionID -> adjustment IDs
	seq              int64
	now              func() time.Time
}

// NewService 创建一个空的内存版服务。
func NewService() *Service {
	return &Service{
		navs:             make(map[string]*NAVRecord),
		current:          make(map[string]string),
		confirmations:    make(map[string]*TradeConfirmation),
		confirmsByNAV:    make(map[string][]string),
		corrections:      make(map[string]*Correction),
		adjustments:      make(map[string]*Adjustment),
		adjustsByConfirm: make(map[string][]string),
		adjustsByCorrect: make(map[string][]string),
		now:              time.Now,
	}
}

func navKey(fundID string, date time.Time) string {
	return fundID + "|" + normDate(date).Format("2006-01-02")
}

func normDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (s *Service) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// SaveDraft 保存一份净值草稿。草稿不参与当前版本判定，可在发布前撤回。
func (s *Service) SaveDraft(fundID string, date time.Time, nav NAV, basisVersion string) (*NAVRecord, error) {
	if fundID == "" {
		return nil, fmt.Errorf("fund id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := &NAVRecord{
		ID:            s.nextID("NAV"),
		FundID:        fundID,
		ValuationDate: normDate(date),
		NAV:           nav,
		BasisVersion:  basisVersion,
		Version:       1,
		Status:        NAVStatusDraft,
		CreatedAt:     s.now(),
	}
	s.navs[rec.ID] = rec
	cp := *rec
	return &cp, nil
}

// PublishDraft 发布一份草稿。
// 若同一基金同一估值日已存在当前有效版本：
//   - 内容（单位净值与计算依据版本）完全一致时，返回原版本，草稿被标记为撤回；
//   - 内容不同时返回 ErrVersionConflict，草稿保持草稿状态，可撤回或修改后更正。
func (s *Service) PublishDraft(draftID string) (*NAVRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	draft, ok := s.navs[draftID]
	if !ok {
		return nil, ErrNAVNotFound
	}
	if draft.Status != NAVStatusDraft {
		return nil, fmt.Errorf("%w: draft %s is %s", ErrInvalidTransition, draftID, draft.Status)
	}

	key := navKey(draft.FundID, draft.ValuationDate)
	if curID, ok := s.current[key]; ok {
		cur := s.navs[curID]
		if cur.NAV == draft.NAV && cur.BasisVersion == draft.BasisVersion {
			draft.Status = NAVStatusWithdrawn
			cp := *cur
			return &cp, nil
		}
		return nil, fmt.Errorf("%w: fund %s date %s already has version %d",
			ErrVersionConflict, draft.FundID, draft.ValuationDate.Format("2006-01-02"), cur.Version)
	}

	draft.Status = NAVStatusPublished
	draft.PublishedAt = s.now()
	s.current[key] = draft.ID
	cp := *draft
	return &cp, nil
}

// PublishNAV 是 SaveDraft + PublishDraft 的便捷形式，语义与 PublishDraft 相同。
func (s *Service) PublishNAV(fundID string, date time.Time, nav NAV, basisVersion string) (*NAVRecord, error) {
	draft, err := s.SaveDraft(fundID, date, nav, basisVersion)
	if err != nil {
		return nil, err
	}
	return s.PublishDraft(draft.ID)
}

// WithdrawDraft 撤回一份尚未被任何交易确认引用的草稿。
func (s *Service) WithdrawDraft(draftID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	draft, ok := s.navs[draftID]
	if !ok {
		return ErrNAVNotFound
	}
	if draft.Status != NAVStatusDraft {
		return fmt.Errorf("%w: only drafts can be withdrawn, %s is %s", ErrInvalidTransition, draftID, draft.Status)
	}
	if len(s.confirmsByNAV[draftID]) > 0 {
		return fmt.Errorf("%w: %s", ErrNAVReferenced, draftID)
	}
	draft.Status = NAVStatusWithdrawn
	return nil
}

// ConfirmTrade 基于指定净值版本确认一笔申购或赎回。
// 只有当前有效版本可以被引用；引用已被取代的旧版本会返回 ErrStaleNAVVersion。
// 确认金额按统一精度在确认时刻计算并冻结。
func (s *Service) ConfirmTrade(fundID string, date time.Time, navVersionID string, tradeType TradeType, shares Shares) (*TradeConfirmation, error) {
	if shares <= 0 {
		return nil, fmt.Errorf("shares must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.navs[navVersionID]
	if !ok {
		return nil, ErrNAVNotFound
	}
	if rec.FundID != fundID || !rec.ValuationDate.Equal(normDate(date)) {
		return nil, fmt.Errorf("%w: version %s does not belong to fund %s date %s",
			ErrStaleNAVVersion, navVersionID, fundID, normDate(date).Format("2006-01-02"))
	}
	switch rec.Status {
	case NAVStatusPublished:
		// 唯一可引用的状态。
	case NAVStatusSuperseded:
		return nil, fmt.Errorf("%w: version %s has been superseded", ErrStaleNAVVersion, navVersionID)
	default:
		return nil, fmt.Errorf("%w: version %s is %s", ErrNAVNotPublished, navVersionID, rec.Status)
	}
	if s.current[navKey(fundID, date)] != navVersionID {
		return nil, fmt.Errorf("%w: version %s is not current", ErrStaleNAVVersion, navVersionID)
	}

	conf := &TradeConfirmation{
		ID:            s.nextID("CONF"),
		FundID:        fundID,
		ValuationDate: rec.ValuationDate,
		NAVVersionID:  navVersionID,
		Type:          tradeType,
		Shares:        shares,
		Amount:        AmountFor(shares, rec.NAV),
		CreatedAt:     s.now(),
	}
	s.confirmations[conf.ID] = conf
	s.confirmsByNAV[navVersionID] = append(s.confirmsByNAV[navVersionID], conf.ID)
	cp := *conf
	return &cp, nil
}

// CorrectNAV 对当前有效版本登记更正：生成递增的新版本并置为当前有效，
// 原版本转为 SUPERSEDED，其记录与既有引用关系保持不变。
// 同一临界区内为所有引用原版本的确认逐笔生成差额处理记录（Adjustment），
// 原确认金额不被修改。
func (s *Service) CorrectNAV(fundID string, date time.Time, nav NAV, basisVersion, reason, correctedBy string) (*CorrectionResult, error) {
	if reason == "" || correctedBy == "" {
		return nil, fmt.Errorf("correction reason and operator are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := navKey(fundID, date)
	curID, ok := s.current[key]
	if !ok {
		return nil, fmt.Errorf("%w: no published nav for fund %s date %s",
			ErrNAVNotFound, fundID, normDate(date).Format("2006-01-02"))
	}
	cur := s.navs[curID]
	if cur.NAV == nav && cur.BasisVersion == basisVersion {
		return nil, fmt.Errorf("%w: identical to version %d", ErrNoChange, cur.Version)
	}

	now := s.now()
	newRec := &NAVRecord{
		ID:            s.nextID("NAV"),
		FundID:        fundID,
		ValuationDate: cur.ValuationDate,
		NAV:           nav,
		BasisVersion:  basisVersion,
		Version:       cur.Version + 1,
		Status:        NAVStatusPublished,
		CorrectionOf:  cur.ID,
		Reason:        reason,
		CorrectedBy:   correctedBy,
		PublishedAt:   now,
		CreatedAt:     now,
	}
	correction := &Correction{
		ID:            s.nextID("CORR"),
		FundID:        fundID,
		ValuationDate: cur.ValuationDate,
		FromVersionID: cur.ID,
		ToVersionID:   newRec.ID,
		Reason:        reason,
		CorrectedBy:   correctedBy,
		CreatedAt:     now,
	}

	// 原子切换：原版本冻结，新版本成为唯一当前有效版本。
	cur.Status = NAVStatusSuperseded
	s.navs[newRec.ID] = newRec
	s.corrections[correction.ID] = correction
	s.current[key] = newRec.ID

	var adjustments []*Adjustment
	for _, confID := range s.confirmsByNAV[cur.ID] {
		conf := s.confirmations[confID]
		newAmount := AmountFor(conf.Shares, newRec.NAV)
		delta := newAmount - conf.Amount
		if delta == 0 {
			continue
		}
		adj := &Adjustment{
			ID:             s.nextID("ADJ"),
			CorrectionID:   correction.ID,
			ConfirmationID: conf.ID,
			FromVersionID:  cur.ID,
			ToVersionID:    newRec.ID,
			OldAmount:      conf.Amount,
			NewAmount:      newAmount,
			Delta:          delta,
			Status:         AdjustmentPending,
			CreatedAt:      now,
		}
		s.adjustments[adj.ID] = adj
		s.adjustsByConfirm[conf.ID] = append(s.adjustsByConfirm[conf.ID], adj.ID)
		s.adjustsByCorrect[correction.ID] = append(s.adjustsByCorrect[correction.ID], adj.ID)
		adjCopy := *adj
		adjustments = append(adjustments, &adjCopy)
	}

	newCopy := *newRec
	corrCopy := *correction
	return &CorrectionResult{
		Correction:  &corrCopy,
		NewVersion:  &newCopy,
		Adjustments: adjustments,
	}, nil
}

// ProcessAdjustment 把一笔差额处理记录标记为已处理。
func (s *Service) ProcessAdjustment(adjustmentID string) (*Adjustment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	adj, ok := s.adjustments[adjustmentID]
	if !ok {
		return nil, ErrAdjustmentNotFound
	}
	adj.Status = AdjustmentProcessed
	cp := *adj
	return &cp, nil
}

// GetNAV 按 ID 查询净值版本。
func (s *Service) GetNAV(id string) (*NAVRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.navs[id]
	if !ok {
		return nil, ErrNAVNotFound
	}
	cp := *rec
	return &cp, nil
}

// CurrentNAV 查询某一基金某一估值日的当前有效版本。
func (s *Service) CurrentNAV(fundID string, date time.Time) (*NAVRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.current[navKey(fundID, date)]
	if !ok {
		return nil, fmt.Errorf("%w: no published nav", ErrNAVNotFound)
	}
	cp := *s.navs[id]
	return &cp, nil
}

// NAVHistory 从当前有效版本沿 CorrectionOf 链回溯到原始版本，
// 返回按版本号降序排列的完整版本链。
func (s *Service) NAVHistory(fundID string, date time.Time) ([]*NAVRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.current[navKey(fundID, date)]
	if !ok {
		return nil, fmt.Errorf("%w: no published nav", ErrNAVNotFound)
	}
	var chain []*NAVRecord
	for id != "" {
		rec, ok := s.navs[id]
		if !ok {
			return nil, fmt.Errorf("%w: broken correction chain at %s", ErrNAVNotFound, id)
		}
		cp := *rec
		chain = append(chain, &cp)
		id = rec.CorrectionOf
	}
	return chain, nil
}

// GetConfirmation 按 ID 查询交易确认。
func (s *Service) GetConfirmation(id string) (*TradeConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conf, ok := s.confirmations[id]
	if !ok {
		return nil, ErrConfirmationNotFound
	}
	cp := *conf
	return &cp, nil
}

// ConfirmationsByNAV 查询引用了指定净值版本的所有交易确认。
func (s *Service) ConfirmationsByNAV(navVersionID string) []*TradeConfirmation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*TradeConfirmation
	for _, id := range s.confirmsByNAV[navVersionID] {
		cp := *s.confirmations[id]
		out = append(out, &cp)
	}
	return out
}

// AdjustmentsByConfirmation 查询某笔确认关联的所有差额处理记录。
func (s *Service) AdjustmentsByConfirmation(confirmationID string) []*Adjustment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Adjustment
	for _, id := range s.adjustsByConfirm[confirmationID] {
		cp := *s.adjustments[id]
		out = append(out, &cp)
	}
	return out
}

// AdjustmentsByCorrection 查询某次更正产生的所有差额处理记录。
func (s *Service) AdjustmentsByCorrection(correctionID string) []*Adjustment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Adjustment
	for _, id := range s.adjustsByCorrect[correctionID] {
		cp := *s.adjustments[id]
		out = append(out, &cp)
	}
	return out
}
