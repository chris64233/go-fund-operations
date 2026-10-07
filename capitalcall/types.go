package capitalcall

import "time"

// Money 以最小货币单位（分）表示金额，避免浮点误差。
type Money int64

// Commitment 投资者出资承诺。
type Commitment struct {
	ID        string
	Investor  string
	Currency  string
	Total     Money // 承诺总额
	Called    Money // 已调用金额（含已通知未缴与已实缴）
	Active    bool  // 有效状态
	CreatedAt time.Time
}

// Remaining 未调用承诺额度。
func (c *Commitment) Remaining() Money { return c.Total - c.Called }

// NoticeStatus 缴款通知状态。
type NoticeStatus string

const (
	NoticeOpen      NoticeStatus = "OPEN"
	NoticeCancelled NoticeStatus = "CANCELLED"
	NoticeCompleted NoticeStatus = "COMPLETED"
)

// Notice 缴款通知。
type Notice struct {
	ID        string
	Purpose   string
	Currency  string
	Target    Money // 目标总额
	DueDate   time.Time
	Status    NoticeStatus
	CreatedAt time.Time
}

// Allocation 投资者在一份通知下的应缴分配。
type Allocation struct {
	ID         string
	NoticeID   string
	Investor   string
	Amount     Money // 应缴金额
	Paid       Money // 累计实缴金额
	Commitment string // 冻结的承诺 ID
}

// Outstanding 应缴未缴金额。
func (a *Allocation) Outstanding() Money { return a.Amount - a.Paid }

// Payment 一次缴款确认，保留银行流水与对应分配。
type Payment struct {
	ID           string
	NoticeID     string
	AllocationID string
	Investor     string
	BankRef      string // 银行流水号，幂等键
	Amount       Money
	ConfirmedAt  time.Time
}

// Position 投资者额度视图。
type Position struct {
	Investor  string
	Currency  string
	Total     Money // 承诺总额
	Uncalled  Money // 未调用
	Notified  Money // 已通知未缴
	Paid      Money // 已实缴
	Called    Money // 已调用 = 已通知未缴 + 已实缴
	Remaining Money // 剩余可调用额度
}
