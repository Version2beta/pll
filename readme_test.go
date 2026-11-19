package pll_test

import (
	"bytes"
	"context"
	"go/format"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/version2beta/pll"
)

// This helper is kept identical to README.md; the test below checks the text
// as well as compiling and exercising it.
func pace(ctx context.Context, p *pll.Pacer, out chan<- pll.Span) error {
	start := time.Now()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			span, err := p.Due(time.Since(start), 4096)
			if err != nil {
				return err
			}
			if span.Count > 0 {
				select {
				case out <- span:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

func TestREADMEPaceCancellation(t *testing.T) {
	helper := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		start := strings.Index(s, "func pace(")
		if start < 0 {
			t.Fatalf("%s: missing pace helper", path)
		}
		end := strings.Index(s[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("%s: missing helper end", path)
		}
		b, err = format.Source([]byte(s[start : start+end+3]))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(helper("README.md"), helper("readme_test.go")) {
		t.Fatal("tested pace helper differs from README.md")
	}
	p, err := pll.NewPacer(mustRate(t, 48000, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan pll.Span)
	done := make(chan error, 1)
	go func() { done <- pace(ctx, p, out) }()
	// Drain one batch, then stop consuming while the next send blocks.
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("no first batch")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("pace returned %v", err)
		}
	case <-time.After(time.Second):
		// Release the old, non-cancellable implementation on failure.
		go func() {
			for {
				select {
				case <-out:
				case <-done:
					return
				}
			}
		}()
		t.Fatal("pace did not cancel with a stopped consumer")
	}
}
