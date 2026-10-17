package appservice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// An AppService receives only events of its own namespace, even when it is interested in
// every event type.
func TestDispatcher_FiltersByNamespace(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p deliveryPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		for _, e := range p.Events {
			got = append(got, e.ID)
		}
		mu.Unlock()
	}))
	defer srv.Close()

	reg, err := New(&domain.AppService{ID: "bot", Endpoint: srv.URL, ASToken: "t", NamespaceID: "ns-a", InterestedEvents: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDispatcher(reg)
	d.Start(ctx)

	d.Dispatch(&domain.Event{ID: "other-ns", NamespaceID: "ns-b", EventType: "m.room.message"})
	d.Dispatch(&domain.Event{ID: "no-ns", EventType: "m.room.message"})
	d.Dispatch(&domain.Event{ID: "own-ns", NamespaceID: "ns-a", EventType: "m.room.message"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let any wrongly-routed batch arrive too
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "own-ns" {
		t.Fatalf("delivered %v, want only [own-ns]", got)
	}
}

// A transient failure is retried with the same transaction ID; a definitive 4xx is not.
func TestDispatcher_RetriesTransientFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		statuses []int
		wantHits int
	}{
		"503 then ok":  {[]int{503, 503, 200}, 3},
		"400 is final": {[]int{400, 200}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var txnIDs []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p deliveryPayload
				_ = json.NewDecoder(r.Body).Decode(&p)
				mu.Lock()
				i := len(txnIDs)
				txnIDs = append(txnIDs, p.TransactionID)
				mu.Unlock()
				w.WriteHeader(tc.statuses[min(i, len(tc.statuses)-1)])
			}))
			defer srv.Close()

			reg, err := New(&domain.AppService{ID: "bot", Endpoint: srv.URL, ASToken: "t", NamespaceID: "ns", InterestedEvents: []string{"*"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := NewDispatcher(reg)
			d.Start(ctx)
			d.Dispatch(&domain.Event{ID: "e1", NamespaceID: "ns", EventType: "m.text"})

			time.Sleep(2 * time.Second) // backoff 200ms + 400ms for the 503 case
			mu.Lock()
			defer mu.Unlock()
			if len(txnIDs) != tc.wantHits {
				t.Fatalf("attempts = %d, want %d", len(txnIDs), tc.wantHits)
			}
			for _, id := range txnIDs {
				if id != txnIDs[0] {
					t.Fatalf("retries must reuse the transaction ID: %v", txnIDs)
				}
			}
		})
	}
}
