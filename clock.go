package fundoperations

import "time"

// Clock 统一时钟，截止边界判断与批次时间戳均取自同一时钟。
type Clock interface {
	Now() time.Time
}

// SystemClock 系统时钟。
type SystemClock struct{}

// Now 返回当前时间。
func (SystemClock) Now() time.Time { return time.Now() }
