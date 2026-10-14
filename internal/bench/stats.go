package bench

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

type stats struct {
	mu        sync.Mutex
	start     time.Time
	connected int
	peak      int
	failures  int
	lastErr   string

	// This interval, and the whole run.
	objects, bytes           int64
	totalObjects, totalBytes int64
	resets, ends, subFails   int64
	latencies, allLatencies  []time.Duration
}

// maxSamples bounds the latencies kept for the run's percentiles.
const maxSamples = 1 << 20

func newStats() *stats { return &stats{start: time.Now()} }

func (s *stats) connect(d int) {
	s.mu.Lock()
	s.connected += d
	s.peak = max(s.peak, s.connected)
	s.mu.Unlock()
}

func (s *stats) failed(err error) {
	s.mu.Lock()
	s.failures++
	s.lastErr = err.Error()
	s.mu.Unlock()
}

func (s *stats) object(n int, latency time.Duration) {
	s.mu.Lock()
	s.objects++
	s.bytes += int64(n)
	if latency >= 0 {
		s.latencies = append(s.latencies, latency)
		if len(s.allLatencies) < maxSamples {
			s.allLatencies = append(s.allLatencies, latency)
		}
	}
	s.mu.Unlock()
}

func (s *stats) reset() {
	s.mu.Lock()
	s.resets++
	s.mu.Unlock()
}

func (s *stats) ended() {
	s.mu.Lock()
	s.ends++
	s.mu.Unlock()
}

func (s *stats) subscribeFailed() {
	s.mu.Lock()
	s.subFails++
	s.mu.Unlock()
}

func percentiles(ls []time.Duration) (p50, p95, top time.Duration) {
	if len(ls) == 0 {
		return 0, 0, 0
	}
	ls = slices.Clone(ls)
	slices.Sort(ls)
	at := func(q float64) time.Duration { return ls[min(len(ls)-1, int(q*float64(len(ls))))] }
	return at(0.5), at(0.95), ls[len(ls)-1]
}

func (b *bench) report(ctx context.Context, out io.Writer) {
	t := time.NewTicker(b.cfg.Every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s := b.stats
			s.mu.Lock()
			secs := now.Sub(last).Seconds()
			p50, p95, top := percentiles(s.latencies)
			fmt.Fprintf(out, "%5.0fs  viewers %d/%d  %.0f objects/s  %.1f Mbit/s  latency p50 %v p95 %v max %v  resets %d  ended %d  subscribe failures %d  failed %d\n",
				now.Sub(s.start).Seconds(), s.connected, b.cfg.Viewers, float64(s.objects)/secs, float64(s.bytes)*8/secs/1e6,
				p50.Round(time.Millisecond), p95.Round(time.Millisecond), top.Round(time.Millisecond), s.resets, s.ends, s.subFails, s.failures)
			s.totalObjects += s.objects
			s.totalBytes += s.bytes
			s.objects, s.bytes, s.latencies = 0, 0, s.latencies[:0]
			s.mu.Unlock()
			last = now
		}
	}
}

// Totals summarises a run.
type Totals struct {
	PeakViewers, Failures              int
	LastError                          string
	Objects, Bytes                     int64
	Resets, Ended, SubscribeFailures   int64
	LatencyP50, LatencyP95, LatencyMax time.Duration
	Duration                           time.Duration
}

func (s *stats) totals(d time.Duration) Totals {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Totals{
		PeakViewers: s.peak, Failures: s.failures, LastError: s.lastErr,
		Objects: s.totalObjects + s.objects, Bytes: s.totalBytes + s.bytes,
		Resets: s.resets, Ended: s.ends, SubscribeFailures: s.subFails, Duration: d,
	}
	t.LatencyP50, t.LatencyP95, t.LatencyMax = percentiles(s.allLatencies)
	return t
}

func (t Totals) String() string {
	s := fmt.Sprintf("total: peak viewers %d, %d objects, %.1f MB (%.1f Mbit/s), latency p50 %v p95 %v max %v, resets %d, ended %d, subscribe failures %d, failed %d",
		t.PeakViewers, t.Objects, float64(t.Bytes)/1e6, float64(t.Bytes)*8/t.Duration.Seconds()/1e6,
		t.LatencyP50.Round(time.Millisecond), t.LatencyP95.Round(time.Millisecond), t.LatencyMax.Round(time.Millisecond),
		t.Resets, t.Ended, t.SubscribeFailures, t.Failures)
	if t.LastError != "" {
		s += " (last error: " + t.LastError + ")"
	}
	return s
}
