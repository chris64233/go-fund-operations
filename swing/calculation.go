package swing

import "fmt"

// Direction 摆动方向。
type Direction int

const (
	// DirectionNone 未触发摆动。
	DirectionNone Direction = iota
	// DirectionUp 净申购净流入，净值上调。
	DirectionUp
	// DirectionDown 净赎回净流出，净值下调。
	DirectionDown
)

func (d Direction) String() string {
	switch d {
	case DirectionUp:
		return "UP"
	case DirectionDown:
		return "DOWN"
	default:
		return "NONE"
	}
}

// CalcStatus 计算结果状态。
type CalcStatus int

const (
	// CalcDraft 试算结果，未确认，不用于交易。
	CalcDraft CalcStatus = iota + 1
	// CalcConfirmed 已确认结果，当日交易统一使用其摆动净值。
	CalcConfirmed
)

// TradeDelta 更正版本相对上一版本的单笔交易差额明细。
type TradeDelta struct {
	TradeID     string
	Change      string // "ADDED"（新纳入快照）/ "REMOVED"（撤销剔除）
	Type        TradeType
	Amount      Decimal
	OldPrice    Decimal // 上一确认版本使用的净值
	NewPrice    Decimal // 本版本使用的净值
	AmountDelta Decimal // 该笔交易对净资金流的影响（申购为正、赎回为负）
}

// Calculation 一次日终摆动定价计算（试算或确认版本）。
// 确认后的版本不可修改；更正只能生成新版本。
type Calculation struct {
	FundID       string
	TradeDate    string
	Version      int          // 该基金该交易日的计算版本号，从 1 递增
	Status       CalcStatus   // 试算或已确认
	RuleVersion  int          // 引用的规则版本
	BaseNAV      Decimal      // 冻结的基础净值
	TotalSub     Decimal      // 冻结的有效申购总额
	TotalRed     Decimal      // 冻结的有效赎回总额
	NetFlow      Decimal      // 净资金流 = 申购 - 赎回
	Threshold    Decimal      // 快照时点的门槛
	Triggered    bool         // 是否触发摆动
	Direction    Direction    // 摆动方向
	AdjustFactor Decimal      // 实际采用的调整比例（受最大幅度封顶）
	AdjustedNAV  Decimal      // 摆动后净值
	TradeIDs     []string     // 冻结的交易快照
	Steps        []string     // 计算过程说明
	Supersedes   int          // 更正时指向被替代的版本号，首版为 0
	Deltas       []TradeDelta // 更正版本相对上一版本的差额明细
}

// compute 是摆动定价核心：给定冻结的申购、赎回、基础净值与规则，
// 先算净资金流，再判断是否触发摆动并得出调整后净值。
func compute(fundID, tradeDate string, baseNAV, totalSub, totalRed Decimal, rule SwingRule) Calculation {
	c := Calculation{
		FundID:      fundID,
		TradeDate:   tradeDate,
		RuleVersion: rule.Version,
		BaseNAV:     baseNAV,
		TotalSub:    totalSub,
		TotalRed:    totalRed,
		Threshold:   rule.Threshold,
		Direction:   DirectionNone,
	}
	c.NetFlow = totalSub.Sub(totalRed)
	c.Steps = append(c.Steps,
		fmt.Sprintf("冻结当日有效申购 %s、有效赎回 %s、基础净值 %s", totalSub, totalRed, baseNAV),
		fmt.Sprintf("净资金流 = %s - %s = %s", totalSub, totalRed, c.NetFlow),
		fmt.Sprintf("引用规则版本 %d：门槛 %s，上调比例 %s，下调比例 %s，最大调整幅度 %s",
			rule.Version, rule.Threshold, rule.UpFactor, rule.DownFactor, rule.MaxAdjustment),
	)

	absFlow := c.NetFlow.Abs()
	if absFlow.Cmp(rule.Threshold) <= 0 {
		c.Triggered = false
		c.AdjustFactor = DecimalFromInt(0)
		c.AdjustedNAV = baseNAV
		c.Steps = append(c.Steps,
			fmt.Sprintf("|净资金流| %s 未超过门槛 %s，不触发摆动，净值维持 %s", absFlow, rule.Threshold, baseNAV))
		return c
	}

	c.Triggered = true
	one := DecimalFromInt(1)
	if c.NetFlow.Sign() > 0 {
		c.Direction = DirectionUp
		c.AdjustFactor = minDecimal(rule.UpFactor, rule.MaxAdjustment)
		c.AdjustedNAV = baseNAV.Mul(one.Add(c.AdjustFactor))
		c.Steps = append(c.Steps,
			fmt.Sprintf("|净资金流| %s 超过门槛 %s，触发摆动", absFlow, rule.Threshold),
			fmt.Sprintf("净流入，按上调比例 %s（封顶 %s）取 %s", rule.UpFactor, rule.MaxAdjustment, c.AdjustFactor),
			fmt.Sprintf("调整后净值 = %s * (1 + %s) = %s", baseNAV, c.AdjustFactor, c.AdjustedNAV),
		)
	} else {
		c.Direction = DirectionDown
		c.AdjustFactor = minDecimal(rule.DownFactor, rule.MaxAdjustment)
		c.AdjustedNAV = baseNAV.Mul(one.Sub(c.AdjustFactor))
		c.Steps = append(c.Steps,
			fmt.Sprintf("|净资金流| %s 超过门槛 %s，触发摆动", absFlow, rule.Threshold),
			fmt.Sprintf("净流出，按下调比例 %s（封顶 %s）取 %s", rule.DownFactor, rule.MaxAdjustment, c.AdjustFactor),
			fmt.Sprintf("调整后净值 = %s * (1 - %s) = %s", baseNAV, c.AdjustFactor, c.AdjustedNAV),
		)
	}
	return c
}

func minDecimal(a, b Decimal) Decimal {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
