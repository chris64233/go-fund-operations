package fundoperations

import (
	"fmt"
	"math/big"
	"strings"
)

// 全领域统一精度：单位净值 4 位小数，份额/金额 2 位小数。
const (
	navScale    uint = 4
	amountScale uint = 2
	shareScale  uint = 2
)

// Decimal 是不可变的定点十进制数，内部以 big.Int 非标度值 + 标度表示，
// 避免 float64 在金额计算中的舍入误差。算术运算均返回新值。
type Decimal struct {
	unscaled *big.Int
	scale    uint
}

// ParseDecimal 按指定标度解析十进制字符串。小数位超过 scale 会被拒绝，
// 以免在输入边界静默丢失精度；不足的小数位右侧补零。
func ParseDecimal(s string, scale uint) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("empty decimal")
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return Decimal{}, fmt.Errorf("invalid decimal")
	}
	intPart, fracPart, dot := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal %q", s)
		}
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal %q", s)
		}
	}
	if dot && len(fracPart) > int(scale) {
		return Decimal{}, fmt.Errorf("decimal %q exceeds scale %d", s, scale)
	}
	if len(intPart) > 1 {
		intPart = strings.TrimLeft(intPart, "0")
		if intPart == "" {
			intPart = "0"
		}
	}
	digits := intPart + fracPart + strings.Repeat("0", int(scale)-len(fracPart))
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	if neg {
		v.Neg(v)
	}
	return Decimal{unscaled: v, scale: scale}, nil
}

func mustParseDecimal(s string, scale uint) Decimal {
	d, err := ParseDecimal(s, scale)
	if err != nil {
		panic(err)
	}
	return d
}

// String 按定标输出，例如标度 4 的 1.2 输出 "1.2000"。
func (d Decimal) String() string {
	if d.unscaled == nil {
		return ""
	}
	neg := d.unscaled.Sign() < 0
	abs := new(big.Int).Abs(d.unscaled)
	s := abs.String()
	if scale := int(d.scale); len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	intPart, fracPart := s[:len(s)-int(d.scale)], s[len(s)-int(d.scale):]
	out := intPart
	if d.scale > 0 {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out
}

func (d Decimal) toScale(scale uint) *big.Int {
	if d.unscaled == nil {
		return new(big.Int)
	}
	if d.scale == scale {
		return new(big.Int).Set(d.unscaled)
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absDiff(scale, d.scale))), nil)
	if d.scale < scale {
		return new(big.Int).Mul(d.unscaled, pow)
	}
	return new(big.Int).Quo(d.unscaled, pow)
}

func absDiff(a, b uint) uint {
	if a > b {
		return a - b
	}
	return b - a
}

func (d Decimal) align(o Decimal) (*big.Int, *big.Int, uint) {
	scale := d.scale
	if o.scale > scale {
		scale = o.scale
	}
	return d.toScale(scale), o.toScale(scale), scale
}

// Cmp 比较两个数值（允许标度不同），返回 -1/0/1。
func (d Decimal) Cmp(o Decimal) int {
	a, b, _ := d.align(o)
	return a.Cmp(b)
}

// Equal 按数值判等，与标度无关。
func (d Decimal) Equal(o Decimal) bool { return d.Cmp(o) == 0 }

func (d Decimal) Sign() int {
	if d.unscaled == nil {
		return 0
	}
	return d.unscaled.Sign()
}

// Add 返回 d+o，标度取两者较大值。
func (d Decimal) Add(o Decimal) Decimal {
	a, b, scale := d.align(o)
	return Decimal{unscaled: a.Add(a, b), scale: scale}
}

// Sub 返回 d-o，标度取两者较大值。
func (d Decimal) Sub(o Decimal) Decimal {
	a, b, scale := d.align(o)
	return Decimal{unscaled: a.Sub(a, b), scale: scale}
}

// Neg 返回 -d。
func (d Decimal) Neg() Decimal {
	return Decimal{unscaled: new(big.Int).Neg(d.unscaled), scale: d.scale}
}

// Mul 返回精确乘积，标度为两者标度之和。
func (d Decimal) Mul(o Decimal) Decimal {
	return Decimal{
		unscaled: new(big.Int).Mul(d.toScale(d.scale), o.toScale(o.scale)),
		scale:    d.scale + o.scale,
	}
}

// Round 按四舍五入（绝对值半数进位）归整到指定标度。
func (d Decimal) Round(scale uint) Decimal {
	if d.unscaled == nil || d.scale <= scale {
		v := new(big.Int)
		if d.unscaled != nil {
			v = d.toScale(scale)
		}
		return Decimal{unscaled: v, scale: scale}
	}
	diff := d.scale - scale
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(diff)), nil)
	q, r := new(big.Int).QuoRem(d.unscaled, div, new(big.Int))
	twice := new(big.Int).Abs(r)
	twice.Mul(twice, big.NewInt(2))
	if twice.Cmp(div) >= 0 {
		if d.unscaled.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return Decimal{unscaled: q, scale: scale}
}
