// Package corporateaction 实现基金份额拆分(share split)与份额合并(share merge)
// 的公司行动全生命周期:登记 → 确认 → 执行 → (执行前)取消。
//
// # 精度约定(定点数,避免浮点误差)
//   - Money       金额,单位 分(0.01 元)
//   - Shares      份额,单位 0.01 份
//   - MicroShares 零碎份额,单位 1e-6 份(仅用于保留差额来源)
//   - UnitAmount  单价,单位 1e-6 元
//
// # 转换比例
//
// 比例以正整数分数 Ratio{Num, Den} 表示:每份旧份额兑换 Num/Den 份新份额。
// Num > Den 为拆分,Num < Den 为合并,登记时按最大公约数归一化,
// 因此 2/1 与 4/2 视为同一比例(重复登记返回原结果)。
//
// # 零碎份额规则(固定,全局一致)
//
// 新份额 = 旧份额 × Num ÷ Den,向下取整到 0.01 份;无法整除被舍去的
// 零碎份额以 1e-6 份精度保留在转换明细中(FractionalShares),
// 并按基金份额面值 1.00 元/份折算现金返还持有人(CashInLieu,四舍五入到分)。
// 每条转换明细均可追溯原持有人、原数量、零碎差额来源与行动版本。
//
// # 登记日快照
//
// 行动确认时按登记日日终冻结持有人份额(含冻结份额):
//   - 申购/赎回/份额冻结的生效日早于或等于登记日:纳入快照;
//   - 生效日晚于登记日(即使早于转换生效日):不纳入本次转换;
//   - 快照冻结后,拒绝补登生效日早于或等于登记日的交易或冻结;
//   - 登记日(含)前冻结的份额按同一比例转换,转换后继续冻结;
//   - 执行仅按快照数量进行,生效日之后的新交易不会被重复转换。
package corporateaction

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrActionNotFound     = errors.New("corporateaction: 公司行动不存在")
	ErrActionExists       = errors.New("corporateaction: 行动号已登记")
	ErrActionConflict     = errors.New("corporateaction: 重复登记内容与已存在行动不一致")
	ErrOverlappingAction  = errors.New("corporateaction: 同一基金同一份额类型在重叠生效期间已有行动")
	ErrActionNotConfirmed = errors.New("corporateaction: 行动未确认,不能执行")
	ErrActionCancelled    = errors.New("corporateaction: 行动已取消")
	ErrNotCancellable     = errors.New("corporateaction: 行动已开始或完成执行,不能取消")
	ErrExecutionConflict  = errors.New("corporateaction: 执行参数与已登记行动不一致")
	ErrInvalidStatus      = errors.New("corporateaction: 当前状态不允许该操作")
	ErrBackdatedEntry     = errors.New("corporateaction: 登记日快照已冻结,拒绝补登该日及之前的交易或冻结")
	ErrInsufficientShares = errors.New("corporateaction: 可用份额不足")
	ErrInsufficientFreeze = errors.New("corporateaction: 可解冻份额不足")
	ErrDuplicateTrade     = errors.New("corporateaction: 交易号重复")
	ErrDuplicateFreeze    = errors.New("corporateaction: 冻结请求号重复")
	ErrInvalidAmount      = errors.New("corporateaction: 金额、份额或比例非法")
	ErrInvalidDate        = errors.New("corporateaction: 生效日不得早于登记日")
)

// Date 为不含时区的日历日。
type Date struct{ t time.Time }

func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("corporateaction: 非法日期 %q: %w", s, err)
	}
	return Date{t: t}, nil
}

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

// Money 金额,单位分。
type Money int64

func (m Money) String() string {
	sign := ""
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

// Shares 份额,单位 0.01 份。
type Shares int64

func (s Shares) String() string {
	sign := ""
	if s < 0 {
		sign = "-"
		s = -s
	}
	return fmt.Sprintf("%s%d.%02d", sign, s/100, s%100)
}

// MicroShares 零碎份额,单位 1e-6 份。
type MicroShares int64

func (m MicroShares) String() string {
	return fmt.Sprintf("%d.%06d", m/1000000, m%1000000)
}

// Ratio 转换比例:每份旧份额兑换 Num/Den 份新份额。
type Ratio struct {
	Num int64 // 新份额分子
	Den int64 // 旧份额分母
}

// Normalize 按最大公约数归一化比例。比例必须为正。
func (r Ratio) Normalize() (Ratio, error) {
	if r.Num <= 0 || r.Den <= 0 {
		return Ratio{}, fmt.Errorf("%w: 比例必须为正(Num=%d,Den=%d)", ErrInvalidAmount, r.Num, r.Den)
	}
	g := gcd(r.Num, r.Den)
	return Ratio{Num: r.Num / g, Den: r.Den / g}, nil
}

func (r Ratio) Equal(o Ratio) bool {
	a, _ := r.Normalize()
	b, _ := o.Normalize()
	return a == b
}

func (r Ratio) String() string { return fmt.Sprintf("%d:%d", r.Num, r.Den) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Direction 转换方向。
type Direction string

const (
	DirectionSplit Direction = "SPLIT" // 拆分:Num > Den
	DirectionMerge Direction = "MERGE" // 合并:Num < Den
	DirectionSame  Direction = "SAME"  // 1:1 等比例
)

func (r Ratio) Direction() Direction {
	switch {
	case r.Num > r.Den:
		return DirectionSplit
	case r.Num < r.Den:
		return DirectionMerge
	default:
		return DirectionSame
	}
}

// ActionStatus 公司行动状态。
type ActionStatus string

const (
	ActionDraft     ActionStatus = "DRAFT"     // 已登记,未确认
	ActionConfirmed ActionStatus = "CONFIRMED" // 已确认,登记日快照已冻结
	ActionExecuting ActionStatus = "EXECUTING" // 执行中(可能因中断停留)
	ActionExecuted  ActionStatus = "EXECUTED"  // 执行完成
	ActionCancelled ActionStatus = "CANCELLED" // 执行前取消
)

// ConversionStatus 转换明细状态。
type ConversionStatus string

const (
	ConversionPending ConversionStatus = "PENDING"
	ConversionDone    ConversionStatus = "DONE"
)

// Action 公司行动定义。Version 为行动定义版本,登记时置为 1,
// 不可变更(比例/日期/基金不同的重复提交直接冲突),
// 每条转换明细都记录生成时所采用的版本。
type Action struct {
	ActionID      string
	FundID        string
	ShareClass    string // 适用份额类型
	RecordDate    Date   // 登记日
	EffectiveDate Date   // 生效日(除权日)
	Ratio         Ratio
	Version       int
	Status        ActionStatus
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SnapshotRow 登记日持有人份额快照,行动确认时冻结。
type SnapshotRow struct {
	ActionID     string
	FundID       string
	ShareClass   string
	HolderID     string
	TotalShares  Shares // 登记日日终持有份额(含冻结份额)
	FrozenShares Shares // 其中冻结份额
	FrozenAt     time.Time
}

// Conversion 单个持有人的转换明细,同一行动内唯一,
// 执行后不得修改或删除,可追溯原持有人、原数量与行动版本。
type Conversion struct {
	ActionID      string
	FundID        string
	ShareClass    string
	HolderID      string
	ActionVersion int // 采用的行动版本

	BeforeShares Shares // 转换前份额(快照)
	BeforeFrozen Shares // 转换前冻结份额(快照)

	AfterShares      Shares      // 转换后份额(向下取整到 0.01 份)
	AfterFrozen      Shares      // 转换后继续冻结的份额
	FractionalShares MicroShares // 被舍去的零碎份额(1e-6 份,差额来源)
	CashInLieu       Money       // 零碎份额按面值 1.00 元/份折算的现金(分)

	Status     ConversionStatus
	FinishedAt time.Time
}

// ExecutionResult 行动执行结果,重复执行时原样返回。
type ExecutionResult struct {
	ActionID        string
	Status          ActionStatus
	Direction       Direction
	HolderCount     int
	TotalBefore     Shares      // 转换前份额合计
	TotalAfter      Shares      // 转换后份额合计
	TotalFraction   MicroShares // 零碎份额合计
	TotalCashInLieu Money       // 零碎现金替代合计
	FinishedAt      time.Time
}

// StatusChange 行动状态变化记录,每次迁移持久化一条。
type StatusChange struct {
	ActionID string
	From     ActionStatus
	To       ActionStatus
	Reason   string
	At       time.Time
}

// PostingKind 账户流水类型。
type PostingKind string

const (
	PostingTrade            PostingKind = "TRADE"             // 申购(正)/赎回(负)
	PostingFreeze           PostingKind = "FREEZE"            // 份额冻结(正)/解冻(负),仅改冻结额
	PostingConversionRetire PostingKind = "CONVERSION_RETIRE" // 转换:注销旧份额
	PostingConversionIssue  PostingKind = "CONVERSION_ISSUE"  // 转换:登记新份额与零碎现金
)

// Posting 账户流水,带幂等键,重复应用不产生重复入账。
type Posting struct {
	Key         string // 幂等键
	FundID      string
	ShareClass  string
	HolderID    string
	Date        Date
	ShareDelta  Shares // 总份额变动
	FrozenDelta Shares // 冻结份额变动(不改变总份额)
	CashDelta   Money
	Kind        PostingKind
	Ref         string // 关联行动号
}

// convertShares 按比例转换份额(单位 0.01 份):
// 精确结果以 1e-6 份计算,新份额向下取整到 0.01 份,
// 返回取整后份额与被舍去的零碎份额。
func convertShares(before Shares, r Ratio) (after Shares, fractional MicroShares) {
	exactMicro := int64(before) * 10000 * r.Num / r.Den
	return Shares(exactMicro / 10000), MicroShares(exactMicro % 10000)
}

// convertFrozen 冻结份额按同一比例向下取整转换,
// 保证转换后冻结额不超过转换后总份额。
func convertFrozen(before Shares, r Ratio) Shares {
	return Shares(int64(before) * r.Num / r.Den)
}

// cashInLieuFor 零碎份额按面值 1.00 元/份折算现金,四舍五入到分:
// 1e-6 份 = 1e-6 元 = 1/10000 分。
func cashInLieuFor(fractional MicroShares) Money {
	return Money((int64(fractional) + 5000) / 10000)
}
