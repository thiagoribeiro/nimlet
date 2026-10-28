package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// series buckets everything per wall-clock second, so a run can be lined up with cluster
// events (instance added, killed, ...) recorded by the scripts in unix seconds.
type series struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
}

type bucket struct {
	sendOK, sendErr, retries, shed atomic.Int64
	syncOK, syncErr, reconnects    atomic.Int64
	mu                             sync.Mutex
	delivery                       []time.Duration
}

func newSeries() *series { return &series{buckets: make(map[int64]*bucket)} }

func (s *series) at(t time.Time) *bucket {
	sec := t.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[sec]
	if b == nil {
		b = &bucket{}
		s.buckets[sec] = b
	}
	return b
}

func (b *bucket) addDelivery(d time.Duration) {
	b.mu.Lock()
	b.delivery = append(b.delivery, d)
	b.mu.Unlock()
}

type stepResult struct {
	index    int // lg_step: position among all phases, warm-up included
	num      int // 1-based step number shown in reports
	rate     float64
	duration time.Duration

	acked, failed, retries, shed atomic.Int64

	mu       sync.Mutex
	send     []time.Duration
	delivery []time.Duration

	// Set by evaluate, after the step's settle time.
	achieved  float64
	delivered int64
	expected  int64
	pass      bool
	reasons   []string
}

func (s *stepResult) addSend(d time.Duration) {
	s.mu.Lock()
	s.send = append(s.send, d)
	s.mu.Unlock()
}

func (s *stepResult) addDelivery(d time.Duration) {
	s.mu.Lock()
	s.delivery = append(s.delivery, d)
	s.mu.Unlock()
}

func (s *stepResult) evaluate(r *runState) {
	r.mu.Lock()
	for _, m := range r.msgs {
		if m.step == s.index && m.acked {
			s.expected += int64(r.listenersPerRoom[m.room])
		}
	}
	r.mu.Unlock()
	s.mu.Lock()
	s.delivered = int64(len(s.delivery))
	p99 := quantile(s.delivery, 0.99)
	s.mu.Unlock()

	s.achieved = float64(s.acked.Load()) / s.duration.Seconds()
	if s.achieved < 0.98*s.rate {
		s.reasons = append(s.reasons, fmt.Sprintf("rate %.0f<98%%", s.achieved))
	}
	if n := s.failed.Load(); n > 0 {
		s.reasons = append(s.reasons, fmt.Sprintf("%d failed sends", n))
	}
	if s.delivered < s.expected {
		s.reasons = append(s.reasons, fmt.Sprintf("delivered %.2f%%", pct(s.delivered, s.expected)))
	}
	if p99 > r.cfg.slo {
		s.reasons = append(s.reasons, fmt.Sprintf("delivery p99 %s>slo", p99.Round(time.Millisecond)))
	}
	s.pass = len(s.reasons) == 0
}

func (s *stepResult) print() {
	verdict := "PASS"
	if !s.pass {
		verdict = "FAIL (" + strings.Join(s.reasons, ", ") + ")"
	}
	fmt.Printf("\nstep %d: target=%.0f/s achieved=%.1f/s acked=%d failed=%d retries=%d shed=%d delivered=%d/%d (%.2f%%) %s\n",
		s.num, s.rate, s.achieved, s.acked.Load(), s.failed.Load(), s.retries.Load(), s.shed.Load(),
		s.delivered, s.expected, pct(s.delivered, s.expected), verdict)
	s.mu.Lock()
	printDist("  send", s.send)
	printDist("  delivery", s.delivery)
	s.mu.Unlock()
}

func pct(a, b int64) float64 { return 100 * float64(a) / float64(max(b, 1)) }

// quantile sorts ds in place.
func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[min(int(q*float64(len(ds))), len(ds)-1)]
}

func printDist(name string, ds []time.Duration) {
	if len(ds) == 0 {
		fmt.Printf("%-13s n=0\n", name)
		return
	}
	p := func(q float64) time.Duration { return quantile(ds, q).Round(10 * time.Microsecond) }
	fmt.Printf("%-13s n=%-7d p50=%-10s p95=%-10s p99=%-10s max=%s\n", name, len(ds), p(0.50), p(0.95), p(0.99), ds[len(ds)-1].Round(10*time.Microsecond))
}

// capacity is the highest step that passed (0 when none did).
func capacity(results []*stepResult) float64 {
	var c float64
	for _, s := range results {
		if s.pass {
			c = max(c, s.rate)
		}
	}
	return c
}

func report(cfg config, r *runState, results []*stepResult, rel reliability) {
	fmt.Printf("\nscenario: members=%d rooms=%d listeners=%d senders=%d steps=%v duration=%s\n",
		cfg.members, cfg.rooms, cfg.listeners, cfg.senders, cfg.steps, cfg.duration)
	fmt.Printf("capacity: %.0f msg/s (highest passing step, slo delivery p99 < %s)\n", capacity(results), cfg.slo)
	fmt.Printf("sends: attempted=%d acked=%d failed=%d\n", rel.Attempted, rel.Acked, rel.Failed)
	fmt.Printf("deliveries: expected=%d live=%d late=%d lost=%d (%.4f%%) unacked-delivered=%d\n",
		rel.Expected, rel.Live, rel.Late, rel.Lost, pct(rel.Lost, rel.Expected), rel.UnackedDelivered)
	fmt.Printf("integrity: redelivered=%d double-insert=%d reordered=%d\n", rel.Redelivered, rel.DoubleInsert, rel.Reorders)
	r.mu.Lock()
	fmt.Printf("sync: calls=%d errors=%d reconnects=%d\n", r.syncCalls.Load(), r.syncErrors.Load(), len(r.reconnect))
	printDist("reconnect", r.reconnect)
	printDist("initial sync", r.initialSync)
	r.mu.Unlock()
	if errs := r.errorClasses(); len(errs) > 0 {
		fmt.Printf("errors: %s\n", strings.Join(errs, ", "))
	}
}

type tsLine struct {
	TS            int64   `json:"ts"`
	SendOK        int64   `json:"send_ok"`
	SendErr       int64   `json:"send_err"`
	Retries       int64   `json:"retries"`
	Shed          int64   `json:"shed"`
	Delivered     int     `json:"delivered"`
	DeliveryP50ms float64 `json:"delivery_p50_ms"`
	DeliveryP99ms float64 `json:"delivery_p99_ms"`
	DeliveryMaxms float64 `json:"delivery_max_ms"`
	SyncOK        int64   `json:"sync_ok"`
	SyncErr       int64   `json:"sync_err"`
	Reconnects    int64   `json:"reconnects"`
}

type stepSummary struct {
	Rate        float64  `json:"rate"`
	Achieved    float64  `json:"achieved"`
	Acked       int64    `json:"acked"`
	Failed      int64    `json:"failed"`
	Retries     int64    `json:"retries"`
	Shed        int64    `json:"shed"`
	Delivered   int64    `json:"delivered"`
	Expected    int64    `json:"expected"`
	SendP50ms   float64  `json:"send_p50_ms"`
	SendP99ms   float64  `json:"send_p99_ms"`
	DelivP50ms  float64  `json:"delivery_p50_ms"`
	DelivP99ms  float64  `json:"delivery_p99_ms"`
	Pass        bool     `json:"pass"`
	FailReasons []string `json:"fail_reasons,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func writeOutputs(cfg config, r *runState, results []*stepResult, rel reliability) error {
	secs := make([]int64, 0, len(r.ts.buckets))
	for s := range r.ts.buckets {
		secs = append(secs, s)
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
	f, err := os.Create(cfg.out + ".ts.jsonl")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, sec := range secs {
		b := r.ts.buckets[sec]
		line := tsLine{
			TS: sec, SendOK: b.sendOK.Load(), SendErr: b.sendErr.Load(), Retries: b.retries.Load(), Shed: b.shed.Load(),
			Delivered: len(b.delivery), SyncOK: b.syncOK.Load(), SyncErr: b.syncErr.Load(), Reconnects: b.reconnects.Load(),
		}
		if len(b.delivery) > 0 {
			line.DeliveryP50ms = ms(quantile(b.delivery, 0.50))
			line.DeliveryP99ms = ms(quantile(b.delivery, 0.99))
			line.DeliveryMaxms = ms(b.delivery[len(b.delivery)-1])
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}

	steps := make([]stepSummary, len(results))
	for i, s := range results {
		steps[i] = stepSummary{
			Rate: s.rate, Achieved: s.achieved, Acked: s.acked.Load(), Failed: s.failed.Load(), Retries: s.retries.Load(),
			Shed: s.shed.Load(), Delivered: s.delivered, Expected: s.expected, Pass: s.pass, FailReasons: s.reasons,
			SendP50ms: ms(quantile(s.send, 0.50)), SendP99ms: ms(quantile(s.send, 0.99)),
			DelivP50ms: ms(quantile(s.delivery, 0.50)), DelivP99ms: ms(quantile(s.delivery, 0.99)),
		}
	}
	r.mu.Lock()
	reconnectMax := time.Duration(0)
	if len(r.reconnect) > 0 {
		reconnectMax = slices.Max(r.reconnect)
	}
	summary := map[string]any{
		"run": r.tag, "members": cfg.members, "rooms": cfg.rooms, "listeners": cfg.listeners, "senders": cfg.senders,
		"step_duration_s": cfg.duration.Seconds(), "slo_ms": ms(cfg.slo), "capacity": capacity(results),
		"steps": steps, "reliability": rel, "sync_calls": r.syncCalls.Load(), "sync_errors": r.syncErrors.Load(),
		"reconnects": len(r.reconnect), "reconnect_p99_ms": ms(quantile(r.reconnect, 0.99)), "reconnect_max_ms": ms(reconnectMax),
		"errors": r.errors,
	}
	r.mu.Unlock()
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.out+".summary.json", b, 0o644)
}
