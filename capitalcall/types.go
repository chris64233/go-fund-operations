// Package capitalcall implements private fund capital commitments and
// capital call notices. A fund calls committed capital from investors in
// instalments as projects require; calls may not exceed commitments and
// concurrent notices may not reserve the same uncalled capital twice.
//
// All monetary values are integer minor units (e.g. cents) to keep
// rounding exact.
package capitalcall

import (
	"errors"
	"time"
)

// NoticeStatus is the lifecycle state of a capital call notice.
type NoticeStatus string

const (
	// NoticeActive means the notice reserves uncalled commitment and
	// still accepts payments.
	NoticeActive NoticeStatus = "ACTIVE"
	// NoticeCancelled means the notice was cancelled and its unpaid
	// reserved amounts were released back to uncalled commitment.
	NoticeCancelled NoticeStatus = "CANCELLED"
)

var (
	ErrCommitmentExists   = errors.New("capitalcall: commitment already exists")
	ErrCommitmentNotFound = errors.New("capitalcall: commitment not found")
	ErrNoticeExists       = errors.New("capitalcall: notice already exists")
	ErrNoticeNotFound     = errors.New("capitalcall: notice not found")
	ErrNoticeNotActive    = errors.New("capitalcall: notice is not active")
	ErrInsufficient       = errors.New("capitalcall: insufficient uncalled commitment")
	ErrInvalidAmount      = errors.New("capitalcall: amount must be positive")
	ErrOverpayment        = errors.New("capitalcall: payment exceeds allocated amount")
	ErrNoAllocation       = errors.New("capitalcall: investor has no allocation on notice")
)

// Commitment records an investor's capital commitment.
type Commitment struct {
	ID         string
	InvestorID string
	Currency   string
	// Total is the committed amount.
	Total int64
	// Called is the amount already called by active notices, both
	// notified-but-unpaid and paid.
	Called int64
	// Paid is the amount actually received and confirmed.
	Paid int64
	// Active reports whether the commitment can be called on.
	Active bool
}

// Uncalled returns the remaining amount available for new notices.
func (c *Commitment) Uncalled() int64 { return c.Total - c.Called }

// NotifiedUnpaid returns the amount called but not yet paid.
func (c *Commitment) NotifiedUnpaid() int64 { return c.Called - c.Paid }

// Allocation is one commitment's share of a notice. An investor with
// several commitments in the notice currency has one allocation per
// commitment; API queries aggregate them per investor.
type Allocation struct {
	CommitmentID string
	InvestorID   string
	// Amount is the amount due from the investor for this notice.
	Amount int64
	// PaidAmount is the cumulative confirmed payment against Amount.
	PaidAmount int64
}

// Outstanding returns the unpaid remainder of the allocation.
func (a *Allocation) Outstanding() int64 { return a.Amount - a.PaidAmount }

// Notice is a capital call notice sent to investors.
type Notice struct {
	ID           string
	Purpose      string
	Currency     string
	TargetAmount int64
	DueDate      time.Time
	Status       NoticeStatus
	Allocations  []Allocation
	CreatedAt    time.Time
}

// PaidTotal returns the cumulative confirmed payments on the notice.
func (n *Notice) PaidTotal() int64 {
	var sum int64
	for i := range n.Allocations {
		sum += n.Allocations[i].PaidAmount
	}
	return sum
}

// Payment is a confirmed payment against one allocation, backed by a
// bank reference. Bank references are unique: confirming the same
// reference twice never increases paid amounts.
type Payment struct {
	ID            string
	NoticeID      string
	InvestorID    string
	BankReference string
	Amount        int64
	ConfirmedAt   time.Time
}

// Position summarises an investor's commitment position.
type Position struct {
	InvestorID     string
	Currency       string
	Total          int64
	Uncalled       int64
	NotifiedUnpaid int64
	Paid           int64
}
