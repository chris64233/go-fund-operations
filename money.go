package fundoperations

// 本文件定义统一的定点数精度规则：
//   - NAV    单位净值，万分之一为最小单位（4 位小数）。
//   - Shares 份额，百分之一份为最小单位（2 位小数）。
//   - Amount 金额，以货币最小单位（分）表示。
//
// 所有确认金额与更正差额都必须通过 AmountFor 计算，
// 以保证全系统使用同一套舍入规则。

// NAV 单位净值，定点数，4 位小数（1.0000 表示为 10000）。
type NAV int64

// Shares 份额，定点数，2 位小数（1 份表示为 100）。
type Shares int64

// Amount 金额，单位为分。
type Amount int64

// navScale 是 NAV 的小数位数对应的放大倍数。
const navScale = 10_000

// sharesScale 是 Shares 的小数位数对应的放大倍数。
const sharesScale = 100

// AmountFor 按统一精度把份额折算为金额：
// 先以最高精度相乘，再对结果四舍五入（half-up）到分。
// 份额与净值均非负，因此只需处理非负舍入。
func AmountFor(shares Shares, nav NAV) Amount {
	product := int64(shares) * int64(nav) // 10^-2 * 10^-4 = 10^-6
	return Amount((product + navScale/2) / navScale)
}
