package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runState is everything one run shares between senders, listeners and the report.
type runState struct {
	cfg              config
	tag              string // lg_run: tells this run's events from older ones in the same rooms
	ts               *series
	seq              atomic.Int64
	listenersPerRoom []int

	syncCalls  atomic.Int64
	syncErrors atomic.Int64

	mu          sync.Mutex
	stepsRun    []*stepResult
	msgs        map[int64]*msgInfo // lg_n → what was sent
	errors      map[string]int     // final send / sync error classes
	initialSync []time.Duration
	reconnect   []time.Duration
}

type msgInfo struct {
	room, step int
	acked      bool
}

func (r *runState) attempt(n int64, room, step int) {
	r.mu.Lock()
	if r.msgs == nil {
		r.msgs = make(map[int64]*msgInfo)
	}
	r.msgs[n] = &msgInfo{room: room, step: step}
	r.mu.Unlock()
}

func (r *runState) ack(n int64) {
	r.mu.Lock()
	r.msgs[n].acked = true
	r.mu.Unlock()
}

// noteError counts errors by a short class ("send 503", "sync connection refused", ...).
func (r *runState) noteError(err error) { r.noteErrorAs("send", err) }

func (r *runState) noteErrorAs(op string, err error) {
	class := err.Error()
	if he, ok := err.(*httpError); ok {
		class = fmt.Sprint(he.status)
	} else {
		for _, s := range []string{"connection refused", "connection reset", "EOF", "timeout", "broken pipe"} {
			if strings.Contains(class, s) {
				class = s
				break
			}
		}
		if len(class) > 60 {
			class = class[:60]
		}
	}
	r.mu.Lock()
	if r.errors == nil {
		r.errors = make(map[string]int)
	}
	r.errors[op+" "+class]++
	r.mu.Unlock()
}

func (r *runState) stepAt(k int) *stepResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stepsRun[k]
}

type syncResponse struct {
	NextBatch string `json:"next_batch"` // opaque cursor
	Events    []struct {
		ID      string `json:"event_id"`
		RoomID  string `json:"room_id"`
		RoomSeq int64  `json:"room_seq"`
		Content struct {
			SentNs int64  `json:"lg_sent_ns"`
			Run    string `json:"lg_run"`
			N      int64  `json:"lg_n"`
			Step   int    `json:"lg_step"`
		} `json:"content"`
	} `json:"events"`
}

// listener is one user long-polling /sync. Only its own goroutine touches it until the
// catch-up, which runs after that goroutine has returned.
type listener struct {
	u       user
	room    string
	roomIdx int
	since   string

	got          map[int64]string // lg_n → event_id of the first copy received
	late         int64            // received only in the final catch-up
	lastSeq      int64            // highest room_seq seen in the room
	reorders     int64            // room_seq lower than one already received
	redelivered  int64            // same event received twice (cursor went backwards)
	doubleInsert int64            // same txn stored as two events (idempotency failed)
	errStart     time.Time
}

func (l *listener) syncURL(addr string, timeout time.Duration) string {
	return fmt.Sprintf("%s/sync?since=%s&timeout=%d", addr, url.QueryEscape(l.since), max(timeout.Milliseconds(), 1))
}

// initialSync pages through history until caught up (the "app open" cost).
// timeout=1ms because the server treats timeout<=0 as its 30s default.
func (l *listener) initialSync(ctx context.Context, r *runState) error {
	l.got = make(map[int64]string)
	t0 := time.Now()
	var err error
	for tries := 0; tries < 20; {
		var resp syncResponse
		if err = call(ctx, http.MethodGet, l.syncURL(r.cfg.addr, time.Millisecond), l.u.Token, nil, &resp); err != nil {
			tries++
			time.Sleep(250 * time.Millisecond)
			continue
		}
		l.handle(r, &resp, true)
		l.since = resp.NextBatch
		if len(resp.Events) == 0 {
			r.mu.Lock()
			r.initialSync = append(r.initialSync, time.Since(t0))
			r.mu.Unlock()
			return nil
		}
	}
	return err
}

func (l *listener) loop(ctx context.Context, r *runState) {
	for ctx.Err() == nil {
		var resp syncResponse
		err := call(ctx, http.MethodGet, l.syncURL(r.cfg.addr, r.cfg.syncTimeout), l.u.Token, nil, &resp)
		now := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A cut long-poll (instance gone, LB 503, ...): the cursor didn't move, so
			// retrying from it must lose nothing.
			r.syncErrors.Add(1)
			r.ts.at(now).syncErr.Add(1)
			r.noteErrorAs("sync", err)
			if l.errStart.IsZero() {
				l.errStart = now
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !l.errStart.IsZero() {
			d := now.Sub(l.errStart)
			l.errStart = time.Time{}
			r.ts.at(now).reconnects.Add(1)
			r.mu.Lock()
			r.reconnect = append(r.reconnect, d)
			r.mu.Unlock()
		}
		r.syncCalls.Add(1)
		r.ts.at(now).syncOK.Add(1)
		l.handle(r, &resp, true)
		l.since = resp.NextBatch
	}
}

func (l *listener) handle(r *runState, resp *syncResponse, live bool) {
	now := time.Now()
	for _, e := range resp.Events {
		if e.RoomID != l.room {
			continue
		}
		if e.Content.Run != r.tag {
			l.lastSeq = max(l.lastSeq, e.RoomSeq)
			continue
		}
		if id, dup := l.got[e.Content.N]; dup {
			if id == e.ID {
				l.redelivered++
			} else {
				l.doubleInsert++
			}
			continue
		}
		if e.RoomSeq <= l.lastSeq {
			l.reorders++
		} else {
			l.lastSeq = e.RoomSeq
		}
		l.got[e.Content.N] = e.ID
		if !live {
			l.late++
			continue
		}
		d := time.Duration(now.UnixNano() - e.Content.SentNs)
		r.stepAt(e.Content.Step).addDelivery(d)
		r.ts.at(now).addDelivery(d)
	}
}

// catchUp drains what each listener didn't get live, retrying through errors for up to 30s.
func (r *runState) catchUp(ctx context.Context, ls []*listener) {
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				var resp syncResponse
				if err := call(ctx, http.MethodGet, l.syncURL(r.cfg.addr, time.Millisecond), l.u.Token, nil, &resp); err != nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				l.handle(r, &resp, false)
				l.since = resp.NextBatch
				if len(resp.Events) == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
}

type reliability struct {
	Attempted        int64 `json:"attempted"`
	Acked            int64 `json:"acked"`
	Failed           int64 `json:"failed"` // gave up after -send-retries
	Expected         int64 `json:"expected"`
	Live             int64 `json:"delivered_live"`
	Late             int64 `json:"delivered_late"`
	Lost             int64 `json:"lost"`
	UnackedDelivered int64 `json:"unacked_delivered"` // failed send that was stored anyway
	Redelivered      int64 `json:"redelivered"`
	DoubleInsert     int64 `json:"double_insert"`
	Reorders         int64 `json:"reorders"`
}

// reconcile compares what every listener received against what was acked in its room.
func (r *runState) reconcile(ls []*listener) reliability {
	var rel reliability
	byRoom := make([][]int64, r.cfg.rooms)
	r.mu.Lock()
	for n, m := range r.msgs {
		rel.Attempted++
		if m.acked {
			rel.Acked++
		}
		byRoom[m.room] = append(byRoom[m.room], n)
	}
	r.mu.Unlock()
	for _, s := range r.stepsRun {
		rel.Failed += s.failed.Load()
	}
	for _, l := range ls {
		for _, n := range byRoom[l.roomIdx] {
			_, got := l.got[n]
			switch acked := r.msgs[n].acked; {
			case acked && got:
				rel.Expected++
			case acked:
				rel.Expected++
				rel.Lost++
			case got:
				rel.UnackedDelivered++
			}
		}
		rel.Late += l.late
		rel.Live += int64(len(l.got)) - l.late
		rel.Redelivered += l.redelivered
		rel.DoubleInsert += l.doubleInsert
		rel.Reorders += l.reorders
	}
	return rel
}

func (r *runState) errorClasses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.errors))
	for k, v := range r.errors {
		out = append(out, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(out)
	return out
}
