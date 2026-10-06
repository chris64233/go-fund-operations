package fundoperations

import (
	"fmt"
)

func validateNAVInput(fundID, valueDate string, nav Decimal, basis string) error {
	if fundID == "" || valueDate == "" {
		return fmt.Errorf("%w: fundID and valueDate required", ErrInvalidArgument)
	}
	if nav.unscaled == nil {
		return fmt.Errorf("%w: unit nav required", ErrInvalidArgument)
	}
	if nav.Sign() <= 0 {
		return fmt.Errorf("%w: unit nav must be positive", ErrInvalidArgument)
	}
	if basis == "" {
		return fmt.Errorf("%w: calculation basis required", ErrInvalidArgument)
	}
	return nil
}

// SettleAdjustment 将一笔差额标记为已处理（补收/退回完成）。
// 重复结算返回 ErrAlreadySettled；差额记录本身不删除，保持可追溯。
func (s *Service) SettleAdjustment(fundID, valueDate, adjustmentID, settlementRef string) (*Adjustment, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: adjustment %s", ErrNotFound, adjustmentID)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()

	adj := ser.adjustments[adjustmentID]
	if adj == nil {
		return nil, fmt.Errorf("%w: adjustment %s", ErrNotFound, adjustmentID)
	}
	if adj.Status == AdjustSettled {
		return nil, fmt.Errorf("%w: %s", ErrAlreadySettled, adjustmentID)
	}
	adj.Status = AdjustSettled
	adj.SettledAt = s.now()
	adj.SettlementRef = settlementRef
	return cloneAdjustment(adj), nil
}

// CurrentNAV 返回某基金某估值日的当前有效净值版本。
func (s *Service) CurrentNAV(fundID, valueDate string) (*NAVVersion, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil || ser.current == 0 {
		return nil, fmt.Errorf("%w: current nav for %s %s", ErrNotFound, fundID, valueDate)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	return cloneNAV(ser.versions[ser.current-1]), nil
}

// NAVVersion 按版本号查询净值版本（含已被取代的历史版本和更正元数据）。
func (s *Service) NAVVersion(fundID, valueDate string, version int) (*NAVVersion, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil || version <= 0 || version > len(ser.versions) {
		return nil, fmt.Errorf("%w: nav v%d for %s %s", ErrNotFound, version, fundID, valueDate)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	return cloneNAV(ser.versions[version-1]), nil
}

// ListNAVVersions 返回某基金某估值日的全部版本，按版本号升序。
// 可沿 BasedOn 链从当前版本逐级追溯到原版本。
func (s *Service) ListNAVVersions(fundID, valueDate string) []*NAVVersion {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	out := make([]*NAVVersion, 0, len(ser.versions))
	for _, v := range ser.versions {
		out = append(out, cloneNAV(v))
	}
	return out
}

// ListDrafts 返回草稿（默认仅未发布草稿，includeAll 含已撤回）。
func (s *Service) ListDrafts(fundID, valueDate string, includeWithdrawn bool) []*NAVVersion {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	var out []*NAVVersion
	for _, id := range ser.draftOrder {
		v := ser.drafts[id]
		if v.Status == StatusWithdrawn && !includeWithdrawn {
			continue
		}
		out = append(out, cloneNAV(v))
	}
	return out
}

// Confirmation 查询单笔交易确认。
func (s *Service) Confirmation(fundID, valueDate, confirmID string) (*Confirmation, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: confirmation %s", ErrNotFound, confirmID)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	c := ser.confirms[confirmID]
	if c == nil {
		return nil, fmt.Errorf("%w: confirmation %s", ErrNotFound, confirmID)
	}
	return cloneConfirmation(c), nil
}

// ListConfirmations 返回某基金某估值日的全部确认，按确认先后排序。
func (s *Service) ListConfirmations(fundID, valueDate string) []*Confirmation {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	out := make([]*Confirmation, 0, len(ser.confirmOrd))
	for _, id := range ser.confirmOrd {
		out = append(out, cloneConfirmation(ser.confirms[id]))
	}
	return out
}

// Adjustment 查询单笔差额处理。
func (s *Service) Adjustment(fundID, valueDate, adjustmentID string) (*Adjustment, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil, fmt.Errorf("%w: adjustment %s", ErrNotFound, adjustmentID)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	adj := ser.adjustments[adjustmentID]
	if adj == nil {
		return nil, fmt.Errorf("%w: adjustment %s", ErrNotFound, adjustmentID)
	}
	return cloneAdjustment(adj), nil
}

// AdjustmentFilter 差额查询过滤条件，零值不过滤。
type AdjustmentFilter struct {
	ConfirmID    string
	ToNAVVersion int
	Status       AdjustmentStatus
}

// ListAdjustments 返回差额处理记录，按登记先后排序并可按确认/更正版本/状态过滤。
func (s *Service) ListAdjustments(fundID, valueDate string, f AdjustmentFilter) []*Adjustment {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil {
		return nil
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()
	var out []*Adjustment
	for _, id := range ser.adjustOrd {
		adj := ser.adjustments[id]
		if f.ConfirmID != "" && adj.ConfirmID != f.ConfirmID {
			continue
		}
		if f.ToNAVVersion != 0 && adj.ToNAVVersion != f.ToNAVVersion {
			continue
		}
		if f.Status != "" && adj.Status != f.Status {
			continue
		}
		out = append(out, cloneAdjustment(adj))
	}
	return out
}

// AdjustmentsForConfirmation 返回某笔原确认关联的全部差额处理，
// 用于“原确认 → 差额”的正向追溯。
func (s *Service) AdjustmentsForConfirmation(fundID, valueDate, confirmID string) []*Adjustment {
	return s.ListAdjustments(fundID, valueDate, AdjustmentFilter{ConfirmID: confirmID})
}

// VersionTrail 版本追溯视图：一个版本及其到原版本的链路，
// 外加该版本作为更正目标时产生的差额与对应确认。
type VersionTrail struct {
	Current     *NAVVersion
	Chain       []*NAVVersion // 从当前版本沿 BasedOn 直到首发原版本
	Adjustments []*Adjustment // 链上各更正产生的差额，按登记先后
}

// TraceVersion 从指定版本（version=0 表示当前版本）追溯到原版本，
// 并汇总链上每一步更正登记的差额，形成
// 当前版本 → 原版本 → 受影响交易 → 处理状态 的完整视图。
func (s *Service) TraceVersion(fundID, valueDate string, version int) (*VersionTrail, error) {
	ser := s.lookupSeries(fundID, valueDate)
	if ser == nil || ser.current == 0 {
		return nil, fmt.Errorf("%w: nav for %s %s", ErrNotFound, fundID, valueDate)
	}
	ser.mu.Lock()
	defer ser.mu.Unlock()

	if version == 0 {
		version = ser.current
	}
	if version <= 0 || version > len(ser.versions) {
		return nil, fmt.Errorf("%w: nav v%d", ErrNotFound, version)
	}

	var chain []*NAVVersion
	seen := map[int]bool{}
	for v := version; v > 0; {
		if seen[v] {
			return nil, fmt.Errorf("corrupt version chain at v%d", v)
		}
		seen[v] = true
		node := ser.versions[v-1]
		chain = append(chain, cloneNAV(node))
		if node.BasedOn <= 0 {
			break
		}
		v = node.BasedOn
	}

	var adjs []*Adjustment
	correctionVersions := map[int]bool{}
	for _, node := range chain {
		if node.BasedOn > 0 {
			correctionVersions[node.Version] = true
		}
	}
	for _, id := range ser.adjustOrd {
		adj := ser.adjustments[id]
		if correctionVersions[adj.ToNAVVersion] {
			adjs = append(adjs, cloneAdjustment(adj))
		}
	}
	return &VersionTrail{Current: chain[0], Chain: chain, Adjustments: adjs}, nil
}
