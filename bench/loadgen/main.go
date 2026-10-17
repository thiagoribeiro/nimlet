// Command loadgen drives a running Nimlet server over HTTP and reports send latency and
// end-to-end delivery latency (send → event received by a /sync long-poll).
//
// It registers -members users, puts them all in one public room, keeps -listeners of them
// in a /sync long-poll loop, and has -senders of them send -rate messages per second for
// -duration. With -members >= 1000 the room crosses the large-room threshold, exercising
// the room-key wake-up path instead of per-user fan-out.
//
//	go run ./bench/loadgen -addr http://localhost:8080 -members 200 -listeners 200 -rate 200 -duration 30s
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	addr        string
	members     int
	listeners   int
	senders     int
	rate        float64
	duration    time.Duration
	setupConc   int
	syncTimeout time.Duration
	prefix      string
}

type user struct {
	id    string
	token string
}

type stats struct {
	mu          sync.Mutex
	send        []time.Duration
	delivery    []time.Duration
	initialSync []time.Duration
	sendErrors  atomic.Int64
	syncErrors  atomic.Int64
	syncCalls   atomic.Int64
	duplicates  atomic.Int64
}

func (s *stats) add(dst *[]time.Duration, d time.Duration) {
	s.mu.Lock()
	*dst = append(*dst, d)
	s.mu.Unlock()
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", "http://localhost:8080", "server base URL")
	flag.IntVar(&cfg.members, "members", 200, "room members (>=1000 makes it a large room)")
	flag.IntVar(&cfg.listeners, "listeners", 200, "members running /sync long-polls")
	flag.IntVar(&cfg.senders, "senders", 10, "members sending messages")
	flag.Float64Var(&cfg.rate, "rate", 100, "total messages per second")
	flag.DurationVar(&cfg.duration, "duration", 30*time.Second, "send phase duration")
	flag.IntVar(&cfg.setupConc, "setup-concurrency", 4, "parallel registrations (argon2 is memory-heavy)")
	flag.DurationVar(&cfg.syncTimeout, "sync-timeout", 30*time.Second, "/sync long-poll timeout")
	flag.StringVar(&cfg.prefix, "prefix", fmt.Sprintf("lg%d", time.Now().Unix()), "username prefix")
	flag.Parse()

	cfg.listeners = min(cfg.listeners, cfg.members)
	cfg.senders = min(cfg.senders, cfg.members)

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

var client = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        10000,
	MaxIdleConnsPerHost: 10000,
	IdleConnTimeout:     90 * time.Second,
}}

func run(cfg config) error {
	ctx := context.Background()
	start := time.Now()

	users, err := setupUsers(ctx, cfg)
	if err != nil {
		return err
	}
	roomID, err := setupRoom(ctx, cfg, users)
	if err != nil {
		return err
	}
	fmt.Printf("setup: %d users joined room %s in %s\n", len(users), roomID, time.Since(start).Round(time.Millisecond))

	st := &stats{}
	listenCtx, stopListeners := context.WithCancel(ctx)
	var ready, listenWG sync.WaitGroup
	for _, u := range users[:cfg.listeners] {
		ready.Add(1)
		listenWG.Add(1)
		go func() {
			defer listenWG.Done()
			listen(listenCtx, cfg, u, st, &ready)
		}()
	}
	ready.Wait()

	sendStart := time.Now()
	sent := sendLoop(ctx, cfg, users[len(users)-cfg.senders:], roomID, st)
	sendElapsed := time.Since(sendStart)

	// Let in-flight deliveries land before tearing down the long-polls.
	time.Sleep(2 * time.Second)
	stopListeners()
	listenWG.Wait()

	report(cfg, st, sent, sendElapsed)
	return nil
}

func setupUsers(ctx context.Context, cfg config) ([]user, error) {
	users := make([]user, cfg.members)
	sem := make(chan struct{}, cfg.setupConc)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := range users {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var resp struct {
				UserID string `json:"user_id"`
				Token  string `json:"access_token"`
			}
			err := call(ctx, http.MethodPost, cfg.addr+"/register", "", map[string]string{
				"username":    fmt.Sprintf("%s_%d", cfg.prefix, i),
				"password":    "loadgen-password",
				"device_name": "loadgen",
			}, &resp)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("register %d: %w", i, err):
				default:
				}
				return
			}
			users[i] = user{id: resp.UserID, token: resp.Token}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return nil, err
	default:
		return users, nil
	}
}

func setupRoom(ctx context.Context, cfg config, users []user) (string, error) {
	var room struct {
		RoomID string `json:"room_id"`
	}
	if err := call(ctx, http.MethodPost, cfg.addr+"/rooms", users[0].token,
		map[string]string{"name": cfg.prefix, "visibility": "public"}, &room); err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for _, u := range users[1:] {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := call(ctx, http.MethodPost, cfg.addr+"/rooms/"+room.RoomID+"/join", u.token, struct{}{}, nil); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failed.Load(); n > 0 {
		return "", fmt.Errorf("%d joins failed", n)
	}
	return room.RoomID, nil
}

type syncResponse struct {
	NextBatch string `json:"next_batch"` // opaque cursor
	Events    []struct {
		ID      string `json:"event_id"`
		Content struct {
			SentNs int64 `json:"lg_sent_ns"`
		} `json:"content"`
	} `json:"events"`
}

func listen(ctx context.Context, cfg config, u user, st *stats, ready *sync.WaitGroup) {
	// Initial sync: page through history until caught up (the "app open" cost).
	// timeout=1ms because the server treats timeout<=0 as its 30s default.
	var since string
	t0 := time.Now()
	for {
		var resp syncResponse
		if err := call(ctx, http.MethodGet, fmt.Sprintf("%s/sync?since=%s&timeout=1", cfg.addr, url.QueryEscape(since)), u.token, nil, &resp); err != nil {
			st.syncErrors.Add(1)
			ready.Done()
			return
		}
		since = resp.NextBatch
		if len(resp.Events) == 0 {
			break
		}
	}
	st.add(&st.initialSync, time.Since(t0))
	ready.Done()

	seen := make(map[string]struct{})
	syncURL := func() string {
		return fmt.Sprintf("%s/sync?since=%s&timeout=%d", cfg.addr, url.QueryEscape(since), cfg.syncTimeout.Milliseconds())
	}
	for ctx.Err() == nil {
		var resp syncResponse
		err := call(ctx, http.MethodGet, syncURL(), u.token, nil, &resp)
		if err != nil {
			if ctx.Err() == nil {
				st.syncErrors.Add(1)
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		st.syncCalls.Add(1)
		now := time.Now().UnixNano()
		for _, e := range resp.Events {
			if e.Content.SentNs == 0 {
				continue
			}
			if _, dup := seen[e.ID]; dup {
				st.duplicates.Add(1)
				continue
			}
			seen[e.ID] = struct{}{}
			st.add(&st.delivery, time.Duration(now-e.Content.SentNs))
		}
		since = resp.NextBatch
	}
}

func sendLoop(ctx context.Context, cfg config, senders []user, roomID string, st *stats) int64 {
	interval := time.Duration(float64(time.Second) / cfg.rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.After(cfg.duration)
	var sent atomic.Int64
	var wg sync.WaitGroup
	var n int
	for {
		select {
		case <-deadline:
			wg.Wait()
			return sent.Load()
		case <-ticker.C:
			u := senders[n%len(senders)]
			n++
			txn := fmt.Sprintf("%s-%d", cfg.prefix, n)
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := time.Now()
				body := map[string]any{"body": "loadgen", "lg_sent_ns": t0.UnixNano()}
				err := call(ctx, http.MethodPut, cfg.addr+"/rooms/"+roomID+"/send/m.room.message/"+txn, u.token, body, nil)
				if err != nil {
					st.sendErrors.Add(1)
					return
				}
				sent.Add(1)
				st.add(&st.send, time.Since(t0))
			}()
		}
	}
}

func call(ctx context.Context, method, url, token string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, msg)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func report(cfg config, st *stats, sent int64, elapsed time.Duration) {
	fmt.Printf("\nscenario: members=%d listeners=%d senders=%d rate=%.0f/s duration=%s\n",
		cfg.members, cfg.listeners, cfg.senders, cfg.rate, cfg.duration)
	fmt.Printf("sent: %d (%.1f/s), send errors: %d, sync errors: %d, sync calls: %d\n",
		sent, float64(sent)/elapsed.Seconds(), st.sendErrors.Load(), st.syncErrors.Load(), st.syncCalls.Load())
	expected := sent * int64(cfg.listeners)
	fmt.Printf("delivered: %d / %d expected (%.2f%%), duplicates: %d\n", len(st.delivery), expected,
		100*float64(len(st.delivery))/float64(max(expected, 1)), st.duplicates.Load())
	printDist("initial sync", st.initialSync)
	printDist("send", st.send)
	printDist("delivery", st.delivery)
}

func printDist(name string, ds []time.Duration) {
	if len(ds) == 0 {
		fmt.Printf("%-13s n=0\n", name)
		return
	}
	slices.Sort(ds)
	p := func(q float64) time.Duration {
		return ds[min(int(q*float64(len(ds))), len(ds)-1)].Round(10 * time.Microsecond)
	}
	fmt.Printf("%-13s n=%-7d p50=%-10s p95=%-10s p99=%-10s max=%s\n", name, len(ds), p(0.50), p(0.95), p(0.99), ds[len(ds)-1].Round(10*time.Microsecond))
}
