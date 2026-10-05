package fundoperations

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// decimalPattern 只接受规范的十进制小数字符串：整数部分至少一位，
// 小数部分可为空或若干位。不接受指数形式与分子/分母分数形式。
var decimalPattern = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)

// Decimal 是基于 big.Rat 的精确十进制数值，避免浮点误差。
// 零值即数字 0。业务中的金额、净值、份额均使用该类型。
type Decimal struct {
	rat *big.Rat
}

// ParseDecimal 解析十进制小数字符串，例如 "1000.00"。
func ParseDecimal(s string) (Decimal, error) {
	if !decimalPattern.MatchString(s) {
		return Decimal{}, fmt.Errorf("invalid decimal: %q", s)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "-")
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	num, ok := new(big.Int).SetString(intPart, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal: %q", s)
	}
	scale := 0
	if len(parts) == 2 {
		frac := parts[1]
		scale = len(frac)
		if scale > 0 {
			f, ok := new(big.Int).SetString(frac, 10)
			if !ok {
				return Decimal{}, fmt.Errorf("invalid decimal: %q", s)
			}
			num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
			num.Add(num, f)
		}
	}
	if neg {
		num.Neg(num)
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	return Decimal{rat: new(big.Rat).SetFrac(num, den)}, nil
}

// MustParseDecimal 解析失败时 panic，适用于测试与字面量。
func MustParseDecimal(s string) Decimal {
	d, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Decimal) norm() *big.Rat {
	if d.rat == nil {
		return new(big.Rat)
	}
	return d.rat
}

// Zero reports whether the value equals zero.
func (d Decimal) Zero() bool { return d.norm().Sign() == 0 }

// Positive reports whether the value is greater than zero.
func (d Decimal) Positive() bool { return d.norm().Sign() > 0 }

// Cmp returns -1, 0, 1 as d <, ==, > other.
func (d Decimal) Cmp(other Decimal) int { return d.norm().Cmp(other.norm()) }

// Add returns d + other.
func (d Decimal) Add(other Decimal) Decimal {
	return Decimal{rat: new(big.Rat).Add(d.norm(), other.norm())}
}

// Sub returns d - other.
func (d Decimal) Sub(other Decimal) Decimal {
	return Decimal{rat: new(big.Rat).Sub(d.norm(), other.norm())}
}

// Mul returns d * other.
func (d Decimal) Mul(other Decimal) Decimal {
	return Decimal{rat: new(big.Rat).Mul(d.norm(), other.norm())}
}

// Quo returns d / other；other 必须非零。
func (d Decimal) Quo(other Decimal) Decimal {
	if other.norm().Sign() == 0 {
		panic("division by zero")
	}
	return Decimal{rat: new(big.Rat).Quo(d.norm(), other.norm())}
}

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{rat: new(big.Rat).Neg(d.norm())} }

// Scale 返回小数位数（去掉末尾的 0 之后的实际位数）。
func (d Decimal) Scale() int {
	r := new(big.Rat).Set(d.norm())
	den := r.Denom()
	ten := big.NewInt(10)
	scale := 0
	num := new(big.Int).Set(r.Num())
	mod := new(big.Int)
	for scale < 100 {
		mod.Mod(num, ten)
		if mod.Sign() != 0 {
			break
		}
		if new(big.Int).Mod(den, ten).Sign() != 0 {
			break
		}
		num.Quo(num, ten)
		den.Quo(den, ten)
		r.SetFrac(num, den)
		scale--
	}
	// 直接从分母统计 2、5 因子更可靠。
	d2 := factorCount(den, big.NewInt(2))
	d5 := factorCount(den, big.NewInt(5))
	if d2 > d5 {
		return d2
	}
	return d5
}

func factorCount(n, f *big.Int) int {
	count := 0
	x := new(big.Int).Set(n)
	zero := big.NewInt(0)
	mod := new(big.Int)
	for x.Cmp(big.NewInt(1)) > 0 {
		mod.Mod(x, f)
		if mod.Cmp(zero) != 0 {
			break
		}
		x.Quo(x, f)
		count++
	}
	return count
}

// RoundHalfUp 按四舍五入（对正数为 half up；负数按绝对值 half up 后恢复符号）
// 保留 scale 位小数。
func (d Decimal) RoundHalfUp(scale int) Decimal {
	r := d.norm()
	neg := r.Sign() < 0
	abs := new(big.Rat).Set(r)
	if neg {
		abs.Neg(abs)
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(abs, new(big.Rat).SetInt(factor))
	q, m := new(big.Int), new(big.Int)
	q.QuoRem(scaled.Num(), scaled.Denom(), m)
	twice := new(big.Int).Mul(m, big.NewInt(2))
	if twice.Cmp(scaled.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	out := new(big.Rat).SetFrac(q, new(big.Int).Set(factor))
	if neg {
		out.Neg(out)
	}
	return Decimal{rat: out}
}

// String 返回精确的十进制字符串；若分母含 2、5 以外的因子（无限小数），
// 退化为 "a/b" 分数形式以保证不丢精度。
func (d Decimal) String() string {
	r := d.norm()
	if r.Sign() == 0 {
		return "0"
	}
	den := r.Denom()
	if factorCount(den, big.NewInt(2))+factorCount(den, big.NewInt(5)) == 0 {
		return r.FloatString(0)
	}
	// 分母仅由 2、5 构成时必定为有限小数。
	s := r.FloatString(maxScale(r))
	return trimFraction(s)
}

func maxScale(r *big.Rat) int {
	d := r.Denom()
	d2 := factorCount(d, big.NewInt(2))
	d5 := factorCount(d, big.NewInt(5))
	if d2 > d5 {
		return d2
	}
	return d5
}

func trimFraction(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// MarshalJSON 序列化为精确字符串。
func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON 同时接受 JSON 字符串与 JSON 数字（按十进制文本解析）。
func (d *Decimal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// 可能是数字字面量。
		text := strings.TrimSpace(string(data))
		if !decimalPattern.MatchString(text) {
			return errors.New("decimal must be a JSON string or number")
		}
		s = text
	}
	parsed, err := parseDecimalOrFraction(s)
	if err != nil {
		return err
	}
	d.rat = parsed.rat
	return nil
}

// parseDecimalOrFraction 内部使用：既接受十进制字符串，也接受 big.Rat 的
// "a/b" 分数形式，用于持久化毛份额等精确分数。
func parseDecimalOrFraction(s string) (Decimal, error) {
	if decimalPattern.MatchString(s) {
		return ParseDecimal(s)
	}
	if r, ok := new(big.Rat).SetString(s); ok {
		return Decimal{rat: r}, nil
	}
	return Decimal{}, fmt.Errorf("invalid decimal: %q", s)
}
