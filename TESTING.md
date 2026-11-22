# Testing pll

The tests check the behavior written in [SPEC.md](https://github.com/Version2beta/pll/blob/main/SPEC.md). The library tests use only the exported API, so they hold any correct implementation to the same standard.

## Running them

```sh
make verify   # gofmt, go vet, go test, and go test -race
make fuzz     # 30 seconds on each fuzz target
make sim      # the follower scenarios, as a table
make bench    # benchmarks, with allocations
```

CI runs `make verify`'s steps on Go 1.23 and on the current release, and fuzzes each target for 30 seconds.

## What each file checks

| File | Checks |
| --- | --- |
| `rate_test.go` | `UnitsPerSecond` and `BitsPerSecond` reduce to lowest terms and reject zeros; `Rate.String` |
| `pacer_test.go` | Each `Pacer` method on hand-worked cases: exact floor times, the ceiling in `NextDue`, capped backlog at an earlier `now`, `Skip`, trim anchoring and retained/shared span timing, floor-fit versus ceiling-overflow boundaries |
| `model_test.go` | The naive pacer model, and the harness that drives it beside the real `Pacer` and checks invariants after every operation |
| `proof_test.go` | Boundary cases at the numeric limits, a wide reduced period despite 32-bit rate terms, the randomized differential test, `FuzzPacer`, and zero allocations for representative fast-path periods |
| `follower_test.go` | Config validation and ordering, anchoring, reading order including pending/rejected samples, lock/holdover and window boundaries, outlier runs, jitter/reset, overflow atomicity, wide-displacement mappings against an integer oracle, extreme finite gains against high-precision arithmetic, trim/error saturation, and precision past 2^53 ns and over 1000 simulated days |
| `followermodel_test.go` | The literal model of the loop equations |
| `followerproof_test.go` | The differential test against the loop model, invariants on random and fuzzed readings (`FuzzFollower`), zero allocations, the simulator scenarios with their thresholds, the device sink, and late wake-ups |
| `example_test.go`, `example_follower_test.go` | The examples on pkg.go.dev, run as tests |
| `readme_test.go` | The README pacing helper matches the compiled helper and cancels when its consumer stops |
| `cmd/pllpace/main_test.go` | Flag boundaries, writer errors and short writes, final partial groups, trial isolation and deadlines, deterministic maximum-rate bisection progress and termination |
| `internal/sim/sim_test.go` | Independent late-wakeup totals, including fractional rates and a capped run that leaves a backlog |
| `bench_test.go` | Benchmarks: one slot per call, 64 slots per call, with a trim, and claiming by bytes |
| `bench_compare_test.go` | The same exact slot times at 30000/1001 per second, read from a `Span` and computed with `big.Rat`, for comparison |

## Reference models

A reference model is a second implementation written to be obviously right rather than fast. When the two disagree, investigate both against SPEC.md; the simpler model is an oracle, not a proof that it cannot be wrong.

- **Pacer.** `model_test.go` recomputes every slot time from the formula in SPEC.md 1.3 with `big.Rat`, from scratch, on every call. It has no fast path and never reduces a fraction. It even checks its own answer to "which slot is due" against the neighboring slots. The differential test and the fuzz target run the real `Pacer` and the model through the same operations and compare every result, error included.
- **Follower.** `followermodel_test.go` applies the loop equations from SPEC.md 2.5 literally, with no window, outliers, lock or jitter. The differential test feeds both the same noisy readings, with outlier rejection and the clamp out of the way, and compares the error, the rate estimate and the mapping after every reading. That float64 model is used at moderate times and gains, not as an oracle at numeric extremes; hand-worked and high-precision boundary tests cover those separately.
- **Simulator.** `internal/sim` adds an independent least-squares line fit through the readings, so the scenarios check the follower's rate estimate against a method that shares none of its code.

## Invariants

After every pacer operation:

- the cursor never moves back;
- spans are contiguous and never overlap;
- times never decrease;
- every slot `Due` hands out was due;
- a trim never changes a span already handed out.

After every follower reading:

- out-of-order readings return `ErrOrder` and nothing else;
- the rate estimate stays within `MaxOffsetPPM`;
- `Local` never decreases as remote ticks increase;
- an accepted update moves the phase by at most the error that caused it;
- a rejected or pending reading leaves the clock mapping unchanged; reading-order, window and outlier metadata may change;
- a step anchors exactly on the stepping reading.

## Why the pacer is exact

Each stretch of timing between trims is a segment: slot `s` is at `A + (s − S) × pn/pd` nanoseconds, where `A` is a whole number and `pn/pd` is the period reduced to lowest terms.

- **Times.** When `pn` and `pd` fit in 64 bits, `k × pn` is computed exactly as a 128-bit product. `bits.Div64` then gives the exact floor and remainder whenever the high word is below `pd`. When it is not, the quotient is at least 2^64, far beyond the largest `time.Duration`, so reporting overflow is the exact answer. Otherwise the arithmetic falls back to `math/big`, which is exact.
- **Which slots are due.** Slot `s` is due when `(s − S) × pn ≤ (now − A) × pd`, which is the same as `s − S ≤ floor((now − A) × pd / pn)`. That is computed the same two ways, so the comparison is exact, never rounded.
- **No growing fractions.** A trim starts a new segment at the floor of the cursor slot's exact time. Anchors stay whole numbers and every period's denominator stays at most `num × (10^12 + trim)`, so fractions never pile up across trims.

`TestBoundaries` reaches each extreme: a rate of 2^64 − 1 units per second, a denominator of 2^64 − 1, slots near 2^64, times near the largest `time.Duration`, and trims of ±(10^12 − 1).

## Fuzzing

```sh
go test -run '^$' -fuzz '^FuzzPacer$' -fuzztime 30s .
go test -run '^$' -fuzz '^FuzzFollower$' -fuzztime 30s .
```

`FuzzPacer` decodes a rate and up to 48 operations from bytes and runs them through the differential harness. `FuzzFollower` decodes a config and up to 64 readings, including out-of-order and huge ones, and checks the invariants after each.

A failure is saved under `testdata/fuzz/` and replays on every `go test` from then on. `testdata/fuzz/FuzzFollower/007889dba9202302` is one the fuzzer found during development: a local time past 2^53 ns, where a float64 can no longer hold every nanosecond. It is why the follower keeps its anchor as whole nanoseconds plus a fraction.

## The simulator

`go run ./cmd/pllsim` runs each scenario and prints what it measured. Every run is deterministic, so the same seed gives the same table everywhere. `TestScenarios` holds each scenario to thresholds about twice the worst a reference implementation measured over three seeds.

| Scenario | Remote clock and delivery | Held to |
| --- | --- | --- |
| sound-card | +80 ppm, a reading every 10 ms, 20 µs of jitter | Locked by 200 ms; RMS error ≤ 20 µs; rate within 2 ppm |
| sensor-network | −150 ppm; readings every 100 ms with 2 ms of jitter, 2% heavy-tailed spikes, in batches of 8; network settings | Locked by 3 min; RMS error ≤ 1.5 ms; rate within 15 ppm; one step to recover from a delayed first reading |
| rate-change | 0 ppm, then +100 ppm at 60 s | As sound-card, after it settles |
| outage | +30 ppm, no readings from 60 s to 90 s | Relocks within 1 s; error ≤ 60 µs |
| clock-step | +20 ppm, jumps 2 s at 60 s | Exactly one step; relocks within 1 s; no slewing |
| aging-crystal | +10 ppm, drifting 1 ppm per hour, for 2 hours | RMS error ≤ 20 µs; rate within 4 ppm |
| device-sink | A device consuming 40 ppm fast, the pacer linked by queue level | Queue stays within 10 units for 5 minutes; unlinked, it drifts by more than 400 |
| late-wakeups | The pacer woken up to 20 ms late | Every due slot handed out exactly once, none early |

## Measuring a pacing loop

The benchmarks time single calls. `cmd/pllpace` times a whole loop: it sleeps until the next slot is due, hands out what is due, writes each unit to an output, and reports offered units, writer-accepted bytes and their rate, how far behind schedule it ended, and how late the oldest unit was at each wake-up.

```sh
go run ./cmd/pllpace -seconds 5
go run ./cmd/pllpace -mode ticker -seconds 5
go run ./cmd/pllpace -find-max -seconds 2 -udp loopback
```

`-mode ticker` uses one timer tick per unit instead of the pacer, for comparison. `-find-max` requires a nonempty trial and doubles the rate until a trial ends more than `-slack` (5 ms) behind schedule, or its 99th-percentile lateness exceeds it, then narrows the range to within 5% or to adjacent integer bit rates. If integer resolution prevents reaching 5%, it reports that limit and the exact passing/failing bit rates. `-udp loopback` sends each unit to a UDP receiver in the same process; `-per-datagram` groups units into larger datagrams. Each trial flushes its final partial group; elapsed time and byte counts include that write. Write errors (including short writes) stop the run, and buffered bytes never carry into the next trial. Offered units are units passed to the sink; writer-accepted bytes count the bytes returned by `Write`, not UDP acknowledgment or receiver delivery. Rates include the whole trial, not just the last wake-up. Invalid sizes, durations, modes and unrepresentable ticker periods are rejected before opening sockets. Results depend on the machine and are noisy near the limit, so run a search more than once.

## Plant a bug yourself

The best way to trust a test suite is to break the code and watch it fail. Make one of these changes, run `go test .`, see which tests catch it, then undo only that experimental edit. Start from a clean worktree so unrelated changes are not lost.

| File | Change | Caught by |
| --- | --- | --- |
| `pacer.go`, in `ceilTime` | `if r != 0 {` → `if false && r != 0 {` (forget to round up) | `TestNextDue`, `TestBoundaries`, `TestDifferential` |
| `pacer.go`, in `Due` | `if n > max {` → `if n > max+1 {` and `n = max` → `n = max + 1` | `TestDueLimits`, `TestOverflow`, `TestBoundaries`, `TestDifferential`, `FuzzPacer`, and more |
| `pacer.go`, in `SetTrim` | delete the early return when the trim is unchanged | `TestSetTrim`, `TestDifferential` |
| `follower.go`, in `accept` | `f.y-(x*x/dt)*e/1e9` → `f.y+(x*x/dt)*e/1e9` (positive feedback) | Eleven tests, including `TestLoopEquations` and `TestScenarios` |
| `follower.go`, in `accept` | `x := math.Min(0.5, wn*dt)` → `x := wn * dt` (no gain cap) | `TestLoopEquations`, `TestFollowerDifferential`, `TestFollowerLongRun`, `TestScenarios` |
| `follower.go`, in `Observe` | `e < f.candErr` → `e > f.candErr` (keep the most delayed reading) | `TestWindow`, `TestScenarios` |

If you find a change that no test catches, that is a missing test. Please open an issue.
