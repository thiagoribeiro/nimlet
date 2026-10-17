package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/thiagoribeiro/nimlet/internal/metrics"
)

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(60, 3) // 1/s, burst 3
	now := time.Unix(1000, 0)
	for i := range 3 {
		if ok, _ := l.allow("k", now); !ok {
			t.Fatalf("request %d within burst rejected", i)
		}
	}
	ok, retry := l.allow("k", now)
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("4th request: ok=%v retry=%v, want rejected with ~1s retry", ok, retry)
	}
	if ok, _ := l.allow("other", now); !ok {
		t.Fatal("keys must be independent")
	}
	if ok, _ := l.allow("k", now.Add(1100*time.Millisecond)); !ok {
		t.Fatal("a token refills after 1s")
	}
	l.allow("k", now.Add(20*time.Minute)) // triggers a sweep of idle buckets
	if _, ok := l.buckets["other"]; ok {
		t.Fatal("idle buckets are swept")
	}
}

func TestLimitByIP(t *testing.T) {
	h := limitByIP(newRateLimiter(60, 2))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	call := func(addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = addr
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	call("10.0.0.1:1111")
	call("10.0.0.1:2222") // same IP, other port: same bucket
	rr := call("10.0.0.1:3333")
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("3rd call from one IP: code %d, Retry-After %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if code := call("10.0.0.2:1111").Code; code != http.StatusOK {
		t.Fatalf("another IP must not be limited, got %d", code)
	}
}

// statusWriter must expose ReadFrom/Unwrap of the real writer (zero-copy downloads,
// per-route deadlines) and still record the status.
func TestStatusWriter_ForwardsReadFrom(t *testing.T) {
	// The real case: a media file. *os.File's WriteTo falls back to the destination's
	// ReadFrom, which is where net/http does sendfile.
	f, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("payload")
	_, _ = f.Seek(0, io.SeekStart)
	defer f.Close()

	rec := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: rec}
	if _, err := io.Copy(sw, f); err != nil {
		t.Fatal(err)
	}
	if !rec.readFromCalled {
		t.Fatal("io.Copy must reach the underlying ReadFrom (sendfile path)")
	}
	if sw.code() != http.StatusOK || rec.Body.String() != "payload" {
		t.Fatalf("status %d body %q", sw.code(), rec.Body.String())
	}
	if http.ResponseWriter(rec) != sw.Unwrap() {
		t.Fatal("Unwrap must return the wrapped writer")
	}
}

type readFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalled bool
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalled = true
	return io.Copy(r.ResponseRecorder, src)
}

// The route pattern, never the raw path, labels metrics: raw paths carry IDs and would
// create unbounded time series.
func TestObserve_LabelsByRoutePattern(t *testing.T) {
	r := chi.NewRouter()
	r.Use(observe)
	r.Put("/rooms/{roomId}/send/{eventType}/{txnId}", func(w http.ResponseWriter, _ *http.Request) {})
	before := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodPut, "/rooms/{roomId}/send/{eventType}/{txnId}", "200"))
	for _, path := range []string{"/rooms/a/send/m.text/1", "/rooms/b/send/m.text/2"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, path, nil))
	}
	after := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodPut, "/rooms/{roomId}/send/{eventType}/{txnId}", "200"))
	if after-before != 2 {
		t.Fatalf("pattern series grew by %v, want 2", after-before)
	}
}
