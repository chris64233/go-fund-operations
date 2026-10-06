package fundoperations

import "sync/atomic"

// 测试用的轻量原子计数，避免在每个测试文件重复声明。
func atomicAdd(p *int32) { atomic.AddInt32(p, 1) }
