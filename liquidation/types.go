// Package liquidation implements fund termination liquidation:
// plans, frozen share snapshots, initial and supplementary
// distributions, reserve release, resumable payment execution
// and concurrency-safe state transitions.
package liquidation

import (
	"errors"
	"fmt"
	"time"
)

// Money is expressed in the smallest currency unit (cents).
type Money int64

// Shares is expressed in the smallest fund share unit.
type Shares int64

// PlanStatus is the lifecycle status of a liquidation plan.
type PlanStatus string

const (
	PlanDraft     PlanStatus = "DRAFT"
	PlanConfirmed PlanStatus = "CONFIRMED"
	PlanCompleted PlanStatus = "COMPLETED"
	PlanCancelled PlanStatus = "CANCELLED"
)

// ItemStatus is the status of a single distribution line item.
type ItemStatus string

const (
	ItemPending ItemStatus = "PENDING"
	ItemPaid    ItemStatus = "PAID"
	ItemOnHold  ItemStatus = "ON_HOLD" // investor exited or account abnormal
)

// BatchStatus is the status of a distribution batch.
type BatchStatus string

const (
	BatchOpen      BatchStatus = "OPEN"
	BatchDone      BatchStatus = "DONE"
	BatchPartial   BatchStatus = "PARTIAL" // has on-hold items
	BatchCancelled BatchStatus = "CANCELLED"
)

// BatchSource identifies initial vs supplementary (reserve release) batches.
type BatchSource string

const (
	SourceInitial        BatchSource = "INITIAL"
	SourceReserveRelease BatchSource = "RESERVE_RELEASE"
)

// SnapshotEntry is one investor's frozen shares at the record date.
type SnapshotEntry struct {
	InvestorID string `json:"investor_id"`
	Shares     Shares `json:"shares"`
}

// Item is one investor's distribution line inside a batch.
type Item struct {
	InvestorID string     `json:"investor_id"`
	Shares     Shares     `json:"shares"`
	Amount     Money      `json:"amount"`
	Status     ItemStatus `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	PaidAt     *time.Time `json:"paid_at,omitempty"`
}

// Batch is one distribution run (initial or supplementary).
type Batch struct {
	ID        string      `json:"id"`
	Seq       int         `json:"seq"`
	Source    BatchSource `json:"source"`
	Total     Money       `json:"total"` // distributable total of this batch
	Dust      Money       `json:"dust"`  // rounding remainder kept in liquidation assets
	Items     []*Item     `json:"items"`
	Status    BatchStatus `json:"status"`
	CreatedAt time.Time   `json:"created_at"`
}

// Plan fixes fund, record date, distributable cash, fee reserve,
// dispute reserve and the share snapshot.
type Plan struct {
	ID             string          `json:"id"`
	FundID         string          `json:"fund_id"`
	RecordDate     time.Time       `json:"record_date"`
	TotalAssets    Money           `json:"total_assets"`
	FeeReserve     Money           `json:"fee_reserve"`
	DisputeReserve Money           `json:"dispute_reserve"`
	FeeRemaining   Money           `json:"fee_remaining"`
	DisputeRemain  Money           `json:"dispute_remaining"`
	Snapshot       []SnapshotEntry `json:"snapshot"`
	TotalShares    Shares          `json:"total_shares"`
	Status         PlanStatus      `json:"status"`
	Batches        []*Batch        `json:"batches"`
	CreatedAt      time.Time       `json:"created_at"`
	ConfirmedAt    *time.Time      `json:"confirmed_at,omitempty"`
}

var (
	ErrPlanNotFound     = errors.New("liquidation: plan not found")
	ErrPlanNotDraft     = errors.New("liquidation: plan is not in draft status")
	ErrPlanNotConfirmed = errors.New("liquidation: plan is not confirmed")
	ErrPlanCancelled    = errors.New("liquidation: plan is cancelled")
	ErrPlanCompleted    = errors.New("liquidation: plan is completed")
	ErrBatchNotFound    = errors.New("liquidation: batch not found")
	ErrInvalidAmount    = errors.New("liquidation: invalid amount")
	ErrEmptySnapshot    = errors.New("liquidation: snapshot is empty")
	ErrReserveExceed    = errors.New("liquidation: release exceeds remaining reserve")
	ErrTradingHalted    = errors.New("liquidation: trading halted after plan confirmation")
	ErrRecordDatePassed = errors.New("liquidation: holding change after record date cannot enter this liquidation")
)

// Distributable is the amount currently available for distribution.
func (p *Plan) Distributable() Money {
	return p.TotalAssets - p.FeeRemaining - p.DisputeRemain
}

// Reconcile checks the invariant: all distributions plus reserves
// plus rounding dust equal the liquidation assets.
func (p *Plan) Reconcile() error {
	sum := p.FeeRemaining + p.DisputeRemain
	for _, b := range p.Batches {
		var items Money
		for _, it := range b.Items {
			items += it.Amount
		}
		if items+b.Dust != b.Total {
			return fmt.Errorf("liquidation: batch %s out of balance: items %d + dust %d != total %d",
				b.ID, items, b.Dust, b.Total)
		}
		sum += b.Total
	}
	if sum != p.TotalAssets {
		return fmt.Errorf("liquidation: plan out of balance: %d != total assets %d", sum, p.TotalAssets)
	}
	return nil
}

// Progress reports payment progress of one batch.
type Progress struct {
	BatchID      string      `json:"batch_id"`
	Total        Money       `json:"total"`
	PaidAmount   Money       `json:"paid_amount"`
	PaidCount    int         `json:"paid_count"`
	PendingCount int         `json:"pending_count"`
	OnHoldCount  int         `json:"on_hold_count"`
	Status       BatchStatus `json:"status"`
}

// RemainingAssets reports the composition of remaining assets.
type RemainingAssets struct {
	FeeReserve     Money `json:"fee_reserve"`
	DisputeReserve Money `json:"dispute_reserve"`
	UnpaidItems    Money `json:"unpaid_items"`
	Dust           Money `json:"dust"`
	Total          Money `json:"total"`
}
