package fundoperations

import "errors"

var (
	ErrInvalidRequest      = errors.New("fundoperations: 非法请求参数")
	ErrFundNotFound        = errors.New("fundoperations: 基金不存在")
	ErrFundSuspended       = errors.New("fundoperations: 基金已暂停交易")
	ErrCurrencyMismatch    = errors.New("fundoperations: 申购币种与基金币种不符")
	ErrBelowMinimum        = errors.New("fundoperations: 申购金额低于起点")
	ErrConflict            = errors.New("fundoperations: 相同外部申请号但内容不一致")
	ErrBatchClosed         = errors.New("fundoperations: 批次已关闭，申请迟到")
	ErrBatchNotFound       = errors.New("fundoperations: 批次不存在")
	ErrBatchNotClosed      = errors.New("fundoperations: 批次尚未关闭，不能确认份额")
	ErrNAVNotFound         = errors.New("fundoperations: 估值日缺少已发布的净值")
	ErrNoTradingDay        = errors.New("fundoperations: 交易日历中没有可用的交易日")
	ErrApplicationNotFound = errors.New("fundoperations: 申请不存在")
)
