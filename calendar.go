package fundoperations

import "time"

// DateLayout 估值日/交易日字符串格式。
const DateLayout = "2006-01-02"

// Calendar 交易日历，记录哪些日期是交易日。
type Calendar struct {
	TradingDays map[string]bool `json:"trading_days"`
}

// NewCalendar 以交易日日期串（"2006-01-02"）构造日历。
func NewCalendar(days ...string) *Calendar {
	c := &Calendar{TradingDays: make(map[string]bool, len(days))}
	for _, d := range days {
		c.TradingDays[d] = true
	}
	return c
}

// IsTradingDay 判断某日是否为交易日。
func (c *Calendar) IsTradingDay(date string) bool {
	return c != nil && c.TradingDays[date]
}

// NextTradingDay 返回 from 所在日期之后（不含当日）的下一个交易日。
func (c *Calendar) NextTradingDay(from time.Time) (string, error) {
	for i := 1; i <= 370; i++ {
		d := from.AddDate(0, 0, i).Format(DateLayout)
		if c.IsTradingDay(d) {
			return d, nil
		}
	}
	return "", ErrNoTradingDay
}
