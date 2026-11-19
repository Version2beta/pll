// Command pllpace runs a real-time pacing loop with a pll.Pacer and reports
// offered units, writer-accepted bytes, schedule backlog and wake-up lateness. It hands units of a chosen size to an output: discarded by
// default, or sent as UDP datagrams.
//
// Usage:
//
//	pllpace [flags]
//
// With -mode ticker it uses one timer tick per unit instead, for comparison.
// With -find-max it doubles the rate from -bps until a trial falls behind,
// then narrows the range, and reports the highest rate it sustained.
//
// With -udp loopback it starts a receiver in the same process on 127.0.0.1
// and sends to it, so the result includes the cost of each send. A successful
// UDP write does not acknowledge receipt; no received-unit count is measured.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"os"
	"slices"
	"time"

	"github.com/version2beta/pll"
)

var (
	bps       = flag.Uint64("bps", 100_000_000, "target rate in bits per second (the starting rate, with -find-max)")
	unitBytes = flag.Uint64("unit-bytes", 1000, "size of each unit in bytes")
	secs      = flag.Float64("seconds", 10, "how long to run (each trial, with -find-max)")
	mode      = flag.String("mode", "pacer", "pacer, or ticker for one timer tick per unit")
	udp       = flag.String("udp", "", `send units as UDP datagrams to host:port, or to "loopback" for a receiver in this process (default: discard)`)
	perDgm    = flag.Int("per-datagram", 1, "units per UDP datagram, with -udp")
	findMax   = flag.Bool("find-max", false, "search for the highest rate the pacing loop sustains")
	slack     = flag.Duration("slack", 5*time.Millisecond, "with -find-max: how far behind schedule a trial may run and still pass")
)

// options are validated before allocating payloads or opening UDP sockets.
type options struct {
	bps, unitBytes uint64
	seconds        float64
	mode, udp      string
	perDatagram    int
	findMax        bool
	slack          time.Duration
}

const searchLimit = uint64(1_000_000_000_000)

func (o options) validate() (time.Duration, error) {
	ns := o.seconds * float64(time.Second)
	maxInt := uint64(^uint(0) >> 1)
	switch {
	case o.bps == 0:
		return 0, fmt.Errorf("-bps must be positive")
	case o.unitBytes == 0 || o.unitBytes > math.MaxUint64/8 || o.unitBytes > maxInt:
		return 0, fmt.Errorf("-unit-bytes must fit in int and convert to bits without overflow")
	case math.IsNaN(ns) || math.IsInf(ns, 0) || ns < 1 || ns >= float64(math.MaxInt64):
		return 0, fmt.Errorf("-seconds must be finite, positive and representable as a time.Duration")
	case o.mode != "pacer" && o.mode != "ticker":
		return 0, fmt.Errorf("-mode must be pacer or ticker")
	case o.perDatagram < 1 || uint64(o.perDatagram) > maxInt/o.unitBytes:
		return 0, fmt.Errorf("-per-datagram must be positive and its byte size must fit in int")
	case o.udp != "" && uint64(o.perDatagram)*o.unitBytes > 65507:
		return 0, fmt.Errorf("UDP payload must not exceed 65507 bytes")
	case o.findMax && o.mode != "pacer":
		return 0, fmt.Errorf("-find-max requires -mode pacer")
	case o.findMax && o.bps > searchLimit:
		return 0, fmt.Errorf("-bps must not exceed %d with -find-max", searchLimit)
	case o.slack < 0:
		return 0, fmt.Errorf("-slack must not be negative")
	}
	if o.mode == "ticker" {
		if _, err := tickerPeriod(o.bps, o.unitBytes); err != nil {
			return 0, err
		}
	}
	return time.Duration(ns), nil
}

// tickerPeriod floors the exact period; unlike a float-to-duration conversion,
// this also detects periods outside NewTicker's positive int64 domain.
func tickerPeriod(bps, size uint64) (time.Duration, error) {
	if bps == 0 || size == 0 || size > math.MaxUint64/8 {
		return 0, fmt.Errorf("invalid ticker rate or unit size")
	}
	var n big.Int
	n.SetUint64(size * 8)
	n.Mul(&n, big.NewInt(int64(time.Second)))
	n.Quo(&n, new(big.Int).SetUint64(bps))
	if !n.IsInt64() || n.Sign() <= 0 {
		return 0, fmt.Errorf("ticker period must be between 1 ns and the largest time.Duration")
	}
	return time.Duration(n.Int64()), nil
}

// sink groups units. Every trial flushes its final partial group; on a write
// failure the unwritten buffer is discarded, never carried into another trial.
type sink struct {
	w         io.Writer
	buf       []byte
	per, size int
	written   uint64 // bytes accepted by Write, not acknowledged UDP delivery
}

func (s *sink) unit(u []byte) error {
	s.buf = append(s.buf, u...)
	if len(s.buf) >= s.per*s.size {
		return s.flush()
	}
	return nil
}

func (s *sink) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	length := len(s.buf)
	n, err := s.w.Write(s.buf)
	s.written += uint64(n)
	s.buf = s.buf[:0]
	if err != nil {
		return err
	}
	if n != length {
		return io.ErrShortWrite
	}
	return nil
}

type result struct {
	offered, written, wakeups, maxBatch uint64 // units offered; bytes accepted by the writer
	elapsed                             time.Duration
	behind                              time.Duration   // how far behind schedule the output cursor is
	worst                               []time.Duration // per wake-up: how late its oldest unit was
}

func (r result) mbps() float64 {
	if r.elapsed <= 0 {
		return 0
	}
	return float64(r.written) * 8 / r.elapsed.Seconds() / 1e6
}

// sustained requires a nonempty trial as well as meeting both timing bounds.
func (r result) sustained(slack time.Duration) bool {
	return r.offered > 0 && r.behind <= slack && quantile(r.worst, 0.99) <= slack
}

// finishTrial includes the final partial write in both elapsed time and byte count.
func finishTrial(r *result, out *sink, start time.Time, initial uint64, err *error) {
	if *err == nil {
		*err = out.flush()
	}
	out.buf = out.buf[:0]
	r.written = out.written - initial
	r.elapsed = time.Since(start)
	slices.Sort(r.worst)
}

func quantile(d []time.Duration, f float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(f*float64(len(d)-1))]
}

func runPacer(bps uint64, dur time.Duration, out *sink, progress bool) (r result, err error) {
	rate, err := pll.BitsPerSecond(bps, *unitBytes*8)
	if err != nil {
		return r, err
	}
	p, err := pll.NewPacer(rate)
	if err != nil {
		return r, err
	}
	u := make([]byte, *unitBytes)
	start := time.Now()
	initial := out.written
	defer func() {
		finishTrial(&r, out, start, initial, &err)
		if err == nil {
			_, at, nextErr := p.NextDue()
			if nextErr != nil {
				err = nextErr
				return
			}
			r.behind = max(0, r.elapsed-at)
		}
	}()
	nextReport := time.Second
	for {
		now := time.Since(start)
		if now >= dur {
			break
		}
		span, err := p.Due(now, 4096)
		if err != nil {
			return r, err
		}
		r.wakeups++
		if span.Count > 0 {
			r.worst = append(r.worst, now-span.Time(0))
		}
		for range span.Count {
			r.offered++
			if err := out.unit(u); err != nil {
				return r, err
			}
		}
		r.maxBatch = max(r.maxBatch, span.Count)
		r.elapsed = time.Since(start)
		r.written = out.written - initial
		if progress && now >= nextReport {
			fmt.Printf("%6.1fs  %10d offered units  %9.3f writer Mbps\n", now.Seconds(), r.offered, r.mbps())
			nextReport += time.Second
		}
		_, at, err := p.NextDue()
		if err != nil {
			return r, err
		}
		if d := min(at-time.Since(start), dur-time.Since(start)); d > 0 {
			time.Sleep(d)
		}
	}
	return r, nil
}

func runTicker(bps uint64, dur time.Duration, out *sink) (r result, err error) {
	period, err := tickerPeriod(bps, *unitBytes)
	if err != nil {
		return r, err
	}
	ups := float64(bps) / float64(*unitBytes*8)
	u := make([]byte, *unitBytes)
	t := time.NewTicker(period)
	defer t.Stop()
	start := time.Now()
	initial := out.written
	defer func() {
		finishTrial(&r, out, start, initial, &err)
		// A ticker starts at one period, so the next ideal tick after n
		// offered units is at (n+1)/ups, even if actual ticks were dropped.
		r.behind = time.Duration(max(0, float64(r.elapsed)-float64(r.offered+1)/ups*float64(time.Second)))
	}()
	end := time.NewTimer(dur)
	defer end.Stop()
	nextReport := time.Second
	for {
		select {
		case <-end.C:
			return r, nil
		case <-t.C:
			now := time.Since(start)
			if now >= dur {
				return r, nil
			}
			r.wakeups++
			r.offered++
			if err := out.unit(u); err != nil {
				return r, err
			}
			r.maxBatch = 1
			r.elapsed = time.Since(start)
			r.written = out.written - initial
			if now >= nextReport {
				fmt.Printf("%6.1fs  %10d offered units  %9.3f writer Mbps\n", now.Seconds(), r.offered, r.mbps())
				nextReport += time.Second
			}
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pllpace:", err)
	os.Exit(1)
}

// loopback starts a UDP receiver on 127.0.0.1 that discards what it reads,
// and returns its address.
func loopback() string {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		fail(err)
	}
	_ = c.SetReadBuffer(8 << 20)
	go func() {
		buf := make([]byte, 65536)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()
	return c.LocalAddr().String()
}

func main() {
	flag.Parse()
	dur, err := (options{
		bps: *bps, unitBytes: *unitBytes, seconds: *secs, mode: *mode,
		udp: *udp, perDatagram: *perDgm, findMax: *findMax, slack: *slack,
	}).validate()
	if err != nil {
		fail(err)
	}
	out := &sink{w: io.Discard, per: 1, size: int(*unitBytes)}
	where := "discarded"
	if *udp != "" {
		addr := *udp
		if addr == "loopback" {
			addr = loopback()
		}
		conn, err := net.Dial("udp", addr)
		if err != nil {
			fail(err)
		}
		defer conn.Close()
		out = &sink{w: conn, per: *perDgm, size: int(*unitBytes)}
		where = fmt.Sprintf("UDP writes to %s, up to %d units per datagram; receipt not measured", addr, out.per)
	}

	if *findMax {
		maxRate(dur, out, where)
		return
	}

	ups := float64(*bps) / float64(*unitBytes*8)
	fmt.Printf("target  %.3f Mbps = %.2f units/s of %d bytes, one every %.2f µs (%s, %s)\n",
		float64(*bps)/1e6, ups, *unitBytes, 1e6/ups, *mode, where)
	var r result
	switch *mode {
	case "pacer":
		var err error
		if r, err = runPacer(*bps, dur, out, true); err != nil {
			fail(err)
		}
	case "ticker":
		if r, err = runTicker(*bps, dur, out); err != nil {
			fail(err)
		}
	}
	fmt.Printf("result  %d offered units, %d writer-accepted bytes in %.3fs = %.3f writer Mbps (target %.3f), %v behind schedule at the end\n",
		r.offered, r.written, r.elapsed.Seconds(), r.mbps(), float64(*bps)/1e6, r.behind.Round(time.Microsecond))
	fmt.Printf("        %d wake-ups, %.1f units per wake-up on average, %d at most\n",
		r.wakeups, float64(r.offered)/float64(max(r.wakeups, 1)), r.maxBatch)
	if len(r.worst) > 0 {
		fmt.Printf("        oldest unit at each wake-up, lateness: median %v, 99th %v, max %v\n",
			quantile(r.worst, 0.5).Round(time.Microsecond), quantile(r.worst, 0.99).Round(time.Microsecond),
			quantile(r.worst, 1).Round(time.Microsecond))
	}
}

// maxRate doubles the rate from -bps until a trial fails, then bisects to
// within 5%. A trial passes when it ends no more than -slack behind schedule
// and the 99th percentile of per-wake-up lateness is within -slack.
func maxRate(dur time.Duration, out *sink, where string) {
	fmt.Printf("searching for the highest sustained rate: %d-byte units, %v per trial, pass if within %v of schedule (%s)\n",
		*unitBytes, dur, *slack, where)
	fmt.Printf("%14s  %12s  %12s  %12s  %10s  %s\n", "target Mbps", "units/s", "writer Mbps", "behind (ms)", "late p99", "")
	trial := func(bps uint64) bool {
		r, err := runPacer(bps, dur, out, false)
		if err != nil {
			fail(err)
		}
		p99 := quantile(r.worst, 0.99)
		ok := r.sustained(*slack)
		verdict := "pass"
		if !ok {
			verdict = "FAIL"
		}
		fmt.Printf("%14.3f  %12.0f  %12.3f  %12.3f  %10v  %s\n",
			float64(bps)/1e6, float64(bps)/float64(*unitBytes*8), r.mbps(),
			r.behind.Seconds()*1e3, p99.Round(time.Microsecond), verdict)
		return ok
	}
	lo, hi := uint64(0), uint64(0)
	for b := *bps; b <= searchLimit; b *= 2 {
		if !trial(b) {
			hi = b
			break
		}
		lo = b
	}
	switch {
	case lo == 0:
		fmt.Printf("the starting rate, %.3f Mbps, did not pass\n", float64(*bps)/1e6)
		return
	case hi == 0:
		fmt.Printf("every rate up to %.0f Mbps passed\n", float64(lo)/1e6)
		return
	}
	for float64(hi)/float64(lo) > 1.05 {
		mid := lo + (hi-lo)/2
		if trial(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	fmt.Printf("highest rate sustained: %.1f Mbps (%.0f units/s); %.1f Mbps failed\n",
		float64(lo)/1e6, float64(lo)/float64(*unitBytes*8), float64(hi)/1e6)
}
