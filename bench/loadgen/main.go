// Command loadgen drives a running Nimlet server (or a cluster behind a load balancer) over
// HTTP and reports send latency, end-to-end delivery latency (send → event received by a
// /sync long-poll) and delivery reliability (loss, duplication, reordering, late arrivals).
//
// It registers -members users, splits them round-robin over -rooms public rooms, keeps
// -listeners of them in a /sync long-poll loop and has -senders of them send into their own
// room. The send rate runs through -steps (one or more rates, -duration each); a step passes
// when it reached its rate with no failed sends, delivered everything and kept delivery p99
// under -slo. With >= 1000 members per room the rooms cross the large-room threshold.
//
//	go run ./bench/loadgen -addr http://localhost:8080 -members 200 -listeners 200 -rate 200 -duration 30s
//	go run ./bench/loadgen -addr http://localhost:58080 -members 2000 -rooms 100 -listeners 2000 \
//	    -senders 100 -steps 200,400,800 -duration 30s -out results/run1
//
// Setup (argon2 registrations) is slow on CPU-limited instances, so it can run once against a
// separate instance: -setup-only -state users.json, then -state users.json for the runs.
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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	addr        string
	setupAddr   string
	members     int
	rooms       int
	listeners   int
	senders     int
	steps       []float64
	duration    time.Duration
	warmup      time.Duration
	settle      time.Duration
	setupConc   int
	syncTimeout time.Duration
	prefix      string
	sendRetries int
	maxInflight int
	slo         time.Duration
	stopOnFail  bool
	out         string
	state       string
	setupOnly   bool
}

type user struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// state is what -state saves after setup: everything a later run needs to skip it.
type state struct {
	Prefix string   `json:"prefix"`
	Rooms  int      `json:"rooms"`
	Users  []user   `json:"users"`
	RoomID []string `json:"room_ids"`
}

func main() {
	var cfg config
	var rate float64
	var steps string
	flag.StringVar(&cfg.addr, "addr", "http://localhost:8080", "server base URL")
	flag.StringVar(&cfg.setupAddr, "setup-addr", "", "server used for registrations and joins (default -addr)")
	flag.IntVar(&cfg.members, "members", 200, "users (>=1000 per room makes it a large room)")
	flag.IntVar(&cfg.rooms, "rooms", 1, "rooms; member i joins room i%rooms")
	flag.IntVar(&cfg.listeners, "listeners", 200, "members running /sync long-polls")
	flag.IntVar(&cfg.senders, "senders", 10, "members sending messages (each into its own room)")
	flag.Float64Var(&rate, "rate", 100, "total messages per second (when -steps is empty)")
	flag.StringVar(&steps, "steps", "", "comma-separated rates, one step of -duration each")
	flag.DurationVar(&cfg.duration, "duration", 30*time.Second, "duration of each send step")
	flag.DurationVar(&cfg.warmup, "warmup", 10*time.Second, "send at the first step's rate this long before measuring (cold caches, new connections)")
	flag.DurationVar(&cfg.settle, "settle", 2*time.Second, "wait after each step for in-flight deliveries")
	flag.IntVar(&cfg.setupConc, "setup-concurrency", 4, "parallel registrations (argon2 is memory-heavy)")
	flag.DurationVar(&cfg.syncTimeout, "sync-timeout", 30*time.Second, "/sync long-poll timeout")
	flag.StringVar(&cfg.prefix, "prefix", fmt.Sprintf("lg%d", time.Now().Unix()), "username prefix")
	flag.IntVar(&cfg.sendRetries, "send-retries", 5, "retries per send, same txnId (network errors and 5xx)")
	flag.IntVar(&cfg.maxInflight, "max-inflight", 4000, "sends in flight before new ones are shed")
	flag.DurationVar(&cfg.slo, "slo", 500*time.Millisecond, "delivery p99 a step must stay under to pass")
	flag.BoolVar(&cfg.stopOnFail, "stop-on-fail", true, "skip the remaining steps after one fails")
	flag.StringVar(&cfg.out, "out", "", "write <out>.ts.jsonl (per-second series) and <out>.summary.json")
	flag.StringVar(&cfg.state, "state", "", "load setup from this file, or save it there after setup")
	flag.BoolVar(&cfg.setupOnly, "setup-only", false, "register users, create rooms, save -state and exit")
	flag.Parse()

	cfg.steps = []float64{rate}
	if steps != "" {
		cfg.steps = nil
		for _, s := range strings.Split(steps, ",") {
			r, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil || r <= 0 {
				fmt.Fprintf(os.Stderr, "loadgen: bad step %q\n", s)
				os.Exit(2)
			}
			cfg.steps = append(cfg.steps, r)
		}
	}
	if cfg.setupAddr == "" {
		cfg.setupAddr = cfg.addr
	}
	cfg.rooms = max(1, min(cfg.rooms, cfg.members))
	cfg.listeners = min(cfg.listeners, cfg.members)
	cfg.senders = min(cfg.senders, cfg.members)

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

var client = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        20000,
	MaxIdleConnsPerHost: 20000,
	IdleConnTimeout:     90 * time.Second,
}}

func run(cfg config) error {
	ctx := context.Background()
	start := time.Now()

	st, err := loadOrSetup(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.setupOnly {
		fmt.Printf("setup: %d users in %d rooms in %s, saved to %s\n", len(st.Users), st.Rooms, time.Since(start).Round(time.Millisecond), cfg.state)
		return nil
	}
	users := st.Users
	if len(users) < cfg.members || st.Rooms != cfg.rooms {
		return fmt.Errorf("state %s has %d users in %d rooms, want %d in %d", cfg.state, len(users), st.Rooms, cfg.members, cfg.rooms)
	}
	users = users[:cfg.members]
	fmt.Printf("setup: %d users in %d rooms ready in %s\n", len(users), cfg.rooms, time.Since(start).Round(time.Millisecond))

	// A run tag keeps txnIds unique per device across runs that reuse the same -state.
	run := &runState{cfg: cfg, tag: strconv.FormatInt(time.Now().UnixNano(), 36), ts: newSeries()}
	run.listenersPerRoom = make([]int, cfg.rooms)
	listeners := make([]*listener, cfg.listeners)
	for i := range listeners {
		listeners[i] = &listener{u: users[i], room: st.RoomID[i%cfg.rooms], roomIdx: i % cfg.rooms}
		run.listenersPerRoom[i%cfg.rooms]++
	}

	listenCtx, stopListeners := context.WithCancel(ctx)
	var ready, listenWG sync.WaitGroup
	var initFailed atomic.Int64
	for _, l := range listeners {
		ready.Add(1)
		listenWG.Add(1)
		go func() {
			defer listenWG.Done()
			if err := l.initialSync(listenCtx, run); err != nil {
				initFailed.Add(1)
				ready.Done()
				return
			}
			ready.Done()
			l.loop(listenCtx, run)
		}()
	}
	ready.Wait()
	if n := initFailed.Load(); n > 0 {
		stopListeners()
		listenWG.Wait()
		return fmt.Errorf("%d listeners failed their initial sync", n)
	}
	// Scripts wait for this line to measure the send phase only (e.g. reset pg_stat_statements).
	fmt.Printf("listening: %d listeners caught up in %s\n", cfg.listeners, time.Since(start).Round(time.Millisecond))

	senders := make([]sender, cfg.senders)
	for j := range senders {
		i := cfg.members - cfg.senders + j
		senders[j] = sender{u: users[i], room: st.RoomID[i%cfg.rooms], roomIdx: i % cfg.rooms}
	}

	// Warm-up: its sends count for reliability, not for any step's verdict.
	first := 0
	if cfg.warmup > 0 {
		run.step(ctx, 0, cfg.steps[0], cfg.warmup, senders)
		time.Sleep(cfg.settle)
		fmt.Printf("warmup: %s at %.0f/s\n", cfg.warmup, cfg.steps[0])
		first = 1
	}
	var results []*stepResult
	for k, rate := range cfg.steps {
		res := run.step(ctx, first+k, rate, cfg.duration, senders)
		res.num = k + 1
		time.Sleep(cfg.settle)
		res.evaluate(run)
		results = append(results, res)
		res.print()
		if !res.pass && cfg.stopOnFail {
			break
		}
	}

	stopListeners()
	listenWG.Wait()
	// Anything not seen live is fetched now: it was delivered late, or never (lost).
	run.catchUp(ctx, listeners)
	rel := run.reconcile(listeners)

	report(cfg, run, results, rel)
	if cfg.out != "" {
		if err := writeOutputs(cfg, run, results, rel); err != nil {
			return err
		}
	}
	return nil
}

func loadOrSetup(ctx context.Context, cfg config) (*state, error) {
	if cfg.state != "" && !cfg.setupOnly {
		b, err := os.ReadFile(cfg.state)
		if err == nil {
			var st state
			if err := json.Unmarshal(b, &st); err != nil {
				return nil, fmt.Errorf("state %s: %w", cfg.state, err)
			}
			return &st, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	users, err := setupUsers(ctx, cfg)
	if err != nil {
		return nil, err
	}
	roomIDs, err := setupRooms(ctx, cfg, users)
	if err != nil {
		return nil, err
	}
	st := &state{Prefix: cfg.prefix, Rooms: cfg.rooms, Users: users, RoomID: roomIDs}
	if cfg.state != "" {
		b, _ := json.Marshal(st)
		if err := os.WriteFile(cfg.state, b, 0o600); err != nil {
			return nil, err
		}
	}
	return st, nil
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
			err := call(ctx, http.MethodPost, cfg.setupAddr+"/register", "", map[string]string{
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
			users[i] = user{ID: resp.UserID, Token: resp.Token}
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

// setupRooms creates room r with member r (the first member assigned to it) and joins every
// other member i into room i%rooms.
func setupRooms(ctx context.Context, cfg config, users []user) ([]string, error) {
	ids := make([]string, cfg.rooms)
	for r := range ids {
		var room struct {
			RoomID string `json:"room_id"`
		}
		if err := call(ctx, http.MethodPost, cfg.setupAddr+"/rooms", users[r].Token,
			map[string]string{"name": fmt.Sprintf("%s-%d", cfg.prefix, r), "visibility": "public"}, &room); err != nil {
			return nil, fmt.Errorf("create room %d: %w", r, err)
		}
		ids[r] = room.RoomID
	}
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := cfg.rooms; i < len(users); i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := call(ctx, http.MethodPost, cfg.setupAddr+"/rooms/"+ids[i%cfg.rooms]+"/join", users[i].Token, struct{}{}, nil); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failed.Load(); n > 0 {
		return nil, fmt.Errorf("%d joins failed", n)
	}
	return ids, nil
}

type sender struct {
	u       user
	room    string
	roomIdx int
}

// step sends at rate msg/s for d. Sends are open-loop: a slow server doesn't slow
// the schedule down; past -max-inflight new sends are shed, which shows as a missed rate.
func (r *runState) step(ctx context.Context, k int, rate float64, d time.Duration, senders []sender) *stepResult {
	res := &stepResult{index: k, rate: rate, duration: d}
	r.mu.Lock()
	r.stepsRun = append(r.stepsRun, res)
	r.mu.Unlock()

	inflight := make(chan struct{}, r.cfg.maxInflight)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var wg sync.WaitGroup
	t0 := time.Now()
	var launched int64
	for now := range ticker.C {
		elapsed := now.Sub(t0)
		if elapsed >= d {
			break
		}
		for due := int64(elapsed.Seconds() * rate); launched < due; launched++ {
			select {
			case inflight <- struct{}{}:
			default:
				res.shed.Add(1)
				r.ts.at(time.Now()).shed.Add(1)
				continue
			}
			n := r.seq.Add(1)
			s := senders[n%int64(len(senders))]
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-inflight }()
				r.send(ctx, res, s, n)
			}()
		}
	}
	wg.Wait()
	return res
}

// send delivers one message, retrying network errors and 5xx with the same txnId: the server
// must turn every retry of a send that already landed into the original event.
func (r *runState) send(ctx context.Context, res *stepResult, s sender, n int64) {
	t0 := time.Now()
	txn := fmt.Sprintf("%s-%d", r.tag, n)
	body := map[string]any{"body": "loadgen", "lg_sent_ns": t0.UnixNano(), "lg_run": r.tag, "lg_n": n, "lg_step": res.index}
	r.attempt(n, s.roomIdx, res.index)
	backoff := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := call(ctx, http.MethodPut, r.cfg.addr+"/rooms/"+s.room+"/send/m.room.message/"+txn, s.u.Token, body, nil)
		if err == nil {
			d := time.Since(t0)
			r.ack(n)
			res.acked.Add(1)
			res.addSend(d)
			r.ts.at(time.Now()).sendOK.Add(1)
			return
		}
		if attempt >= r.cfg.sendRetries || !retryable(err) {
			res.failed.Add(1)
			r.ts.at(time.Now()).sendErr.Add(1)
			r.noteError(err)
			return
		}
		res.retries.Add(1)
		r.ts.at(time.Now()).retries.Add(1)
		time.Sleep(backoff)
		backoff = min(2*backoff, 2*time.Second)
	}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return fmt.Sprintf("%d %s", e.status, e.msg) }

// retryable: anything that never got an HTTP answer, and 5xx (including the LB's 502/503).
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status >= 500
	}
	return true
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
		return &httpError{status: resp.StatusCode, msg: method + " " + url + ": " + string(msg)}
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
