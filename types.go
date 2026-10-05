package fundoperations

import "time"

// 金额、净值与份额的精度规则（固定，全系统统一）：
//
//   - 金额（Amount/ConfirmedAmount/RoundingRemainder）单位为 0.01 元（分）。
//   - 净值（NAV.Value）单位为 0.0001 元（万分位）。
//   - 份额（Shares）单位为 0.01 份。
//
// 确认份额 = floor(金额 * 10000 / 净值)，一律向下舍入；
// 确认金额 = floor(份额 * 净值 / 10000)；
// 舍入差额 = 申购金额 - 确认金额，按固定方式留存于基金资产，
// 并逐笔记录在申请与批次快照中，保证可追溯。
const (
	MoneyScale = 100   // 金额单位：分
	NAVScale   = 10000 // 净值单位：1e-4 元
	ShareScale = 100   // 份额单位：0.01 份
)

// FundStatus 基金交易状态。
type FundStatus string

const (
	FundStatusTrading   FundStatus = "TRADING"   // 正常受理
	FundStatusSuspended FundStatus = "SUSPENDED" // 暂停交易
)

// Fund 基金产品。
type Fund struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          FundStatus `json:"status"`
	Currency        string     `json:"currency"`         // 交易币种，如 CNY
	MinSubscription int64      `json:"min_subscription"` // 申购起点，单位：分
	Calendar        *Calendar  `json:"calendar"`         // 交易日历
	CutoffTime      string     `json:"cutoff_time"`      // 每日交易截止时间，"HH:MM"
	Timezone        string     `json:"timezone"`         // 截止判断所用时区，如 "Asia/Shanghai"
}

// ApplicationStatus 申请状态。
type ApplicationStatus string

const (
	StatusPending   ApplicationStatus = "PENDING"   // 已进入待处理批次
	StatusConfirmed ApplicationStatus = "CONFIRMED" // 已按冻结净值确认份额
)

// SubscriptionRequest 申购申请请求。
type SubscriptionRequest struct {
	FundID      string
	ExternalID  string // 外部申请号（幂等键）
	InvestorID  string
	Amount      int64 // 申购金额，单位：分
	Currency    string
	SubmittedAt time.Time // 提交时间；零值则使用系统统一时钟
}

// Application 申购申请及其确认结果。
type Application struct {
	FundID            string            `json:"fund_id"`
	ExternalID        string            `json:"external_id"`
	InvestorID        string            `json:"investor_id"`
	Amount            int64             `json:"amount"` // 申购金额，单位：分
	Currency          string            `json:"currency"`
	SubmittedAt       time.Time         `json:"submitted_at"`
	ValuationDate     string            `json:"valuation_date"` // 归入的估值日
	BatchID           string            `json:"batch_id"`
	Status            ApplicationStatus `json:"status"`
	NAVVersion        int               `json:"nav_version"`        // 确认时采用的净值版本
	ConfirmedAmount   int64             `json:"confirmed_amount"`   // 确认金额，单位：分
	Shares            int64             `json:"shares"`             // 确认份额，单位：0.01 份
	RoundingRemainder int64             `json:"rounding_remainder"` // 舍入差额，单位：分
}

// BatchStatus 批次状态。
type BatchStatus string

const (
	BatchOpen      BatchStatus = "OPEN"      // 受理中
	BatchClosed    BatchStatus = "CLOSED"    // 已关闭，申请集合与净值版本已冻结
	BatchConfirmed BatchStatus = "CONFIRMED" // 已完成份额确认
)

// Batch 估值日批次快照。关闭后申请集合、估值日与净值版本不可变更。
type Batch struct {
	ID                   string      `json:"id"` // 基金ID/估值日
	FundID               string      `json:"fund_id"`
	ValuationDate        string      `json:"valuation_date"`
	Status               BatchStatus `json:"status"`
	ExternalIDs          []string    `json:"external_ids"` // 冻结的申请集合
	NAVVersion           int         `json:"nav_version"`
	NAV                  int64       `json:"nav"` // 冻结的净值，单位：1e-4 元
	TotalAmount          int64       `json:"total_amount"`
	TotalShares          int64       `json:"total_shares"`
	TotalConfirmedAmount int64       `json:"total_confirmed_amount"`
	TotalRemainder       int64       `json:"total_remainder"`
	ClosedAt             time.Time   `json:"closed_at"`
	ConfirmedAt          time.Time   `json:"confirmed_at"`
}

// NAV 基金某日净值的一个版本。净值更正会产生新版本，
// 已确认的批次仍引用冻结的旧版本，原结果不被改写。
type NAV struct {
	FundID      string    `json:"fund_id"`
	Date        string    `json:"date"`
	Value       int64     `json:"value"` // 单位：1e-4 元
	Version     int       `json:"version"`
	PublishedAt time.Time `json:"published_at"`
}
