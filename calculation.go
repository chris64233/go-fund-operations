package fundoperations

import (
	"errors"
	"fmt"
	"strings"
)

// CalcStatus 计算版本状态。
type CalcStatus int

const (
	// CalcConfirmed 已确认，当日交易统一使用该版本的摆动净值。
	CalcConfirmed CalcStatus = iota + 1
	// CalcSuperseded 已被更正版本取代，结果保留仅供追溯。
	CalcSuperseded
)

// SwingDirection 摆动方向。
type SwingDirection string

const (
	DirectionNone SwingDirection = "未触发"
	DirectionUp   SwingDirection = "上调"
	DirectionDown SwingDirection = "下调"
)

// Calculation 一次日终计算的不可变快照。
type Calculation struct {
	FundID            string
	TradeDate         string
	Version           int
	Status            CalcStatus
	RuleVersion       int
	BaseNAV           Decimal
	TotalSubscription Decimal
	TotalRedemption   Decimal // 按基础净值折算的赎回金额
	NetFlow           Decimal
	Threshold         Decimal
	Triggered         bool
	Direction         SwingDirection
	AppliedRate       Decimal // 实际调整比例（已受最大幅度限制）
	SwungNAV          Decimal
	TradeIDs          []string // 冻结的当日有效交易
}

// TradeDiff 两个计算版本之间单笔交易的差额明细。
type TradeDiff struct {
	TradeID    string
	Type       TradeType
	Quantity   Decimal
	OldNAV     Decimal
	NewNAV     Decimal
	OldResult  Decimal // 申购为份额，赎回为金额
	NewResult  Decimal
	Difference Decimal // NewResult - OldResult
}

// CalcDiff 两个计算版本的汇总差异。
type CalcDiff struct {
	FromVersion int
	ToVersion   int
	OldSwungNAV Decimal
	NewSwungNAV Decimal
	NAVChange   Decimal
	OldNetFlow  Decimal
	NewNetFlow  Decimal
	TradeDiffs  []TradeDiff
}

// Engine 摆动定价日终计算引擎。
type Engine struct {
	rules  *RuleStore
	trades *TradeStore
	calcs  map[string][]*Calculation // fundID|tradeDate -> 按版本升序
}

// NewEngine 创建计算引擎。
func NewEngine(rules *RuleStore, trades *TradeStore) *Engine {
	return &Engine{rules: rules, trades: trades, calcs: make(map[string][]*Calculation)}
}

func calcKey(fundID, tradeDate string) string { return fundID + "|" + tradeDate }

// SubmitTrade 登记交易；若当日计算已确认，自动标记为迟到申请。
func (e *Engine) SubmitTrade(t Trade) (*Trade, error) {
	if e.confirmedCalc(t.FundID, t.TradeDate) != nil {
		t.Late = true
	}
	return e.trades.Submit(t)
}

// Trial 日终试算：冻结当前有效交易与基础净值进行计算，不落库、不改变状态。
func (e *Engine) Trial(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	return e.compute(fundID, tradeDate, baseNAV)
}

// Confirm 确认当日计算，生成第一个确认版本。
// 当日已存在确认版本时返回错误（重复确认）。
func (e *Engine) Confirm(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	if e.confirmedCalc(fundID, tradeDate) != nil {
		return nil, fmt.Errorf("swing: %s %s already confirmed", fundID, tradeDate)
	}
	calc, err := e.compute(fundID, tradeDate, baseNAV)
	if err != nil {
		return nil, err
	}
	calc.Version = 1
	calc.Status = CalcConfirmed
	e.appendCalc(calc)
	for _, id := range calc.TradeIDs {
		if t, ok := e.trades.trades[id]; ok && t.Status == TradePending {
			t.Status = TradeConfirmed
		}
	}
	return calc, nil
}

// Correct 在已确认基础上重新计算，生成新的确认版本并返回差额明细；
// 原版本保留为 CalcSuperseded，不被覆盖。
func (e *Engine) Correct(fundID, tradeDate string, baseNAV Decimal) (*Calculation, []TradeDiff, error) {
	prev := e.confirmedCalc(fundID, tradeDate)
	if prev == nil {
		return nil, nil, fmt.Errorf("swing: %s %s has no confirmed calculation to correct", fundID, tradeDate)
	}
	next, err := e.compute(fundID, tradeDate, baseNAV)
	if err != nil {
		return nil, nil, err
	}
	next.Version = prev.Version + 1
	next.Status = CalcConfirmed
	prev.Status = CalcSuperseded
	e.appendCalc(next)
	for _, id := range next.TradeIDs {
		if t, ok := e.trades.trades[id]; ok && t.Status == TradePending {
			t.Status = TradeConfirmed
		}
	}
	diffs := e.tradeDiffs(prev, next)
	return next, diffs, nil
}

// GetCalculation 查询指定版本快照。
func (e *Engine) GetCalculation(fundID, tradeDate string, version int) (*Calculation, error) {
	for _, c := range e.calcs[calcKey(fundID, tradeDate)] {
		if c.Version == version {
			return c, nil
		}
	}
	return nil, fmt.Errorf("swing: calculation %s %s v%d not found", fundID, tradeDate, version)
}

// Versions 返回某交易日全部计算版本（含被取代版本）。
func (e *Engine) Versions(fundID, tradeDate string) []*Calculation {
	src := e.calcs[calcKey(fundID, tradeDate)]
	out := make([]*Calculation, len(src))
	copy(out, src)
	return out
}

// DiffVersions 比较两个计算版本，返回汇总差异与逐笔差额明细。
func (e *Engine) DiffVersions(fundID, tradeDate string, fromVersion, toVersion int) (*CalcDiff, error) {
	from, err := e.GetCalculation(fundID, tradeDate, fromVersion)
	if err != nil {
		return nil, err
	}
	to, err := e.GetCalculation(fundID, tradeDate, toVersion)
	if err != nil {
		return nil, err
	}
	return &CalcDiff{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		OldSwungNAV: from.SwungNAV,
		NewSwungNAV: to.SwungNAV,
		NAVChange:   to.SwungNAV.Sub(from.SwungNAV),
		OldNetFlow:  from.NetFlow,
		NewNetFlow:  to.NetFlow,
		TradeDiffs:  e.tradeDiffs(from, to),
	}, nil
}

// AffectedTrades 返回指定版本相对其前一版本的受影响交易明细。
func (e *Engine) AffectedTrades(fundID, tradeDate string, version int) ([]TradeDiff, error) {
	to, err := e.GetCalculation(fundID, tradeDate, version)
	if err != nil {
		return nil, err
	}
	if version == 1 {
		return nil, errors.New("swing: version 1 has no predecessor")
	}
	from, err := e.GetCalculation(fundID, tradeDate, version-1)
	if err != nil {
		return nil, err
	}
	return e.tradeDiffs(from, to), nil
}

// Explain 说明某交易日是否触发摆动及计算依据。
func (e *Engine) Explain(fundID, tradeDate string) (string, error) {
	calc := e.confirmedCalc(fundID, tradeDate)
	if calc == nil {
		return "", fmt.Errorf("swing: %s %s has no confirmed calculation", fundID, tradeDate)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "基金 %s 交易日 %s（计算版本 v%d，规则版本 v%d）\n",
		calc.FundID, calc.TradeDate, calc.Version, calc.RuleVersion)
	fmt.Fprintf(&b, "基础净值 %s，申购总额 %s，赎回折算金额 %s，净资金流 %s，门槛 %s\n",
		calc.BaseNAV, calc.TotalSubscription, calc.TotalRedemption, calc.NetFlow, calc.Threshold)
	if !calc.Triggered {
		fmt.Fprintf(&b, "净资金流绝对值未超过门槛，未触发摆动，交易净值为基础净值 %s", calc.SwungNAV)
		return b.String(), nil
	}
	fmt.Fprintf(&b, "净资金流绝对值超过门槛，触发摆动（%s），调整比例 %s，摆动后净值 %s",
		calc.Direction, calc.AppliedRate, calc.SwungNAV)
	return b.String(), nil
}

// compute 冻结当前有效交易与基础净值，计算净资金流与摆动净值。
func (e *Engine) compute(fundID, tradeDate string, baseNAV Decimal) (*Calculation, error) {
	if baseNAV.Sign() <= 0 {
		return nil, errors.New("swing: base NAV must be positive")
	}
	rule, err := e.rules.EffectiveRule(fundID, tradeDate)
	if err != nil {
		return nil, err
	}
	calc := &Calculation{
		FundID:      fundID,
		TradeDate:   tradeDate,
		BaseNAV:     baseNAV,
		RuleVersion: rule.Version,
		Threshold:   rule.Threshold,
		Direction:   DirectionNone,
	}
	for _, t := range e.trades.activeTrades(fundID, tradeDate) {
		calc.TradeIDs = append(calc.TradeIDs, t.ID)
		switch t.Type {
		case Subscription:
			calc.TotalSubscription = calc.TotalSubscription.Add(t.Quantity)
		case Redemption:
			calc.TotalRedemption = calc.TotalRedemption.Add(t.Quantity.Mul(baseNAV))
		}
	}
	calc.NetFlow = calc.TotalSubscription.Sub(calc.TotalRedemption)
	calc.SwungNAV = baseNAV
	if calc.NetFlow.Abs().Cmp(rule.Threshold) > 0 {
		calc.Triggered = true
		var rate Decimal
		if calc.NetFlow.Sign() > 0 {
			calc.Direction = DirectionUp
			rate = rule.UpFactor
		} else {
			calc.Direction = DirectionDown
			rate = rule.DownFactor
		}
		if rate.Cmp(rule.MaxAdjustment) > 0 {
			rate = rule.MaxAdjustment
		}
		calc.AppliedRate = rate
		if calc.Direction == DirectionUp {
			calc.SwungNAV = baseNAV.Mul(DecimalFromInt(1).Add(rate))
		} else {
			calc.SwungNAV = baseNAV.Mul(DecimalFromInt(1).Sub(rate))
		}
	}
	return calc, nil
}

func (e *Engine) confirmedCalc(fundID, tradeDate string) *Calculation {
	for _, c := range e.calcs[calcKey(fundID, tradeDate)] {
		if c.Status == CalcConfirmed {
			return c
		}
	}
	return nil
}

func (e *Engine) appendCalc(c *Calculation) {
	k := calcKey(c.FundID, c.TradeDate)
	e.calcs[k] = append(e.calcs[k], c)
}

// tradeResult 按净值计算交易结果：申购得份额，赎回得金额。
func tradeResult(t TradeType, quantity, nav Decimal) Decimal {
	if t == Subscription {
		return quantity.Quo(nav)
	}
	return quantity.Mul(nav)
}

// tradeDiffs 计算 from -> to 两个版本间逐笔差额；仅存在于 from 的
// 交易按新净值重估（NewResult 为零表示不再参与），仅存在于 to 的
// 交易（如迟到申请）OldResult 为零。
func (e *Engine) tradeDiffs(from, to *Calculation) []TradeDiff {
	inFrom := make(map[string]bool, len(from.TradeIDs))
	for _, id := range from.TradeIDs {
		inFrom[id] = true
	}
	inTo := make(map[string]bool, len(to.TradeIDs))
	for _, id := range to.TradeIDs {
		inTo[id] = true
	}
	var diffs []TradeDiff
	appendDiff := func(id string) {
		t, ok := e.trades.Get(id)
		if !ok {
			return
		}
		d := TradeDiff{
			TradeID:  id,
			Type:     t.Type,
			Quantity: t.Quantity,
			OldNAV:   from.SwungNAV,
			NewNAV:   to.SwungNAV,
		}
		if inFrom[id] {
			d.OldResult = tradeResult(t.Type, t.Quantity, from.SwungNAV)
		}
		if inTo[id] {
			d.NewResult = tradeResult(t.Type, t.Quantity, to.SwungNAV)
		}
		d.Difference = d.NewResult.Sub(d.OldResult)
		diffs = append(diffs, d)
	}
	for _, id := range from.TradeIDs {
		appendDiff(id)
	}
	for _, id := range to.TradeIDs {
		if !inFrom[id] {
			appendDiff(id)
		}
	}
	return diffs
}
