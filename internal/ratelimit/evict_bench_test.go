package ratelimit

import (
	"fmt"
	"testing"
)

// BenchmarkGetLimiterExistingHost measures the hot path: an already-cached
// host, no eviction work.
func BenchmarkGetLimiterExistingHost(b *testing.B) {
	m := New()
	m.GetLimiter("cdn.example.com")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.GetLimiter("cdn.example.com")
	}
}

// BenchmarkGetLimiterNewHostAtCapacity measures the eviction cliff: once the
// cache holds maxLimiters hosts, every new host triggers evictLocked while
// the manager mutex is held, blocking all other GetLimiter callers.
func BenchmarkGetLimiterNewHostAtCapacity(b *testing.B) {
	m := New()
	for i := 0; i < maxLimiters; i++ {
		m.GetLimiter(fmt.Sprintf("h%d.example.com", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.GetLimiter(fmt.Sprintf("new%d.example.com", i))
	}
}
