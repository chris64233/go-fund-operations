package fundoperations

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// ShareScale 是基金份额的最小单位精度：1 份 = 100 个最小单位（0.01 份）。
// 全系统以 int64 个最小单位保存份额数量，避免浮点误差。
const ShareScale int64 = 100

// Qty 是以 1/ShareScale 份为单位的份额数量。
type Qty int64

// ParseQty 解析十进制份额字符串（如 "100.05"），不允许超过 ShareScale 位小数。
func ParseQty(s string) (Qty, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("fundoperations: 空份额")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("fundoperations: 非法份额 %q", s)
	}
	parts := strings.SplitN(s, ".", 2)
	intPart, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || intPart < 0 {
		return 0, fmt.Errorf("fundoperations: 非法份额 %q", s)
	}
	frac := int64(0)
	if len(parts) == 2 {
		if parts[1] == "" || len(parts[1]) > 2 {
			return 0, fmt.Errorf("fundoperations: 份额小数最多 %d 位", len(strconv.FormatInt(ShareScale, 10))-1)
		}
		f := parts[1]
		for len(f) < 2 {
			f += "0"
		}
		frac, err = strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("fundoperations: 非法份额 %q", s)
		}
	}
	q := intPart*ShareScale + frac
	if neg && q != 0 {
		q = -q
	}
	return Qty(q), nil
}

// String 输出十进制份额，始终保留两位小数。
func (q Qty) String() string {
	neg := q < 0
	v := int64(q)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%s%d.%02d", negSign(neg), v/ShareScale, v%ShareScale)
	return s
}

func negSign(neg bool) string {
	if neg {
		return "-"
	}
	return ""
}

// Ratio 表示转换比例 oldNum/oldDen -> newNum/newDen。
// 例如 1 拆 2 为 1/1 -> 2/1；2 合 1 为 2/1 -> 1/1。
type Ratio struct {
	OldNum int64
	OldDen int64
	NewNum int64
	NewDen int64
}

// ParseRatio 以 "old:new" 或 "oldNum/oldDen:newNum/newDen" 形式解析比例，
// 例如 "1:2"、"1/2:3/4"。比例两端均必须为正。
func ParseRatio(s string) (Ratio, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return Ratio{}, fmt.Errorf("fundoperations: 非法比例 %q，应为 old:new", s)
	}
	on, od, err := parseFraction(parts[0])
	if err != nil {
		return Ratio{}, err
	}
	nn, nd, err := parseFraction(parts[1])
	if err != nil {
		return Ratio{}, err
	}
	r := Ratio{OldNum: on, OldDen: od, NewNum: nn, NewDen: nd}
	if !r.Valid() {
		return Ratio{}, fmt.Errorf("fundoperations: 非法比例 %q，比例必须为正", s)
	}
	return r, nil
}

func parseFraction(s string) (num, den int64, err error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		fs := strings.SplitN(s, "/", 2)
		num, err = strconv.ParseInt(strings.TrimSpace(fs[0]), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("fundoperations: 非法分数 %q", s)
		}
		den, err = strconv.ParseInt(strings.TrimSpace(fs[1]), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("fundoperations: 非法分数 %q", s)
		}
	} else {
		num, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("fundoperations: 非法分数 %q", s)
		}
		den = 1
	}
	if num <= 0 || den <= 0 {
		return 0, 0, fmt.Errorf("fundoperations: 分数必须为正 %q", s)
	}
	return num, den, nil
}

// Valid 判断比例是否合法（各分量均为正）。
func (r Ratio) Valid() bool {
	return r.OldNum > 0 && r.OldDen > 0 && r.NewNum > 0 && r.NewDen > 0
}

// IsSplit 判断是否为拆分（每单位旧份额换得多于一单位新份额）。
func (r Ratio) IsSplit() bool {
	return big.NewInt(r.NewNum*r.OldDen).Cmp(big.NewInt(r.OldNum*r.NewDen)) > 0
}

// IsMerge 判断是否为合并。
func (r Ratio) IsMerge() bool {
	return big.NewInt(r.NewNum*r.OldDen).Cmp(big.NewInt(r.OldNum*r.NewDen)) < 0
}

func (r Ratio) String() string {
	oldS := strconv.FormatInt(r.OldNum, 10)
	if r.OldDen != 1 {
		oldS += "/" + strconv.FormatInt(r.OldDen, 10)
	}
	newS := strconv.FormatInt(r.NewNum, 10)
	if r.NewDen != 1 {
		newS += "/" + strconv.FormatInt(r.NewDen, 10)
	}
	return oldS + ":" + newS
}

// Fraction 是无法整除时保留下来的零碎份额分子/分母。
// 例如 0.333... 份记为 Num=1, Den=3（单位为“份”，非 ShareScale 单位）。
type Fraction struct {
	Num int64
	Den int64
}

func (f Fraction) valid() bool {
	return f.Num >= 0 && f.Den > 0 && f.Num < f.Den
}

func (f Fraction) String() string {
	if f.Den == 0 {
		return "0"
	}
	return fmt.Sprintf("%d/%d", f.Num, f.Den)
}

// apply 计算 oldUnits（ShareScale 单位的整数份额）按比例转换后的
// 完整新份额（向下取整到 0.01 份）与按“份”计的新份额零碎差额。
// 例如比例 1:3、oldUnits=100（1 份）时，newUnits=300、frac 为零；
// 比例 2:1、oldUnits=100 时，newUnits=50、frac 为零；
// 比例 1:0.33...（1/1:1/3）、oldUnits=100 时，newUnits=33，frac=1/300。
func (r Ratio) apply(oldUnits int64) (newUnits int64, frac Fraction) {
	// 旧份额精确值 = oldUnits/ShareScale 份。
	// 新份额精确值（份）= oldUnits * NewNum * OldDen * 1 ...
	// 分子（份 × 分母 OldDen*NewDen*ShareScale）：
	num := big.NewInt(oldUnits)
	num.Mul(num, big.NewInt(r.NewNum))
	num.Mul(num, big.NewInt(r.OldDen))
	den := big.NewInt(r.NewDen)
	den.Mul(den, big.NewInt(r.OldNum))
	den.Mul(den, big.NewInt(ShareScale))

	wholeShares := new(big.Int).Quo(num, den)
	remShares := new(big.Int).Mod(num, den)

	// 份 × ShareScale = 最小单位。
	scaled := new(big.Int).Mul(num, big.NewInt(ShareScale))
	whole := new(big.Int).Quo(scaled, den)
	rem := new(big.Int).Mod(scaled, den)
	_ = wholeShares
	_ = remShares

	if !whole.IsInt64() {
		panic("fundoperations: 转换后份额溢出 int64")
	}
	newUnits = whole.Int64()
	if rem.Sign() > 0 {
		// rem/den 是以最小单位计的零头，换算为以份计的分子：
		// 零头（份）= rem / (den * ShareScale)，先约分。
		fn := new(big.Int).Set(rem)
		fd := new(big.Int).Set(den)
		fd.Mul(fd, big.NewInt(ShareScale))
		g := new(big.Int).GCD(nil, nil, fn, fd)
		fn.Quo(fn, g)
		fd.Quo(fd, g)
		if fn.IsInt64() && fd.IsInt64() {
			frac = Fraction{Num: fn.Int64(), Den: fd.Int64()}
		}
	}
	return newUnits, frac
}

var dateDay = [...]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// Date 规范化为 UTC 零点，全系统的登记日、生效日均使用日期而非时间戳。
func Date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// ParseDate 解析 YYYY-MM-DD。
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("fundoperations: 非法日期 %q", s)
	}
	return t.UTC(), nil
}

// FormatDate 格式化为 YYYY-MM-DD。
func FormatDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
