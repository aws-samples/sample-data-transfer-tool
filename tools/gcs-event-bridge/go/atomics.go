package main

import "sync/atomic"

// ld 原子读取当前计数值。
func ld(p *int64) int64 { return atomic.LoadInt64(p) }
