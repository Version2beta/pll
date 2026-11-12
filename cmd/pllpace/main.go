// Command pllpace runs a real-time pacing loop with a pll.Pacer and reports
// what it achieved: the rate, how far behind schedule it ran, and how late
// each wake-up was. It hands units of a chosen size to an output: discarded by
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
// and sends to it, so the result includes the cost of each send.
package main

import (
	"flag"
	"fmt"
	"io"
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

// sink collects units and writes them in groups of per units.
type sink struct {
	w    io.Writer
	buf  []byte
	per  int
	size int
}

func (s *sink) unit(u []byte) {
	s.buf = append(s.buf, u...)
	if len(s.buf) >= s.per*s.size {
		s.w.Write(s.buf)
		s.buf = s.buf[:0]
	}
}

type result struct {
	sent, wakeups, maxBatch uint64
	elapsed                 time.Duration
	behind                  time.Duration   // how far behind schedule the last wake-up left the output
	worst                   []time.Duration // per wake-up: how late its oldest unit was
}

func (r result) mbps() float64 {
	return float64(r.sent) * float64(*unitBytes) * 8 / r.elapsed.Seconds() / 1e6
}

func quantile(d []time.Duration, f float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(f*float64(len(d)-1))]
}

func runPacer(bps uint64, dur time.Duration, out *sink, progress bool) (result, error) {
	var r result
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
			out.unit(u)
		}
		r.sent += span.Count
		r.maxBatch = max(r.maxBatch, span.Count)
		r.elapsed = now
		if progress && now >= nextReport {
			fmt.Printf("%6.1fs  %10d units  %9.3f Mbps\n", now.Seconds(), r.sent, r.mbps())
			nextReport += time.Second
		}
		_, at, err := p.NextDue()
		if err != nil {
			return r, err
		}
		r.behind = max(0, now-at)
		if d := at - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
	slices.Sort(r.worst)
	return r, nil
}

func runTicker(bps uint64, dur time.Duration, out *sink) result {
	var r result
	ups := float64(bps) / float64(*unitBytes*8)
	u := make([]byte, *unitBytes)
	t := time.NewTicker(time.Duration(float64(time.Second) / ups))
	defer t.Stop()
	start := time.Now()
	nextReport := time.Second
	for range t.C {
		now := time.Since(start)
		if now >= dur {
			break
		}
		r.behind = max(0, now-time.Duration(float64(r.sent)/ups*float64(time.Second)))
		r.wakeups++
		out.unit(u)
		r.sent++
		r.maxBatch = 1
		r.elapsed = now
		if now >= nextReport {
			fmt.Printf("%6.1fs  %10d units  %9.3f Mbps\n", now.Seconds(), r.sent, r.mbps())
			nextReport += time.Second
		}
	}
	return r
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
	if *unitBytes == 0 {
		fail(fmt.Errorf("-unit-bytes must be at least 1"))
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
		out = &sink{w: conn, per: max(1, *perDgm), size: int(*unitBytes)}
		where = fmt.Sprintf("sent to %s, %d per UDP datagram", addr, out.per)
	}
	dur := time.Duration(*secs * float64(time.Second))

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
		r = runTicker(*bps, dur, out)
	default:
		fmt.Fprintln(os.Stderr, "pllpace: -mode must be pacer or ticker")
		os.Exit(2)
	}
	fmt.Printf("result  %d units in %.3fs = %.3f Mbps (target %.3f), %v behind schedule at the end\n",
		r.sent, r.elapsed.Seconds(), r.mbps(), float64(*bps)/1e6, r.behind.Round(time.Microsecond))
	fmt.Printf("        %d wake-ups, %.1f units per wake-up on average, %d at most\n",
		r.wakeups, float64(r.sent)/float64(max(r.wakeups, 1)), r.maxBatch)
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
	fmt.Printf("%14s  %12s  %12s  %12s  %10s  %s\n", "target Mbps", "units/s", "achieved", "behind (ms)", "late p99", "")
	trial := func(bps uint64) bool {
		r, err := runPacer(bps, dur, out, false)
		if err != nil {
			fail(err)
		}
		p99 := quantile(r.worst, 0.99)
		ok := r.behind <= *slack && p99 <= *slack
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
	for b := *bps; b <= 1_000_000_000_000; b *= 2 {
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
