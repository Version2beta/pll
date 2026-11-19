package pll_test

import (
	"math/big"
	"testing"
	"time"
)

// The two benchmarks below compute the same exact slot times at 30000/1001
// units per second, a rate with no exact period in nanoseconds: one with
// Span.Time, and one the obvious way, with big.Rat.

// BenchmarkSlotTimeSpan reads slot times from a Span.
func BenchmarkSlotTimeSpan(b *testing.B) {
	p := benchPacer(b, 30_000, 1_001, 0)
	const n = 1 << 30
	s, err := p.Claim(n)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sum time.Duration
	for i := 0; i < b.N; i++ {
		sum += s.Time(uint64(i) % n)
	}
	_ = sum
}

// BenchmarkSlotTimeBigRat computes floor(i × period) with big.Rat, reusing
// its values between iterations.
func BenchmarkSlotTimeBigRat(b *testing.B) {
	period := new(big.Rat).SetFrac64(1_001*1_000_000_000, 30_000) // nanoseconds
	k, t, q := new(big.Rat), new(big.Rat), new(big.Int)
	b.ReportAllocs()
	b.ResetTimer()
	var sum int64
	for i := 0; i < b.N; i++ {
		k.SetInt64(int64(i % (1 << 30)))
		t.Mul(k, period)
		q.Quo(t.Num(), t.Denom())
		sum += q.Int64()
	}
	_ = sum
}
