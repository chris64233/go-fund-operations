package fundoperations

import (
	"fmt"
	"sync"
	"time"
)

// Service 是净值版本、交易确认与差额处理的应用服务。
// 每个 (基金, 估值日) 序列由独立互斥锁保护，保证：
// 任何时刻只有一个当前有效版本；版本晋升、确认引用与差额登记
// 在同一临界区内原子完成，因此不会出现旧版本回潮或孤立差额。
type Service struct {
	now func() time.Time

	registryMu sync.Mutex
	series     map[string]*navSeries
}

// NewService 创建服务，时钟默认使用 time.Now。
func NewService() *Service {
	return &Service{
		now:    time.Now,
		series: make(map[string]*navSeries),
	}
}

func (s *Service) withClock(now func() time.Time) *Service {
	s.now = now
	return s
}

type navSeries struct {
	mu sync.Mutex

	drafts      map[string]*NAVVersion // key: draftID
	draftOrder  []string
	versions    []*NAVVersion // 已发布版本，下标 Version-1
	current     int           // 当前有效版本号，0 表示尚无发布版本
	confirms    map[string]*Confirmation
	confirmOrd  []string
	adjustments map[string]*Adjustment
	adjustOrd   []string
}

func seriesKey(fundID, valueDate string) string { return fundID + "|" + valueDate }

func (s *Service) getSeries(fundID, valueDate string) *navSeries {
	key := seriesKey(fundID, valueDate)
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	ser := s.series[key]
	if ser == nil {
		ser = &navSeries{
			drafts:      make(map[string]*NAVVersion),
			confirms:    make(map[string]*Confirmation),
			adjustments: make(map[string]*Adjustment),
		}
		s.series[key] = ser
	}
	return ser
}

// lookupSeries 只取已存在的序列，不存在返回 nil。
func (s *Service) lookupSeries(fundID, valueDate string) *navSeries {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	return s.series[seriesKey(fundID, valueDate)]
}

// DraftInput 创建净值草稿的入参。
type DraftInput struct {
	FundID    string
	ValueDate string
	UnitNAV   Decimal
	Basis     string
}

// CreateDraft 登记一条净值草稿。草稿不占用版本号，版本号在发布时分配。
func (s *Service) CreateDraft(in DraftInput) (*NAVVersion, string, error) {
	if err := validateNAVInput(in.FundID, in.ValueDate, in.UnitNAV, in.Basis); err != nil {
		return nil, "", err
	}
	ser := s.getSeries(in.FundID, in.ValueDate)
	ser.mu.Lock()
	defer ser.mu.Unlock()

	draftID := fmt.Sprintf("DRF-%d", len(ser.draftOrder)+1)
	v := &NAVVersion{
		FundID:    in.FundID,
		ValueDate: in.ValueDate,
		UnitNAV:   in.UnitNAV,
		Basis:     in.Basis,
		Status:    StatusDraft,
		CreatedAt: s.now(),
	}
	ser.drafts[draftID] = v
	ser.draftOrder = append(ser.draftOrder, draftID)
	return cloneNAV(v), draftID, nil
}

// WithdrawDraft 撤回一条尚未发布的草稿。草稿永不参与交易引用；
// 已发布版本不能通过此接口撤回（不可变）。
func (s *Service) WithdrawDraft(fundID, valueDate, draftID string) error {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return fmt.Errorf("%w: draft %s", ErrNotFound, draftID)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()

	v := ser.drafts[draftID]
	if v == nil {
		return fmt.Errorf("%w: draft %s", ErrNotFound, draftID)
	}
	switch v.Status {
	case StatusWithdrawn:
		return fmt.Errorf("%w: draft %s already withdrawn", ErrInvalidArgument, draftID)
	case StatusPublished, StatusSuperseded:
		return fmt.Errorf("%w: published version cannot be withdrawn", ErrImmutableVersion)
	}
	v.Status = StatusWithdrawn
	v.WithdrawnAt = s.now()
	return nil
}

// PublishResult 发布结果；SameContent 为 true 时表示命中幂等，
// 返回的是此前已存在的当前版本，没有产生新版本。
type PublishResult struct {
	Version     *NAVVersion
	SameContent bool
}

// PublishDraft 发布草稿。
//   - 序列尚无发布版本：分配下一个递增版本号并成为当前有效版本；
//   - 当前版本内容（净值与计算依据）完全相同：返回原版本，不产生新版本；
//   - 当前版本内容不同：返回 ErrVersionConflict，更正必须走 CorrectNAV。
func (s *Service) PublishDraft(fundID, valueDate, draftID string) (*PublishResult, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: draft %s", ErrNotFound, draftID)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()

	draft := ser.drafts[draftID]
	if draft == nil {
		return nil, fmt.Errorf("%w: draft %s", ErrNotFound, draftID)
	}
	if draft.Status != StatusDraft {
		return nil, fmt.Errorf("%w: draft %s is %s", ErrInvalidArgument, draftID, draft.Status)
	}

	if ser.current != 0 {
		cur := ser.versions[ser.current-1]
		if cur.UnitNAV.Equal(draft.UnitNAV) && cur.Basis == draft.Basis {
			// 幂等：相同内容重复发布，原封不动返回当前版本，重复草稿作废。
			draft.Status = StatusWithdrawn
			draft.WithdrawnAt = s.now()
			return &PublishResult{Version: cloneNAV(cur), SameContent: true}, nil
		}
		return nil, &ConflictError{
			Err:         ErrVersionConflict,
			FundID:      fundID,
			ValueDate:   valueDate,
			Current:     ser.current,
			SeenVersion: ser.current,
		}
	}

	draft.Version = len(ser.versions) + 1
	draft.Status = StatusPublished
	draft.PublishedAt = s.now()
	ser.versions = append(ser.versions, draft)
	ser.current = draft.Version
	return &PublishResult{Version: cloneNAV(draft)}, nil
}

// CorrectionInput 净值更正登记入参，必须引用原版本、原因与更正人。
type CorrectionInput struct {
	FundID      string
	ValueDate   string
	BasedOn     int
	NewUnitNAV  Decimal
	NewBasis    string
	Reason      string
	CorrectedBy string
}

// CorrectionResult 更正结果。
type CorrectionResult struct {
	Version     *NAVVersion
	Adjustments []*Adjustment // 本次更正为受影响确认登记的差额
}

// CorrectNAV 发布净值更正：
// 基于当前有效版本生成递增新版本，原版本转为 superseded 且永不修改；
// 同一临界区内为所有引用原版本的确认逐笔登记差额，差额与原确认一一关联。
func (s *Service) CorrectNAV(in CorrectionInput) (*CorrectionResult, error) {
	if err := validateNAVInput(in.FundID, in.ValueDate, in.NewUnitNAV, in.NewBasis); err != nil {
		return nil, err
	}
	if in.Reason == "" || in.CorrectedBy == "" {
		return nil, fmt.Errorf("%w: correction reason and correctedBy are required", ErrInvalidArgument)
	}
	ser := s.lookupSeries(in.FundID, in.ValueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: no published nav to correct", ErrNotFound)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	if ser.current == 0 {
		return nil, fmt.Errorf("%w: no published nav to correct", ErrNotFound)
	}

	if in.BasedOn <= 0 || in.BasedOn > len(ser.versions) {
		return nil, fmt.Errorf("%w: based-on version v%d", ErrNotFound, in.BasedOn)
	}
	base := ser.versions[in.BasedOn-1]
	if in.BasedOn != ser.current {
		return nil, &ConflictError{
			Err:         ErrVersionConflict,
			FundID:      in.FundID,
			ValueDate:   in.ValueDate,
			Current:     ser.current,
			SeenVersion: in.BasedOn,
		}
	}
	if base.Status != StatusPublished {
		return nil, fmt.Errorf("%w: version v%d is %s", ErrImmutableVersion, in.BasedOn, base.Status)
	}
	// 与当前内容完全相同的“更正”幂等返回，不产生版本与差额。
	if base.UnitNAV.Equal(in.NewUnitNAV) && base.Basis == in.NewBasis {
		return &CorrectionResult{Version: cloneNAV(base)}, nil
	}

	now := s.now()
	base.Status = StatusSuperseded

	corrected := &NAVVersion{
		FundID:      in.FundID,
		ValueDate:   in.ValueDate,
		Version:     len(ser.versions) + 1,
		UnitNAV:     in.NewUnitNAV,
		Basis:       in.NewBasis,
		Status:      StatusPublished,
		CreatedAt:   now,
		PublishedAt: now,
		BasedOn:     in.BasedOn,
		Reason:      in.Reason,
		CorrectedBy: in.CorrectedBy,
	}
	ser.versions = append(ser.versions, corrected)
	ser.current = corrected.Version

	// 受影响交易 = 所有引用被更正版本的确认。原确认保持不变，
	// 仅按 份额×(新净值-原净值) 追加差额；净值未变（仅依据变化）时无差额。
	var created []*Adjustment
	if in.NewUnitNAV.Cmp(base.UnitNAV) != 0 {
		seq := len(ser.adjustOrd)
		for _, id := range ser.confirmOrd {
			c := ser.confirms[id]
			if c.NAVVersion != in.BasedOn {
				continue
			}
			seq++
			adj := buildAdjustment(c, base.UnitNAV, in.NewUnitNAV, corrected.Version, now, seq)
			ser.adjustments[adj.ID] = adj
			ser.adjustOrd = append(ser.adjustOrd, adj.ID)
			created = append(created, cloneAdjustment(adj))
		}
	}
	return &CorrectionResult{Version: cloneNAV(corrected), Adjustments: created}, nil
}

func buildAdjustment(c *Confirmation, oldNAV, newNAV Decimal, toVersion int, now time.Time, seq int) *Adjustment {
	deltaNAV := newNAV.Sub(oldNAV)
	signed := c.Shares.Mul(deltaNAV).Round(amountScale)
	if c.TxType == TxRedemption {
		// 赎回方向相反：净值上浮应向投资人补付（退回），下浮应追回（补收）。
		signed = signed.Neg()
	}
	kind := AdjustCollect
	if signed.Sign() < 0 {
		kind = AdjustRefund
	}
	return &Adjustment{
		ID:             fmt.Sprintf("ADJ-%d", seq),
		FundID:         c.FundID,
		ValueDate:      c.ValueDate,
		ConfirmID:      c.ConfirmID,
		FromNAVVersion: c.NAVVersion,
		ToNAVVersion:   toVersion,
		TxType:         c.TxType,
		Shares:         c.Shares,
		OldNAV:         oldNAV,
		NewNAV:         newNAV,
		DeltaNAV:       deltaNAV,
		Amount:         signed,
		Type:           kind,
		Status:         AdjustPending,
		CreatedAt:      now,
	}
}

// ConfirmTxInput 交易确认入参。NAVVersion 为调用方依据的净值版本；
// 传 0 表示直接采用当前有效版本。
type ConfirmTxInput struct {
	ConfirmID  string
	FundID     string
	ValueDate  string
	TxType     TxType
	Shares     Decimal
	NAVVersion int
}

// ConfirmTx 生成不可变的交易确认。
// 若调用方基于的版本已不是当前有效版本（迟到确认），返回
// ErrStaleNAVReference 冲突，绝不回退新版本，也不产生任何副作用。
func (s *Service) ConfirmTx(in ConfirmTxInput) (*Confirmation, error) {
	if in.ConfirmID == "" || in.FundID == "" || in.ValueDate == "" {
		return nil, fmt.Errorf("%w: confirmID, fundID, valueDate required", ErrInvalidArgument)
	}
	if in.TxType != TxSubscription && in.TxType != TxRedemption {
		return nil, fmt.Errorf("%w: unknown tx type %q", ErrInvalidArgument, in.TxType)
	}
	if in.Shares.Sign() <= 0 {
		return nil, fmt.Errorf("%w: shares must be positive", ErrInvalidArgument)
	}
	ser := s.lookupSeries(in.FundID, in.ValueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: no published nav for %s %s", ErrNotFound, in.FundID, in.ValueDate)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	if ser.current == 0 {
		return nil, fmt.Errorf("%w: no published nav for %s %s", ErrNotFound, in.FundID, in.ValueDate)
	}

	// 幂等：同一确认号重复请求返回原确认，不重复引用、不重复计费。
	if existing := ser.confirms[in.ConfirmID]; existing != nil {
		return cloneConfirmation(existing), nil
	}

	want := in.NAVVersion
	if want == 0 {
		want = ser.current
	}
	if want != ser.current {
		return nil, &ConflictError{
			Err:         ErrStaleNAVReference,
			FundID:      in.FundID,
			ValueDate:   in.ValueDate,
			Current:     ser.current,
			SeenVersion: want,
		}
	}
	cur := ser.versions[ser.current-1]
	c := &Confirmation{
		ConfirmID:    in.ConfirmID,
		FundID:       in.FundID,
		ValueDate:    in.ValueDate,
		TxType:       in.TxType,
		Shares:       in.Shares,
		NAVVersion:   cur.Version,
		NAVAtConfirm: cur.UnitNAV,
		Amount:       in.Shares.Mul(cur.UnitNAV).Round(amountScale),
		ConfirmedAt:  s.now(),
	}
	ser.confirms[c.ConfirmID] = c
	ser.confirmOrd = append(ser.confirmOrd, c.ConfirmID)
	return cloneConfirmation(c), nil
}
