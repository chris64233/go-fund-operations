package capitalcall

import "errors"

var (
	ErrCommitmentNotFound = errors.New("capitalcall: commitment not found")
	ErrCommitmentInactive = errors.New("capitalcall: commitment inactive")
	ErrNoticeNotFound     = errors.New("capitalcall: notice not found")
	ErrNoticeNotOpen      = errors.New("capitalcall: notice not open")
	ErrAllocationNotFound = errors.New("capitalcall: allocation not found")
	ErrInsufficient       = errors.New("capitalcall: insufficient remaining commitment")
	ErrCurrencyMismatch   = errors.New("capitalcall: currency mismatch")
	ErrInvalidAmount      = errors.New("capitalcall: invalid amount")
	ErrOverpay            = errors.New("capitalcall: payment exceeds allocated amount")
	ErrDuplicatePayment   = errors.New("capitalcall: duplicate bank reference")
)
