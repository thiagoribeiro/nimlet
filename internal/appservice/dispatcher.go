package appservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/wire"
)

const (
	workerBufSize   = 1000 // buffered channel capacity per AS
	batchWindow     = 5 * time.Millisecond
	maxBatchSize    = 100
	deliveryTimeout = 10 * time.Second

	// Retries while an AppService is down or overloaded: 200ms, 400ms … up to 5s, 6 tries
	// (~11s). The worker blocks meanwhile, so a long outage fills its buffer and later events
	// are dropped (logged) — delivery stays best-effort, but a blip no longer loses a batch.
	maxDeliveryAttempts = 6
	retryBackoff        = 200 * time.Millisecond
	maxRetryBackoff     = 5 * time.Second
)

// Dispatcher delivers events to registered AppServices asynchronously.
// Each AppService gets its own goroutine and buffered channel. Events are
// batched within a 5ms window (or up to 100 events) before delivery.
type Dispatcher struct {
	registry *Registry
	workers  map[string]*worker
	once     sync.Once
}

// NewDispatcher creates a Dispatcher. Call Start() to launch worker goroutines.
func NewDispatcher(reg *Registry) *Dispatcher {
	d := &Dispatcher{
		registry: reg,
		workers:  make(map[string]*worker, len(reg.All())),
	}
	for _, as := range reg.All() {
		d.workers[as.ID] = &worker{
			as:     as,
			ch:     make(chan *domain.Event, workerBufSize),
			client: &http.Client{Timeout: deliveryTimeout},
		}
	}
	return d
}

// Start launches one goroutine per AppService. It is safe to call only once.
func (d *Dispatcher) Start(ctx context.Context) {
	d.once.Do(func() {
		for _, w := range d.workers {
			go w.run(ctx)
		}
	})
}

// Dispatch routes the event to all interested AppServices of the event's namespace.
// Non-blocking: if a worker's channel is full, the event is dropped and a warning is logged.
func (d *Dispatcher) Dispatch(event *domain.Event) {
	for _, w := range d.workers {
		// Namespace first: an AppService never sees another namespace's events, whatever
		// its interested_events say. An event without a namespace goes nowhere.
		if event.NamespaceID == "" || event.NamespaceID != w.as.NamespaceID {
			continue
		}
		if !w.as.IsInterestedIn(event.EventType) {
			continue
		}
		select {
		case w.ch <- event:
		default:
			slog.Warn("appservice: worker channel full, dropping event",
				"as_id", w.as.ID,
				"event_id", event.ID,
				"event_type", event.EventType,
			)
		}
	}
}

// worker batches events for one AppService and delivers them via HTTP.
type worker struct {
	as     *domain.AppService
	ch     chan *domain.Event
	client *http.Client
}

func (w *worker) run(ctx context.Context) {
	var batch []*domain.Event
	timer := time.NewTimer(batchWindow)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.deliver(ctx, batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return

		case ev, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= maxBatchSize {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				flush()
				timer.Reset(batchWindow)
			}

		case <-timer.C:
			flush()
			timer.Reset(batchWindow)
		}
	}
}

// deliveryPayload is what Nimlet POSTs to an AppService.
type deliveryPayload struct {
	TransactionID string       `json:"transaction_id"`
	Events        []wire.Event `json:"events"`
}

func (w *worker) deliver(ctx context.Context, batch []*domain.Event) {
	// Deterministic per batch (an AS can dedupe a re-delivery) without exposing global
	// stream positions: the first event's random ID plus the batch size.
	txnID := fmt.Sprintf("nimlet:%s:%s:%d", w.as.ID, batch[0].ID, len(batch))

	payload := deliveryPayload{TransactionID: txnID, Events: wire.FromEvents(batch)}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("appservice: marshal payload", "as_id", w.as.ID, "error", err)
		return
	}

	for attempt := 1; ; attempt++ {
		status, err := w.post(ctx, body)
		if err == nil && status >= 200 && status < 300 {
			return
		}
		// Network errors, 5xx and 429 are worth retrying; any other answer is final.
		retryable := err != nil || status >= 500 || status == http.StatusTooManyRequests
		if !retryable || attempt == maxDeliveryAttempts {
			slog.Warn("appservice: delivery failed, dropping batch",
				"as_id", w.as.ID, "txn_id", txnID, "attempts", attempt, "status", status, "error", err)
			return
		}
		backoff := min(retryBackoff<<(attempt-1), maxRetryBackoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}

// post sends one delivery attempt; the transaction ID inside body is the same on every
// attempt, so the AppService can recognise a re-delivery.
func (w *worker) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.as.Endpoint+"/events", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.as.HSToken)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused
	resp.Body.Close()
	return resp.StatusCode, nil
}

// NopDispatcher is a no-op implementation used when no AppServices are configured.
type NopDispatcher struct{}

func (NopDispatcher) Dispatch(_ *domain.Event) {}
