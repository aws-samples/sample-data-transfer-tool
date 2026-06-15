package main

import "sync/atomic"

// atomicAdd 计数 +1（封装裸 atomic，统一计数风格）。
func atomicAdd(p *int64) { atomic.AddInt64(p, 1) }

// ldAdd 读取当前值（delta=0 时即纯读；保持与 atomicAdd 同源语义）。
func ldAdd(p *int64, delta int64) int64 {
	if delta == 0 {
		return atomic.LoadInt64(p)
	}
	return atomic.AddInt64(p, delta)
}
