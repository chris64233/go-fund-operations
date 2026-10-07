package swing

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal 使用整数尾数 + 十进制标度实现精确十进制表示，
// 避免浮点误差，满足净值与资金流计算"精确表示"的要求。
type Decimal struct {
	mant  *big.Int // 尾数
	scale int      // 小数位数，值 = mant / 10^scale
}

// NewDecimalFromString 解析十进制字符串（如 "1.2345"、"-0.02"）。
func NewDecimalFromString(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("empty decimal string")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return Decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if intPart == "" {
		intPart = "0"
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal %q", s)
		}
	}
	mant, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	if neg {
		mant.Neg(mant)
	}
	return Decimal{mant: mant, scale: len(fracPart)}, nil
}

// MustDecimal 解析失败时 panic，便于测试与常量定义。
func MustDecimal(s string) Decimal {
	d, err := NewDecimalFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// DecimalFromInt 由整数构造。
func DecimalFromInt(v int64) Decimal {
	return Decimal{mant: big.NewInt(v), scale: 0}
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// align 将两个 Decimal 对齐到相同标度，返回对齐后的尾数。
func align(a, b Decimal) (*big.Int, *big.Int, int) {
	scale := a.scale
	if b.scale > scale {
		scale = b.scale
	}
	am := new(big.Int).Mul(a.mant, pow10(scale-a.scale))
	bm := new(big.Int).Mul(b.mant, pow10(scale-b.scale))
	return am, bm, scale
}

// Add 返回 a+b。
func (a Decimal) Add(b Decimal) Decimal {
	am, bm, scale := align(a, b)
	return Decimal{mant: new(big.Int).Add(am, bm), scale: scale}
}

// Sub 返回 a-b。
func (a Decimal) Sub(b Decimal) Decimal {
	am, bm, scale := align(a, b)
	return Decimal{mant: new(big.Int).Sub(am, bm), scale: scale}
}

// Mul 返回 a*b，结果标度为两者标度之和，保持精确。
func (a Decimal) Mul(b Decimal) Decimal {
	return Decimal{
		mant:  new(big.Int).Mul(a.mant, b.mant),
		scale: a.scale + b.scale,
	}
}

// Neg 返回 -a。
func (a Decimal) Neg() Decimal {
	return Decimal{mant: new(big.Int).Neg(a.mant), scale: a.scale}
}

// Abs 返回 |a|。
func (a Decimal) Abs() Decimal {
	if a.Sign() < 0 {
		return a.Neg()
	}
	return a
}

// Sign 返回 -1、0 或 1。
func (a Decimal) Sign() int {
	return a.mant.Sign()
}

// Cmp 比较大小：a<b 返回 -1，a==b 返回 0，a>b 返回 1。
func (a Decimal) Cmp(b Decimal) int {
	am, bm, _ := align(a, b)
	return am.Cmp(bm)
}

// IsZero 报告是否为零。
func (a Decimal) IsZero() bool {
	return a.mant.Sign() == 0
}

// String 输出精确十进制字符串，不产生精度损失。
func (a Decimal) String() string {
	mant := a.mant.String()
	neg := strings.HasPrefix(mant, "-")
	if neg {
		mant = mant[1:]
	}
	if a.scale == 0 {
		if neg {
			return "-" + mant
		}
		return mant
	}
	if len(mant) <= a.scale {
		mant = strings.Repeat("0", a.scale-len(mant)+1) + mant
	}
	intPart := mant[:len(mant)-a.scale]
	fracPart := mant[len(mant)-a.scale:]
	if neg {
		return "-" + intPart + "." + fracPart
	}
	return intPart + "." + fracPart
}
