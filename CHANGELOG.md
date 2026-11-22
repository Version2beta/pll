# Changelog

One entry per released version, newest first.

## v0.4.0 — 2026-11-22

- `cmd/pllpace`: a real-time pacing loop that reports the rate it achieved and how late each wake-up ran. It can compare one timer tick per unit, send units over UDP, and search for the highest rate it sustains (`-find-max`).
- Follower boundary fixes: failed observations leave state unchanged on overflow; extreme finite loop gains remain stable; mappings support displacements wider than `int64`; trim and reported phase errors saturate safely. Mapping rounding is specified relative to the integer anchor. No exported signatures changed.
- Pacing-tool validation rejects invalid flags and unrepresentable ticker periods before opening sockets. Trials include final partial writes, stop on writer errors or short writes, and do not carry buffered bytes into later trials. Maximum-rate searches terminate at adjacent integer bit rates and report the resolution limit.
- Clarified exact-floor versus due-time rounding, fast-path allocation limits, and read-only span ownership; added boundary regressions and independent simulator-total checks.
- Benchmarks comparing slot times read from a `Span` with the same times computed with `big.Rat`.

## v0.3.2 — 2026-11-12

Documentation only. The README now says that `Span.Time` reports a slot's scheduled time rounded down to whole nanoseconds, as its reference section always did. v0.3.1 was tagged on the v0.3.0 commit by mistake, so it is identical to v0.3.0 and is retracted.

## v0.3.0 — 2026-11-12

The first release under `github.com/version2beta/pll`. Version numbers below v0.3.0 belong to an earlier, withdrawn API and do not build under this module path.

- `Rate`: an exact number of units per second, from `UnitsPerSecond` or `BitsPerSecond`.
- `Pacer`: evenly spaced, numbered slots at an exact rate, with `Due`, `Claim`, `Skip`, `NextDue` and a trim in parts per trillion. Times never drift and the hot paths do not allocate.
- `Span`: a run of handed-out slots with their exact times, and an iterator over them.
- `Follower`: a type-II phase-locked loop configured by bandwidth and damping, with a lower-envelope window for jittery delivery, outlier rejection, steps when the other clock jumps, lock and holdover states, and a running jitter estimate. Its `Trim` drives a `Pacer`.
- `cmd/pllsim`: a deterministic simulator that runs the follower against realistic clocks and prints what it measured.
