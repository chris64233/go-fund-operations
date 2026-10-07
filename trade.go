package fundoperations

import (
	"errors"
	"fmt"
	"sort"
)

// TradeType 交易类型。
type TradeType int

const (
	// Subscription 申购，Quantity 为申购金额（元）。
	Subscription TradeType = iota + 1
	// Redemption 赎回，Quantity 为赎回份额（份）。
	Redemption
)

func (t TradeType) String() string {
	switch t {
	case Subscription:
		return "申购"
	case Redemption:
		return "赎回"
	}
	return "未知"
}

// TradeStatus 交易状态。
type TradeStatus int

const (
	// TradePending 已受理，等待日终确认。
	TradePending TradeStatus = iota + 1
	// TradeConfirmed 已按确认净值成交。
	TradeConfirmed
	// TradeCancelled 已撤销，不参与日终计算。
	TradeCancelled
)

// Trade 一笔申购或赎回申请。
type Trade struct {
	ID        string
	FundID    string
	TradeDate string // YYYY-MM-DD
	Type      TradeType
	Quantity  Decimal // 申购为金额，赎回为份额
	Status    TradeStatus
	Late      bool // 确认后到达的迟到申请
}

// TradeStore 交易存储。
type TradeStore struct {
	trades map[string]*Trade
	order  []string
}

// NewTradeStore 创建空交易库。
func NewTradeStore() *TradeStore {
	return &TradeStore{trades: make(map[string]*Trade)}
}

// Submit 登记一笔交易申请。
func (s *TradeStore) Submit(t Trade) (*Trade, error) {
	if t.ID == "" || t.FundID == "" || t.TradeDate == "" {
		return nil, errors.New("swing: trade id, fund id and trade date required")
	}
	if t.Type != Subscription && t.Type != Redemption {
		return nil, errors.New("swing: invalid trade type")
	}
	if t.Quantity.Sign() <= 0 {
		return nil, errors.New("swing: quantity must be positive")
	}
	if _, exists := s.trades[t.ID]; exists {
		return nil, fmt.Errorf("swing: trade %s already exists", t.ID)
	}
	t.Status = TradePending
	c := t
	s.trades[t.ID] = &c
	s.order = append(s.order, t.ID)
	out := c
	return &out, nil
}

// Cancel 撤销未确认交易。
func (s *TradeStore) Cancel(tradeID string) error {
	t, ok := s.trades[tradeID]
	if !ok {
		return fmt.Errorf("swing: trade %s not found", tradeID)
	}
	if t.Status == TradeConfirmed {
		return errors.New("swing: confirmed trade cannot be cancelled")
	}
	t.Status = TradeCancelled
	return nil
}

// Get 查询交易。
func (s *TradeStore) Get(tradeID string) (*Trade, bool) {
	t, ok := s.trades[tradeID]
	if !ok {
		return nil, false
	}
	c := *t
	return &c, true
}

// MarkLate 将交易标记为迟到（确认后到达的申请）。
func (s *TradeStore) MarkLate(tradeID string) error {
	t, ok := s.trades[tradeID]
	if !ok {
		return fmt.Errorf("swing: trade %s not found", tradeID)
	}
	t.Late = true
	return nil
}

// activeTrades 返回某基金某交易日未撤销的交易（按 ID 排序）。
func (s *TradeStore) activeTrades(fundID, tradeDate string) []*Trade {
	var out []*Trade
	for _, id := range s.order {
		t := s.trades[id]
		if t.FundID == fundID && t.TradeDate == tradeDate && t.Status != TradeCancelled {
			c := *t
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
