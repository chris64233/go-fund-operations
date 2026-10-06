package fundoperations

import (
	"errors"
	"time"
)

// NAVVersion 是同一基金同一估值日下的一个净值版本。版本只增不改：
// 草稿可发布、未被引用时可撤回；发布后被交易确认引用则永久冻结，
// 修正只能生成新版本并通过 BasedOn 链回原版本。
type NAVVersion struct {
	FundID      string
	ValueDate   string // 估值日，格式 YYYY-MM-DD
	Version     int    // 同一 (基金, 估值日) 内从 1 递增
	UnitNAV     Decimal
	Basis       string // 计算依据版本（估值数据/计算结果的版本标识）
	Status      NAVStatus
	CreatedAt   time.Time
	PublishedAt time.Time
	WithdrawnAt time.Time
	// BasedOn 指向被更正的原版本号；首发版本为 0。
	BasedOn int
	// Reason / CorrectedBy 仅更正版本有值。
	Reason      string
	CorrectedBy string
}

// NAVStatus 版本生命周期状态。
type NAVStatus string

const (
	StatusDraft      NAVStatus = "draft"      // 草稿：可发布，未被引用时可撤回
	StatusPublished  NAVStatus = "published"  // 已发布：是当前有效版本或曾被引用
	StatusWithdrawn  NAVStatus = "withdrawn"  // 撤回：从未发布使用的草稿被删除
	StatusSuperseded NAVStatus = "superseded" // 已被更正版本取代，但仍可被追溯
)

// TxType 交易类型。
type TxType string

const (
	TxSubscription TxType = "subscription" // 申购
	TxRedemption   TxType = "redemption"   // 赎回
)

// Confirmation 是交易确认。确认一经生成即不可变，永久记录确认时采用的
// 净值版本与单位净值；净值更正不修改确认，只追加差额处理。
type Confirmation struct {
	ConfirmID    string
	FundID       string
	ValueDate    string
	TxType       TxType
	Shares       Decimal
	NAVVersion   int     // 确认时引用的净值版本号
	NAVAtConfirm Decimal // 确认时的单位净值快照
	Amount       Decimal // 确认金额 = 份额 × 净值，按金额精度舍入
	ConfirmedAt  time.Time
}

// AdjustmentType 差额方向。
type AdjustmentType string

const (
	AdjustCollect AdjustmentType = "collect" // 应补收：新净值高于原净值
	AdjustRefund  AdjustmentType = "refund"  // 应退回：新净值低于原净值
)

// AdjustmentStatus 差额处理状态。
type AdjustmentStatus string

const (
	AdjustPending AdjustmentStatus = "pending" // 已登记，待处理
	AdjustSettled AdjustmentStatus = "settled" // 已补收/退回完成
)

// Adjustment 是逐笔确认的差额处理。每条记录都关联一笔原确认与触发更正，
// 不存在没有来源确认的孤立差额。
type Adjustment struct {
	ID             string
	FundID         string
	ValueDate      string
	ConfirmID      string
	FromNAVVersion int // 原确认引用的版本
	ToNAVVersion   int // 触发更正的新版本
	TxType         TxType
	Shares         Decimal // 原确认份额快照
	OldNAV         Decimal
	NewNAV         Decimal
	DeltaNAV       Decimal // 新净值 - 原净值
	Amount         Decimal // 差额金额，带符号，正补负退
	Type           AdjustmentType
	Status         AdjustmentStatus
	CreatedAt      time.Time
	SettledAt      time.Time
	SettlementRef  string
}

// 领域错误，调用方可用 errors.Is 判别。
var (
	ErrNotFound          = errors.New("fundoperations: not found")
	ErrVersionConflict   = errors.New("fundoperations: nav version conflict")
	ErrImmutableVersion  = errors.New("fundoperations: nav version is immutable")
	ErrInvalidArgument   = errors.New("fundoperations: invalid argument")
	ErrStaleNAVReference = errors.New("fundoperations: confirmation references a stale nav version")
	ErrAlreadySettled    = errors.New("fundoperations: adjustment already settled")
)

// ConflictError 携带冲突细节，便于调用方向客户端返回明确信息。
type ConflictError struct {
	Err         error
	FundID      string
	ValueDate   string
	Current     int // 服务端当前有效版本号
	SeenVersion int // 调用方基于的版本号（若有）
}

func (e *ConflictError) Error() string {
	msg := e.Err.Error()
	if e.FundID != "" {
		msg += " fund=" + e.FundID + " date=" + e.ValueDate
	}
	if e.Current != 0 {
		msg += " current=v" + itoa(e.Current)
	}
	if e.SeenVersion != 0 {
		msg += " seen=v" + itoa(e.SeenVersion)
	}
	return msg
}

func (e *ConflictError) Unwrap() error { return e.Err }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var b [20]byte
	i := len(b)
	for n != 0 {
		i--
		b[i] = byte('0' + abs(n%10))
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
