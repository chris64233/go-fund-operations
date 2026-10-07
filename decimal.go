package fundoperations

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal 基于 big.Rat 的精确十进制数，避免浮点误差。
// 零值不可用，请通过构造函数创建。
type Decimal struct {
	r *big.Rat
}

// NewDecimalFromString 解析十进制字符串（支持负数与小数）。
func NewDecimalFromString(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("decimal: empty string")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Decimal{}, fmt.Errorf("decimal: invalid value %q", s)
	}
	return Decimal{r: r}, nil
}

// DecimalFromInt 由整数构造。
func DecimalFromInt(i int64) Decimal {
	return Decimal{r: new(big.Rat).SetInt64(i)}
}

// MustDecimal 解析失败即 panic，仅用于测试与常量。
func MustDecimal(s string) Decimal {
	d, err := NewDecimalFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// rat 返回内部有理数，零值按 0 处理。
func (d Decimal) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

func (d Decimal) valid() bool { return d.r != nil }

// Add 返回 d+o。
func (d Decimal) Add(o Decimal) Decimal {
	return Decimal{r: new(big.Rat).Add(d.rat(), o.rat())}
}

// Sub 返回 d-o。
func (d Decimal) Sub(o Decimal) Decimal {
	return Decimal{r: new(big.Rat).Sub(d.rat(), o.rat())}
}

// Mul 返回 d*o。
func (d Decimal) Mul(o Decimal) Decimal {
	return Decimal{r: new(big.Rat).Mul(d.rat(), o.rat())}
}

// Quo 返回 d/o，o 为零时 panic。
func (d Decimal) Quo(o Decimal) Decimal {
	if o.IsZero() {
		panic("decimal: division by zero")
	}
	return Decimal{r: new(big.Rat).Quo(d.rat(), o.rat())}
}

// Neg 返回 -d。
func (d Decimal) Neg() Decimal {
	return Decimal{r: new(big.Rat).Neg(d.rat())}
}

// Abs 返回 |d|。
func (d Decimal) Abs() Decimal {
	return Decimal{r: new(big.Rat).Abs(d.rat())}
}

// Cmp 比较大小，返回 -1/0/1。
func (d Decimal) Cmp(o Decimal) int { return d.rat().Cmp(o.rat()) }

// Sign 返回符号，-1/0/1。
func (d Decimal) Sign() int { return d.rat().Sign() }

// IsZero 是否为零。
func (d Decimal) IsZero() bool { return d.rat().Sign() == 0 }

// String 输出精确十进制表示，去掉多余的尾零。
func (d Decimal) String() string {
	if !d.valid() {
		return "0"
	}
	if d.r.IsInt() {
		return d.r.Num().String()
	}
	// 寻找能精确表示的最少小数位（分母仅含因子 2 和 5）。
	den := new(big.Int).Set(d.r.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	rem := new(big.Int)
	places := 0
	for {
		if den.Cmp(big.NewInt(1)) == 0 {
			s := d.r.FloatString(places)
			return strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		var q *big.Int
		q, rem = new(big.Int).QuoRem(den, two, new(big.Int))
		if rem.Sign() == 0 {
			den = q
			places++
			continue
		}
		q, rem = new(big.Int).QuoRem(den, five, new(big.Int))
		if rem.Sign() == 0 {
			den = q
			places++
			continue
		}
		// 无法有限表示时保留 12 位小数。
		s := d.r.FloatString(12)
		return strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
}
