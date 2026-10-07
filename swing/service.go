package swing

import (
	"fmt"
	"sort"
	"strings"
)

// Service 摆动定价服务：规则维护、日终试算/确认、版本差异与受影响交易查询。
// 当前实现为内存存储，接口语义与持久化实现一致。
type Service struct {
	rules  *ruleBook
	trades map[string]*Trade         // 按交易 ID
	calcs  map[string][]*Calculation // key: fundID|tradeDate，按版本升序
	seq    int64
}

// NewService 创建空服务实例。
func NewService() *Service {
	return &Service{
		rules:  newRuleBook(),
		trades: make(map[string]*Trade),
		calcs:  make(map[string][]*Calculation),
	}
}

func calcKey(fundID, tradeDate string) string {
	return fundID + "|" + tradeDate
}

// ---------- 规则维护 ----------

// PublishRule 发布规则新版本。已发布版本不可修改；
// 同一基金同一生效日期只允许一个版本。
func (s *Service) PublishRule(rule SwingRule) (SwingRule, error) {
	return s.rules.publish(rule)
}

// EffectiveRule 返回某交易日当时有效的规则版本。
func (s *Service) EffectiveRule(fundID, tradeDate string) (SwingRule, error) {
	r, ok := s.rules.effective(fundID, tradeDate)
	if !ok {
		return SwingRule{}, fmt.Errorf("no effective swing rule for fund %s on %s", fundID, tradeDate)
	}
	return r, nil
}

// ListRuleVersions 返回某基金全部已发布规则版本。
func (s *Service) ListRuleVersions(fundID string) []SwingRule {
	return s.rules.versions(fundID)
}

// ---------- 交易登记 ----------

// AddTrade 登记申购/赎回申请，返回分配的申请顺序号。
func (s *Service) AddTrade(t Trade) (int64, error) {
	if t.ID == "" {
		return 0, fmt.Errorf("trade id is required")
	}
	if _, exists := s.trades[t.ID]; exists {
		return 0, fmt.Errorf("trade %s already exists", t.ID)
	}
	if _, err := parseDate(t.TradeDate); err != nil {
		return 0, err
	}
	if t.Amount.Sign() <= 0 {
		return 0, fmt.Errorf("trade amount must be positive")
	}
	if t.Type != Subscription && t.Type != Redemption {
		return 0, fmt.Errorf("unknown trade type")
	}
	s.seq++
	t.Seq = s.seq
	t.Status = TradeActive
	s.trades[t.ID] = &t
	return t.Seq, nil
}

// CancelTrade 撤销交易。已确认快照不受影响；
// 撤销结果通过后续更正版本体现。
func (s *Service) CancelTrade(tradeID string) error {
	t, ok := s.trades[tradeID]
	if !ok {
		return fmt.Errorf("trade %s not found", tradeID)
	}
	t.Status = TradeCancelled
	return nil
}

// activeTrades 返回某基金某交易日的有效交易（按到达顺序）。
func (s *Service) activeTrades(fundID, tradeDate string) []*Trade {
	var out []*Trade
	for _, t := range s.trades {
		if t.FundID == fundID && t.TradeDate == tradeDate && t.Status == TradeActive {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// snapshot 冻结当前有效交易并汇总申购/赎回总额。
func (s *Service) snapshot(fundID, tradeDate string) (totalSub, totalRed Decimal, ids []string) {
	totalSub = DecimalFromInt(0)
	totalRed = DecimalFromInt(0)
	for _, t := range s.activeTrades(fundID, tradeDate) {
		ids = append(ids, t.ID)
		if t.Type == Subscription {
			totalSub = totalSub.Add(t.Amount)
		} else {
			totalRed = totalRed.Add(t.Amount)
		}
	}
	return totalSub, totalRed, ids
}

// ---------- 日终计算 ----------

// TrialCalculate 日终试算：冻结当前有效申购、赎回与基础净值，
// 计算净资金流并判断是否触发摆动。结果不落库、不影响确认流程。
func (s *Service) TrialCalculate(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	rule, err := s.EffectiveRule(fundID, tradeDate)
	if err != nil {
		return nil, err
	}
	if baseNAV.Sign() <= 0 {
		return nil, fmt.Errorf("base NAV must be positive")
	}
	totalSub, totalRed, ids := s.snapshot(fundID, tradeDate)
	c := compute(fundID, tradeDate, baseNAV, totalSub, totalRed, rule)
	c.Status = CalcDraft
	c.TradeIDs = ids
	return &c, nil
}

// Confirm 确认当日计算，生成不可修改的确认版本。
// 同一基金同一交易日只允许确认一次；重复确认返回错误。
// 确认后当日交易统一使用该版本的摆动净值。
func (s *Service) Confirm(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	key := calcKey(fundID, tradeDate)
	for _, c := range s.calcs[key] {
		if c.Status == CalcConfirmed {
			return nil, fmt.Errorf("fund %s on %s already confirmed at version %d; use Correct to create a new version",
				fundID, tradeDate, c.Version)
		}
	}
	c, err := s.TrialCalculate(fundID, tradeDate, baseNAV)
	if err != nil {
		return nil, err
	}
	c.Status = CalcConfirmed
	c.Version = 1
	s.calcs[key] = append(s.calcs[key], c)
	return c, nil
}

// Correct 在已确认基础上生成新的计算版本（更正）。
// 迟到申请与确认后撤销的交易在此纳入，产生差额明细；
// 原确认版本保持不变。
func (s *Service) Correct(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	key := calcKey(fundID, tradeDate)
	versions := s.calcs[key]
	if len(versions) == 0 {
		return nil, fmt.Errorf("fund %s on %s has no confirmed calculation to correct", fundID, tradeDate)
	}
	prev := versions[len(versions)-1]
	c, err := s.TrialCalculate(fundID, tradeDate, baseNAV)
	if err != nil {
		return nil, err
	}
	c.Status = CalcConfirmed
	c.Version = prev.Version + 1
	c.Supersedes = prev.Version
	c.Deltas = s.diffTrades(prev, c)
	s.calcs[key] = append(s.calcs[key], c)
	return c, nil
}

// diffTrades 计算新旧两版快照之间的交易差额明细。
func (s *Service) diffTrades(prev, next *Calculation) []TradeDelta {
	prevSet := make(map[string]bool, len(prev.TradeIDs))
	for _, id := range prev.TradeIDs {
		prevSet[id] = true
	}
	nextSet := make(map[string]bool, len(next.TradeIDs))
	for _, id := range next.TradeIDs {
		nextSet[id] = true
	}
	var deltas []TradeDelta
	for _, id := range next.TradeIDs {
		if !prevSet[id] {
			deltas = append(deltas, s.newDelta(id, "ADDED", prev, next))
		}
	}
	for _, id := range prev.TradeIDs {
		if !nextSet[id] {
			deltas = append(deltas, s.newDelta(id, "REMOVED", prev, next))
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].TradeID < deltas[j].TradeID })
	return deltas
}

// newDelta 构造单笔差额明细；AmountDelta 为该笔对净资金流的影响
// （申购为正、赎回为负；剔除时取反）。
func (s *Service) newDelta(tradeID, change string, prev, next *Calculation) TradeDelta {
	d := TradeDelta{TradeID: tradeID, Change: change, OldPrice: prev.AdjustedNAV, NewPrice: next.AdjustedNAV}
	t, ok := s.trades[tradeID]
	if !ok {
		return d
	}
	d.Type = t.Type
	d.Amount = t.Amount
	if t.Type == Subscription {
		d.AmountDelta = t.Amount
	} else {
		d.AmountDelta = t.Amount.Neg()
	}
	if change == "REMOVED" {
		d.AmountDelta = d.AmountDelta.Neg()
	}
	return d
}

// ---------- 查询 ----------

// GetCalculation 返回指定版本的计算结果；version 为 0 时返回最新版本。
func (s *Service) GetCalculation(fundID, tradeDate string, version int) (*Calculation, error) {
	versions := s.calcs[calcKey(fundID, tradeDate)]
	if len(versions) == 0 {
		return nil, fmt.Errorf("no calculation for fund %s on %s", fundID, tradeDate)
	}
	if version == 0 {
		return versions[len(versions)-1], nil
	}
	for _, c := range versions {
		if c.Version == version {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no version %d for fund %s on %s", version, fundID, tradeDate)
}

// VersionDiff 比较同一交易日两个计算版本，返回差异说明。
func (s *Service) VersionDiff(fundID, tradeDate string, v1, v2 int) ([]string, error) {
	a, err := s.GetCalculation(fundID, tradeDate, v1)
	if err != nil {
		return nil, err
	}
	b, err := s.GetCalculation(fundID, tradeDate, v2)
	if err != nil {
		return nil, err
	}
	var out []string
	add := func(label string, x, y Decimal) {
		if x.Cmp(y) != 0 {
			out = append(out, fmt.Sprintf("%s: %s -> %s", label, x, y))
		}
	}
	add("有效申购", a.TotalSub, b.TotalSub)
	add("有效赎回", a.TotalRed, b.TotalRed)
	add("净资金流", a.NetFlow, b.NetFlow)
	add("基础净值", a.BaseNAV, b.BaseNAV)
	add("调整后净值", a.AdjustedNAV, b.AdjustedNAV)
	if a.Triggered != b.Triggered {
		out = append(out, fmt.Sprintf("是否触发摆动: %v -> %v", a.Triggered, b.Triggered))
	}
	if a.Direction != b.Direction {
		out = append(out, fmt.Sprintf("摆动方向: %s -> %s", a.Direction, b.Direction))
	}
	for _, d := range b.Deltas {
		out = append(out, fmt.Sprintf("交易 %s %s（影响净资金流 %s）", d.TradeID, d.Change, d.AmountDelta))
	}
	if len(out) == 0 {
		out = append(out, "两个版本无差异")
	}
	return out, nil
}

// AffectedTrade 受影响交易查询结果。
type AffectedTrade struct {
	Trade       Trade
	InSnapshot  bool    // 是否已纳入最新确认快照
	AppliedNAV  Decimal // 该交易适用的净值（已确认的摆动净值）
	PendingNote string  // 未纳入快照时的说明（迟到/撤销待更正）
}

// AffectedTrades 返回某基金某交易日的受影响交易：
// 快照内交易统一使用确认的摆动净值；
// 确认后到达的迟到申请与确认后撤销的交易单独标注，不混入快照。
func (s *Service) AffectedTrades(fundID, tradeDate string) ([]AffectedTrade, error) {
	latest, err := s.GetCalculation(fundID, tradeDate, 0)
	if err != nil {
		return nil, err
	}
	inSnapshot := make(map[string]bool, len(latest.TradeIDs))
	for _, id := range latest.TradeIDs {
		inSnapshot[id] = true
	}
	var out []AffectedTrade
	for _, t := range s.trades {
		if t.FundID != fundID || t.TradeDate != tradeDate {
			continue
		}
		at := AffectedTrade{Trade: *t, AppliedNAV: latest.AdjustedNAV}
		switch {
		case inSnapshot[t.ID] && t.Status == TradeActive:
			at.InSnapshot = true
		case inSnapshot[t.ID] && t.Status == TradeCancelled:
			at.PendingNote = "确认后撤销，待更正版本剔除"
		case t.Status == TradeActive:
			at.PendingNote = "确认后到达的迟到申请，待更正版本纳入"
		default:
			at.PendingNote = "已撤销且不在快照中"
		}
		out = append(out, at)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Trade.Seq < out[j].Trade.Seq })
	return out, nil
}

// Explain 说明某交易日是否触发摆动及其计算依据。
func (s *Service) Explain(fundID, tradeDate string) (string, error) {
	c, err := s.GetCalculation(fundID, tradeDate, 0)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "基金 %s %s 计算版本 %d（规则版本 %d）：\n", c.FundID, c.TradeDate, c.Version, c.RuleVersion)
	for _, step := range c.Steps {
		fmt.Fprintf(&b, "  - %s\n", step)
	}
	if c.Triggered {
		fmt.Fprintf(&b, "结论：触发摆动（%s），调整比例 %s，调整后净值 %s。\n",
			c.Direction, c.AdjustFactor, c.AdjustedNAV)
	} else {
		fmt.Fprintf(&b, "结论：未触发摆动，净值维持 %s。\n", c.AdjustedNAV)
	}
	return b.String(), nil
}
