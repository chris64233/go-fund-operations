package fundoperations

import (
	"errors"
	"time"
)

// 业务错误。调用方应使用 errors.Is 判断。
var (
	// ErrNAVNotFound 表示净值记录不存在。
	ErrNAVNotFound = errors.New("nav record not found")
	// ErrVersionConflict 表示同一基金同一估值日已存在内容不同的当前版本。
	ErrVersionConflict = errors.New("nav version conflict")
	// ErrStaleNAVVersion 表示确认引用了一个已被更正取代的旧版本。
	ErrStaleNAVVersion = errors.New("stale nav version")
	// ErrNAVNotPublished 表示净值尚未发布，不能被交易确认引用。
	ErrNAVNotPublished = errors.New("nav record not published")
	// ErrNAVReferenced 表示净值已被交易确认引用，不可撤回或修改。
	ErrNAVReferenced = errors.New("nav record referenced by confirmations")
	// ErrInvalidTransition 表示状态机不允许的操作。
	ErrInvalidTransition = errors.New("invalid nav status transition")
	// ErrNoChange 表示更正内容与原版本完全一致。
	ErrNoChange = errors.New("correction has no change")
	// ErrConfirmationNotFound 表示交易确认不存在。
	ErrConfirmationNotFound = errors.New("confirmation not found")
	// ErrAdjustmentNotFound 表示差额处理记录不存在。
	ErrAdjustmentNotFound = errors.New("adjustment not found")
)

// NAVStatus 是净值记录的生命周期状态。
type NAVStatus string

const (
	// NAVStatusDraft 草稿，可撤回，不可被交易确认引用。
	NAVStatusDraft NAVStatus = "DRAFT"
	// NAVStatusPublished 当前有效版本，可被交易确认引用。
	NAVStatusPublished NAVStatus = "PUBLISHED"
	// NAVStatusSuperseded 已被更正版本取代，原记录与引用关系冻结。
	NAVStatusSuperseded NAVStatus = "SUPERSEDED"
	// NAVStatusWithdrawn 已撤回的草稿。
	NAVStatusWithdrawn NAVStatus = "WITHDRAWN"
)

// NAVRecord 是某一基金某一估值日的一个单位净值版本。
// 记录一旦创建即不可变，更正通过新增版本完成。
type NAVRecord struct {
	ID            string
	FundID        string
	ValuationDate time.Time
	NAV           NAV
	BasisVersion  string // 计算依据版本
	Version       int    // 同一基金同一估值日内从 1 递增
	Status        NAVStatus
	CorrectionOf  string // 更正来源版本 ID，非更正版本为空
	Reason        string // 更正原因
	CorrectedBy   string // 更正人
	PublishedAt   time.Time
	CreatedAt     time.Time
}

// TradeType 交易类型。
type TradeType string

const (
	TradeSubscribe TradeType = "SUBSCRIBE"
	TradeRedeem    TradeType = "REDEEM"
)

// TradeConfirmation 是一笔引用特定净值版本的交易确认。
// 确认金额在创建时按当时引用的净值版本计算并冻结，之后不再变化。
type TradeConfirmation struct {
	ID            string
	FundID        string
	ValuationDate time.Time
	NAVVersionID  string
	Type          TradeType
	Shares        Shares
	Amount        Amount
	CreatedAt     time.Time
}

// Correction 是一次净值更正登记，关联原版本与新版本。
type Correction struct {
	ID            string
	FundID        string
	ValuationDate time.Time
	FromVersionID string
	ToVersionID   string
	Reason        string
	CorrectedBy   string
	CreatedAt     time.Time
}

// AdjustmentStatus 差额处理状态。
type AdjustmentStatus string

const (
	AdjustmentPending   AdjustmentStatus = "PENDING"
	AdjustmentProcessed AdjustmentStatus = "PROCESSED"
)

// Adjustment 是更正发布后针对单笔受影响确认的差额处理记录。
// Delta 为正表示应补收，为负表示应退回。原确认结果保持不变。
type Adjustment struct {
	ID             string
	CorrectionID   string
	ConfirmationID string
	FromVersionID  string
	ToVersionID    string
	OldAmount      Amount
	NewAmount      Amount
	Delta          Amount
	Status         AdjustmentStatus
	CreatedAt      time.Time
}

// CorrectionResult 是 CorrectNAV 的返回结果。
type CorrectionResult struct {
	Correction  *Correction
	NewVersion  *NAVRecord
	Adjustments []*Adjustment
}
