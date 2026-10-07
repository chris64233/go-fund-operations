package sidepocket

import "time"

// PlanStatus 表示侧袋方案的生命周期状态。
type PlanStatus string

const (
	PlanStatusDraft     PlanStatus = "DRAFT"
	PlanStatusConfirmed PlanStatus = "CONFIRMED"
)

// Plan 记录一次侧袋划分方案：基金、目标资产、登记日、
// 资产估值（即侧袋权益总量）、主份额冻结比例与方案版本。
type Plan struct {
	ID         string
	FundID     string
	AssetID    string
	RecordDate time.Time
	// Valuation 为目标资产估值对应的侧袋权益总量（最小单位）。
	Valuation int64
	// RatioNum/RatioDen 为主基金份额冻结比例（RatioNum/RatioDen）。
	RatioNum int64
	RatioDen int64
	Version  int
	Status   PlanStatus
	// TotalEntitlement 为确认后实际生成的侧袋权益合计，必须等于 Valuation。
	TotalEntitlement int64
}

// Holding 为投资者在基金中的主基金份额及其生效日期。
// 生效日期不晚于方案登记日的份额才参与侧袋划分。
type Holding struct {
	FundID        string
	InvestorID    string
	Shares        int64
	EffectiveDate time.Time
}

// Entitlement 为投资者在某方案下持有的侧袋权益。
// 侧袋权益独立于主基金份额，不参与主基金可赎回数量计算。
type Entitlement struct {
	PlanID     string
	InvestorID string
	Units      int64
}

// Recovery 为目标资产的一次现金回收。
type Recovery struct {
	ID      string
	PlanID  string
	Amount  int64
	Scanned bool
}

// DistributionDetail 为一次回收按当前有效侧袋权益生成的分配明细。
type DistributionDetail struct {
	RecoveryID string
	PlanID     string
	InvestorID string
	Units      int64
	Amount     int64
}
