package swing

// TradeType 交易类型。
type TradeType int

const (
	// Subscription 申购（资金流入）。
	Subscription TradeType = iota + 1
	// Redemption 赎回（资金流出）。
	Redemption
)

func (t TradeType) String() string {
	switch t {
	case Subscription:
		return "SUBSCRIPTION"
	case Redemption:
		return "REDEMPTION"
	default:
		return "UNKNOWN"
	}
}

// TradeStatus 交易状态。
type TradeStatus int

const (
	// TradeActive 有效交易，参与日终快照。
	TradeActive TradeStatus = iota + 1
	// TradeCancelled 已撤销交易，不参与后续计算。
	TradeCancelled
)

// Trade 一笔申购或赎回申请。
type Trade struct {
	ID        string
	FundID    string
	TradeDate string // 交易日 YYYY-MM-DD
	Type      TradeType
	Amount    Decimal // 金额，精确表示
	Status    TradeStatus
	// Seq 申请到达顺序号，用于区分确认前/确认后（迟到）申请。
	Seq int64
}
