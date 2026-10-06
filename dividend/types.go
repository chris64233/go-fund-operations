// Package dividend 实现开放式基金的分红与红利再投资。
//
// 精度约定(全部使用定点整数,避免浮点误差):
//   - Money      金额,单位为分(0.01 元)
//   - Shares     份额,单位为 0.01 份
//   - UnitAmount 单价(每份分红金额、除息净值),单位为 1e-6 元
//
// 固定舍入规则(全局一致,不随方案变化):
//   - 现金红利 = 登记份额 × 每份分红,四舍五入(half-up)到分;
//   - 再投资新增份额 = 现金红利 ÷ 除息净值,向下取整到 0.01 份;
//   - 再投资折算差额(应发金额 − 新增份额×净值,四舍五入到分)以现金补齐,
//     因此任意方案均满足:Σ明细应发 = Σ实发现金 + Σ新增份额折算金额 = 方案总额。
package dividend

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrPlanNotFound        = errors.New("dividend: 分红方案不存在")
	ErrPlanExists          = errors.New("dividend: 分红方案号已存在")
	ErrPlanNotConfirmed    = errors.New("dividend: 方案未确认,不能执行")
	ErrPlanCancelled       = errors.New("dividend: 方案已取消")
	ErrNotCancellable      = errors.New("dividend: 方案已开始或完成执行,不能取消")
	ErrChoiceLocked        = errors.New("dividend: 执行开始后不能修改领取方式")
	ErrExecutionConflict   = errors.New("dividend: 执行参数与已登记方案不一致")
	ErrInvalidStatus       = errors.New("dividend: 当前状态不允许该操作")
	ErrBackdatedTrade      = errors.New("dividend: 登记日快照已冻结,拒绝补登该日及之前的交易")
	ErrInsufficientShares  = errors.New("dividend: 可用份额不足")
	ErrDuplicateTrade      = errors.New("dividend: 交易号重复")
	ErrDuplicateCorrection = errors.New("dividend: 更正记录号重复")
	ErrInvalidArgument     = errors.New("dividend: 参数非法")
)

// Date 为不含时区的日历日。
type Date struct{ t time.Time }

// ParseDate 按 "2006-01-02" 解析日期。
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("dividend: 非法日期 %q: %w", s, err)
	}
	return Date{t: t}, nil
}

// MustDate 解析日期,失败时 panic,仅供测试与常量使用。
func MustDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Date) String() string     { return d.t.Format("2006-01-02") }
func (d Date) Before(o Date) bool { return d.t.Before(o.t) }
func (d Date) After(o Date) bool  { return d.t.After(o.t) }
func (d Date) Equal(o Date) bool  { return d.t.Equal(o.t) }

// Money 金额,单位为分。
type Money int64

func (m Money) String() string {
	sign := ""
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

// Shares 份额,单位为 0.01 份。
type Shares int64

func (s Shares) String() string {
	sign := ""
	if s < 0 {
		sign = "-"
		s = -s
	}
	return fmt.Sprintf("%s%d.%02d", sign, s/100, s%100)
}

// UnitAmount 单价,单位为 1e-6 元。
type UnitAmount int64

func (u UnitAmount) String() string {
	return fmt.Sprintf("%d.%06d", u/1000000, u%1000000)
}

// PayoutMethod 红利领取方式。
type PayoutMethod string

const (
	PayoutCash     PayoutMethod = "CASH"     // 现金领取
	PayoutReinvest PayoutMethod = "REINVEST" // 红利再投资
)

// PlanStatus 分红方案执行状态。
type PlanStatus string

const (
	PlanDraft     PlanStatus = "DRAFT"     // 已创建,未确认
	PlanConfirmed PlanStatus = "CONFIRMED" // 已确认,登记快照已冻结
	PlanExecuting PlanStatus = "EXECUTING" // 执行中(中断后停留于此)
	PlanExecuted  PlanStatus = "EXECUTED"  // 执行完成
	PlanCancelled PlanStatus = "CANCELLED" // 执行前取消
)

// AllocationStatus 分配明细状态。
type AllocationStatus string

const (
	AllocationPending AllocationStatus = "PENDING" // 待入账
	AllocationDone    AllocationStatus = "DONE"    // 已入账
)

// Plan 分红方案。
type Plan struct {
	PlanID           string     // 方案号,幂等键
	FundID           string     // 基金代码
	RecordDate       Date       // 登记日
	ExDate           Date       // 除息日
	DividendPerShare UnitAmount // 每份分红金额
	ExNav            UnitAmount // 除息日净值(再投资折算价)
	Status           PlanStatus // 执行状态
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SnapshotRow 登记日持有人份额快照,方案确认时冻结。
type SnapshotRow struct {
	PlanID   string
	FundID   string
	HolderID string
	Shares   Shares // 登记日日终持有份额
	FrozenAt time.Time
}

// Choice 持有人领取方式选择,执行开始前可修改。
type Choice struct {
	PlanID    string
	HolderID  string
	Method    PayoutMethod
	UpdatedAt time.Time
}

// Allocation 每名持有人的最终分配明细,同一方案内按持有人唯一。
type Allocation struct {
	PlanID         string
	FundID         string
	HolderID       string
	Method         PayoutMethod
	BaseShares     Shares // 登记份额
	Entitlement    Money  // 应发红利总额
	CashAmount     Money  // 实发现金(再投资时为折算剩余)
	ReinvestShares Shares // 再投资新增份额
	Status         AllocationStatus
	FinishedAt     time.Time
}

// ExecutionResult 方案执行结果,重复执行时原样返回。
type ExecutionResult struct {
	PlanID              string
	Status              PlanStatus
	HolderCount         int
	TotalEntitlement    Money // 方案总额 = Σ明细应发
	TotalCash           Money // 实发现金合计(含再投资折算剩余)
	TotalReinvestShares Shares
	FinishedAt          time.Time
}

// StatusChange 方案状态变化记录,每次迁移持久化一条。
type StatusChange struct {
	PlanID string
	From   PlanStatus
	To     PlanStatus
	Reason string
	At     time.Time
}

// Correction 独立更正记录:已执行分红的差错只能通过更正调整,
// 不得删除或修改既有分配明细。
type Correction struct {
	CorrectionID string
	PlanID       string
	HolderID     string
	CashDelta    Money
	ShareDelta   Shares
	Reason       string
	CreatedAt    time.Time
}

// PostingKind 账务流水类型。
type PostingKind string

const (
	PostingTrade        PostingKind = "TRADE"         // 申购/赎回
	PostingDividendCash PostingKind = "DIVIDEND_CASH" // 现金分红
	PostingReinvest     PostingKind = "REINVEST"      // 红利再投资份额
	PostingCorrection   PostingKind = "CORRECTION"    // 更正
)

// Posting 账户流水,带幂等键,重复应用不会产生重复入账。
type Posting struct {
	Key        string // 幂等键
	FundID     string
	HolderID   string
	Date       Date
	ShareDelta Shares
	CashDelta  Money
	Kind       PostingKind
	Ref        string // 关联交易号/方案号/更正号
}

// cashEntitlement 现金红利(分) = 份额(0.01份) × 每份分红(1e-6元) ÷ 1e6,四舍五入。
func cashEntitlement(shares Shares, perShare UnitAmount) Money {
	return Money((int64(shares)*int64(perShare) + 500000) / 1000000)
}

// reinvestShares 再投资份额(0.01份) = 现金(分) × 1e6 ÷ 净值(1e-6元),向下取整。
func reinvestShares(cash Money, nav UnitAmount) Shares {
	if nav <= 0 {
		return 0
	}
	return Shares(int64(cash) * 1000000 / int64(nav))
}

// sharesValue 份额按净值折算的现金(分),四舍五入。
func sharesValue(shares Shares, nav UnitAmount) Money {
	return Money((int64(shares)*int64(nav) + 500000) / 1000000)
}
