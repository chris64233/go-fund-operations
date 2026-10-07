// Package liquidation 实现基金终止后的清算分配：
// 清算方案、份额快照、首笔与补充分配、准备金释放、
// 幂等付款执行与并发状态控制。
package liquidation

import "time"

// Money 以最小货币单位（分）表示的整数金额，避免浮点误差。
type Money = int64

// PlanStatus 清算方案状态机。
type PlanStatus string

const (
	PlanDraft      PlanStatus = "DRAFT"      // 草案，可修改
	PlanConfirmed  PlanStatus = "CONFIRMED"  // 已确认，份额快照冻结
	PlanCancelled  PlanStatus = "CANCELLED"  // 已取消，终态
	PlanInProgress PlanStatus = "IN_PROCESS" // 已开始分配
	PlanCompleted  PlanStatus = "COMPLETED"  // 全部分配完成
)

// BatchStatus 分配批次状态。
type BatchStatus string

const (
	BatchOpen      BatchStatus = "OPEN"      // 已生成，待执行
	BatchExecuting BatchStatus = "EXECUTING" // 执行中
	BatchDone      BatchStatus = "DONE"      // 全部明细已终态
)

// BatchKind 分配批次类型。
type BatchKind string

const (
	BatchFirst        BatchKind = "FIRST"        // 首笔分配
	BatchSupplemental BatchKind = "SUPPLEMENTAL" // 准备金释放后的补充分配
)

// PaymentStatus 付款明细状态。
type PaymentStatus string

const (
	PayPending PaymentStatus = "PENDING" // 待付款
	PayPaid    PaymentStatus = "PAID"    // 已付款（终态）
	PayOnHold  PaymentStatus = "ON_HOLD" // 投资者账户异常，待人工处理
	PayFailed  PaymentStatus = "FAILED"  // 付款失败，可重试
)

// AccountStatus 投资者账户状态。
type AccountStatus string

const (
	AccountActive AccountStatus = "ACTIVE"
	AccountExited AccountStatus = "EXITED"
	AccountFrozen AccountStatus = "FROZEN"
)

// SnapshotEntry 登记日冻结的份额快照条目。
type SnapshotEntry struct {
	InvestorID string
	Shares     int64 // 份额，最小单位
}

// Plan 清算方案。
type Plan struct {
	ID                string
	FundID            string
	RecordDate        time.Time // 登记日
	Distributable     Money     // 可分配现金
	FeeReserve        Money     // 待支付费用准备金
	DisputeReserve    Money     // 争议准备金
	Status            PlanStatus
	Snapshot          []SnapshotEntry // 确认时冻结
	TotalShares       int64
	ReleasedFee       Money // 已释放的费用准备金
	ReleasedDispute   Money // 已释放的争议准备金
	DistributedTotal  Money // 已进入批次的应付总额
	RoundingRemainder Money // 累计舍入尾差
}

// TotalAssets 清算资产总额 = 可分配现金 + 两项准备金。
func (p *Plan) TotalAssets() Money {
	return p.Distributable + p.FeeReserve + p.DisputeReserve
}

// RemainingAssets 尚未进入任何分配批次的资产（含未释放准备金与尾差）。
func (p *Plan) RemainingAssets() Money {
	return p.TotalAssets() - p.DistributedTotal - p.RoundingRemainder
}

// PaymentItem 单名投资者在一个批次中的付款明细。
type PaymentItem struct {
	ID             string
	BatchID        string
	InvestorID     string
	Shares         int64
	Amount         Money
	Status         PaymentStatus
	IdempotencyKey string
	PaidAt         *time.Time
	Attempts       int
}

// Batch 分配批次。
type Batch struct {
	ID        string
	PlanID    string
	Kind      BatchKind
	Seq       int
	Pool      Money // 本批次分配池
	Rounding  Money // 本批次舍入尾差（未分配部分）
	Status    BatchStatus
	Items     []*PaymentItem
	CreatedAt time.Time
}

// BatchProgress 付款进度查询结果。
type BatchProgress struct {
	BatchID     string
	Kind        BatchKind
	Status      BatchStatus
	Total       int
	Paid        int
	OnHold      int
	Failed      int
	Pending     int
	PaidAmount  Money
	TotalAmount Money
}
