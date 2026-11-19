package sim_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/version2beta/pll"
	"github.com/version2beta/pll/internal/sim"
)

func TestRunLateWakeupsExpected(t *testing.T) {
	late := time.Duration(rand.New(rand.NewPCG(1, 0x1a7e)).Int64N(int64(2*time.Second) + 1))
	for _, tc := range []struct {
		name                    string
		num, den                uint64
		wake, maxLate, duration time.Duration
		want                    uint64
	}{
		{"zero terminal wake", 3, 2, time.Second, 0, 0, 1},
		{"fractional rate", 3, 2, time.Second, 0, 2 * time.Second, 4},
		{"fractional slot not yet due", 3, 2, time.Second, 0, time.Second, 2},
		{"late terminal wake", 30000, 1001, time.Second, 2 * time.Second, 0, uint64(int64(late)*30000/(1001*1e9)) + 1},
		// Due is deliberately capped at 1<<20 per wake. A one-second gap
		// at this rate leaves a backlog: the oracle must not call it complete.
		{"bounded under-delivery", 2_000_000, 1, time.Second, 0, time.Second, 2_000_001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate, err := pll.UnitsPerSecond(tc.num, tc.den)
			if err != nil {
				t.Fatal(err)
			}
			r, err := sim.RunLateWakeups(rate, tc.wake, tc.maxLate, tc.duration, 1)
			if err != nil {
				t.Fatal(err)
			}
			if r.Expected != tc.want {
				t.Fatalf("Expected = %d, want %d; %+v", r.Expected, tc.want, r)
			}
			if tc.name == "bounded under-delivery" {
				if r.HandedOut >= r.Expected {
					t.Fatalf("oracle missed under-delivery: %+v", r)
				}
			} else if r.HandedOut != r.Expected || r.Early != 0 || r.Gaps != 0 {
				t.Fatalf("incomplete run: %+v", r)
			}
		})
	}
}
