package main

import (
	"bytes"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

type recordingWriter struct {
	writes [][]byte
	err    error
	short  bool
	delay  time.Duration
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.writes = append(w.writes, append([]byte(nil), p...))
	if w.err != nil {
		if w.short {
			return len(p) - 1, w.err
		}
		return 0, w.err
	}
	if w.short {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func testUnitBytes(t *testing.T, n uint64) {
	t.Helper()
	old := *unitBytes
	*unitBytes = n
	t.Cleanup(func() { *unitBytes = old })
}

func TestRunPacerWriterFailure(t *testing.T) {
	testUnitBytes(t, 1)
	failure := errors.New("write failed")
	for _, tc := range []struct {
		name string
		w    *recordingWriter
		want error
	}{
		{"error", &recordingWriter{err: failure}, failure},
		{"short write", &recordingWriter{short: true}, io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &sink{w: tc.w, per: 1, size: 1}
			_, err := runPacer(8000, 2*time.Millisecond, out, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("runPacer error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunPacerFlushesEachTrial(t *testing.T) {
	testUnitBytes(t, 1)
	w := &recordingWriter{}
	out := &sink{w: w, per: 100, size: 1}
	for trial := 0; trial < 2; trial++ {
		r, err := runPacer(8, 2*time.Millisecond, out, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(w.writes) != trial+1 || len(out.buf) != 0 {
			t.Fatalf("trial %d: %d writes, %d pending bytes; want one partial flush per trial", trial, len(w.writes), len(out.buf))
		}
		if r.offered != 1 || r.written != 1 || r.elapsed < 2*time.Millisecond {
			t.Fatalf("trial units = %d, want 1", r.offered)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	good := options{bps: 8_000_000, unitBytes: 1000, seconds: 1, mode: "pacer", perDatagram: 1, slack: time.Millisecond}
	for _, tc := range []struct {
		name, want string
		change     func(*options)
	}{
		{"zero bps", "-bps", func(o *options) { o.bps = 0 }},
		{"zero bytes", "-unit-bytes", func(o *options) { o.unitBytes = 0 }},
		{"bit overflow", "-unit-bytes", func(o *options) { o.unitBytes = math.MaxUint64/8 + 1 }},
		{"zero seconds", "-seconds", func(o *options) { o.seconds = 0 }},
		{"negative seconds", "-seconds", func(o *options) { o.seconds = -1 }},
		{"nan seconds", "-seconds", func(o *options) { o.seconds = math.NaN() }},
		{"infinite seconds", "-seconds", func(o *options) { o.seconds = math.Inf(1) }},
		{"negative infinite seconds", "-seconds", func(o *options) { o.seconds = math.Inf(-1) }},
		{"subnanosecond", "-seconds", func(o *options) { o.seconds = 0.5e-9 }},
		{"duration overflow", "-seconds", func(o *options) { o.seconds = float64(math.MaxInt64) / 1e9 }},
		{"mode", "-mode", func(o *options) { o.mode = "bad"; o.udp = "loopback" }},
		{"zero group", "-per-datagram", func(o *options) { o.perDatagram = 0 }},
		{"negative group", "-per-datagram", func(o *options) { o.perDatagram = -1 }},
		{"group overflow", "-per-datagram", func(o *options) { o.perDatagram = int(^uint(0) >> 1) }},
		{"UDP size", "UDP payload", func(o *options) { o.udp = "loopback"; o.perDatagram = 66 }},
		{"ticker too fast", "ticker period", func(o *options) { o.mode = "ticker"; o.unitBytes = 1; o.bps = 8_000_000_001 }},
		{"ticker too slow", "ticker period", func(o *options) { o.mode = "ticker"; o.unitBytes = 2_000_000_000; o.bps = 1 }},
		{"ticker search", "-find-max", func(o *options) { o.mode = "ticker"; o.findMax = true }},
		{"search bound", "-bps", func(o *options) { o.findMax = true; o.bps = searchLimit + 1 }},
		{"negative slack", "-slack", func(o *options) { o.slack = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.change(&o)
			if _, err := o.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate: %v, want error containing %q", err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*options)
	}{
		{"pacer", func(o *options) {}},
		{"ticker", func(o *options) { o.mode = "ticker" }},
		{"one ns", func(o *options) { o.mode = "ticker"; o.unitBytes = 1; o.bps = 8_000_000_000; o.seconds = 1e-9 }},
		{"UDP limit", func(o *options) { o.udp = "loopback"; o.unitBytes = 65507 }},
		{"search limit", func(o *options) { o.findMax = true; o.bps = searchLimit }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.change(&o)
			if d, err := o.validate(); err != nil || d <= 0 {
				t.Fatalf("validate = %v, %v", d, err)
			}
		})
	}
}

func TestTickerPeriod(t *testing.T) {
	for _, tc := range []struct {
		bps, size uint64
		want      time.Duration
	}{
		{8_000_000_000, 1, time.Nanosecond},
		{3, 1, 2_666_666_666},
		{8, 1, time.Second},
	} {
		got, err := tickerPeriod(tc.bps, tc.size)
		if err != nil || got != tc.want {
			t.Errorf("tickerPeriod(%d, %d) = %v, %v; want %v", tc.bps, tc.size, got, err, tc.want)
		}
	}
}

func TestSinkGroupsAndPartialWrites(t *testing.T) {
	w := &recordingWriter{}
	s := &sink{w: w, per: 2, size: 2}
	for _, unit := range [][]byte{{1, 2}, {3, 4}, {5, 6}} {
		if err := s.unit(unit); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.writes) != 1 || !bytes.Equal(w.writes[0], []byte{1, 2, 3, 4}) || s.written != 4 {
		t.Fatalf("full group: writes=%v, bytes=%d", w.writes, s.written)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 2 || !bytes.Equal(w.writes[1], []byte{5, 6}) || s.written != 6 || len(s.buf) != 0 {
		t.Fatalf("partial group: writes=%v, bytes=%d, pending=%d", w.writes, s.written, len(s.buf))
	}
	w.short = true
	if err := s.unit([]byte{7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); !errors.Is(err, io.ErrShortWrite) || s.written != 7 || len(s.buf) != 0 {
		t.Fatalf("short partial flush: err=%v, bytes=%d, pending=%d", err, s.written, len(s.buf))
	}
}

func TestTrialAccounting(t *testing.T) {
	testUnitBytes(t, 2)
	failure := errors.New("write failed")
	for _, runner := range []struct {
		name string
		run  func(uint64, time.Duration, *sink) (result, error)
	}{
		{"pacer", func(b uint64, d time.Duration, s *sink) (result, error) { return runPacer(b, d, s, false) }},
		{"ticker", runTicker},
	} {
		t.Run(runner.name, func(t *testing.T) {
			w := &recordingWriter{err: failure}
			out := &sink{w: w, per: 1, size: 2}
			r, err := runner.run(16000, 100*time.Millisecond, out)
			if !errors.Is(err, failure) || r.offered != 1 || r.written != 0 || len(out.buf) != 0 {
				t.Fatalf("failed trial: %+v, %v, pending=%d", r, err, len(out.buf))
			}
			w.err, w.short = nil, true
			r, err = runner.run(16000, 100*time.Millisecond, out)
			if !errors.Is(err, io.ErrShortWrite) || r.offered != 1 || r.written != 1 || len(out.buf) != 0 {
				t.Fatalf("short trial: %+v, %v, pending=%d", r, err, len(out.buf))
			}
			w.short = false
			out.per = 1000 // force a final partial group
			r, err = runner.run(16000, 10*time.Millisecond, out)
			if err != nil || r.offered == 0 || r.written != r.offered*2 || len(out.buf) != 0 || r.elapsed < 10*time.Millisecond {
				t.Fatalf("successful trial: %+v, %v, pending=%d", r, err, len(out.buf))
			}
			// Errors on the final partial group must propagate too.
			w.err = failure
			r, err = runner.run(16000, 10*time.Millisecond, out)
			if !errors.Is(err, failure) || r.offered == 0 || r.written != 0 || len(out.buf) != 0 {
				t.Fatalf("failed final flush: %+v, %v, pending=%d", r, err, len(out.buf))
			}
		})
	}
}

func TestLowRateTrialDeadline(t *testing.T) {
	testUnitBytes(t, 1)
	out := &sink{w: io.Discard, per: 1, size: 1}
	done := make(chan error, 1)
	go func() {
		_, err := runPacer(1, time.Millisecond, out, false)
		if err == nil {
			_, err = runTicker(1, time.Millisecond, out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("trial waited for a slot beyond its deadline")
	}
}

func TestRunPacerFinalFlushAccounting(t *testing.T) {
	testUnitBytes(t, 1)
	const delay = 20 * time.Millisecond
	w := &recordingWriter{delay: delay}
	out := &sink{w: w, per: 1000, size: 1}
	r, err := runPacer(8000, 2*time.Millisecond, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 || r.written != r.offered || r.written == 0 || r.elapsed < 2*time.Millisecond+delay || r.behind < delay/2 {
		t.Fatalf("final flush missing from elapsed/count/backlog: %+v, writes=%d", r, len(w.writes))
	}
}

func TestSinkCountsBytesWithError(t *testing.T) {
	failure := errors.New("partial failure")
	w := &recordingWriter{short: true, err: failure}
	out := &sink{w: w, per: 1, size: 2}
	if err := out.unit([]byte{1, 2}); !errors.Is(err, failure) || out.written != 1 || len(out.buf) != 0 {
		t.Fatalf("partial failure: %v, bytes=%d, pending=%d", err, out.written, len(out.buf))
	}
}

func TestBisectRate(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		lo, hi, highestPassing uint64
		wantLo, wantHi         uint64
		wantTrials             []uint64
	}{
		{"adjacent minimum", 1, 2, 1, 1, 2, nil},
		{"adjacent below five percent precision", 19, 20, 19, 19, 20, nil},
		{"at five percent", 20, 21, 20, 20, 21, nil},
		{"failing midpoint reaches adjacency", 1, 4, 1, 1, 2, []uint64{2}},
		{"passing midpoint reaches adjacency", 1, 4, 2, 2, 3, []uint64{2, 3}},
		{"multiple steps to adjacency", 1, 8, 2, 2, 3, []uint64{4, 2, 3}},
		{"five percent before adjacency", 1_000_000, 2_000_000, 1_500_000, 1_500_000, 1_562_500, []uint64{1_500_000, 1_750_000, 1_625_000, 1_562_500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trials []uint64
			previousLo, previousHi := tc.lo, tc.hi
			lo, hi := bisectRate(tc.lo, tc.hi, func(rate uint64) bool {
				// Fail synchronously on a repeated endpoint, rather than hang
				// if integer rounding prevents the production loop advancing.
				if rate <= previousLo || rate >= previousHi {
					t.Fatalf("trial %d outside open bracket (%d, %d)", rate, previousLo, previousHi)
				}
				trials = append(trials, rate)
				if len(trials) > len(tc.wantTrials) {
					t.Fatalf("too many trials: %v", trials)
				}
				pass := rate <= tc.highestPassing
				if pass {
					previousLo = rate
				} else {
					previousHi = rate
				}
				return pass
			})
			if lo != tc.wantLo || hi != tc.wantHi || !slices.Equal(trials, tc.wantTrials) {
				t.Fatalf("bisectRate = (%d, %d), trials %v; want (%d, %d), trials %v",
					lo, hi, trials, tc.wantLo, tc.wantHi, tc.wantTrials)
			}
		})
	}
}

func TestResultSustained(t *testing.T) {
	const slack = time.Millisecond
	for _, tc := range []struct {
		name string
		r    result
		want bool
	}{
		{"empty", result{}, false},
		{"at bounds", result{offered: 1, behind: slack, worst: []time.Duration{slack}}, true},
		{"backlog", result{offered: 1, behind: slack + 1}, false},
		{"late wake", result{offered: 1, worst: []time.Duration{slack + 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.sustained(slack); got != tc.want {
				t.Fatalf("sustained = %v, want %v", got, tc.want)
			}
		})
	}
}
