package metrics

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveHTTP(t *testing.T) {
	before := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/x/{id}", "200"))
	ObserveHTTP(http.MethodGet, "/x/{id}", 200, time.Millisecond)
	ObserveHTTP(http.MethodGet, "/x/{id}", 200, time.Millisecond)
	if got := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/x/{id}", "200")) - before; got != 2 {
		t.Fatalf("count = %v, want 2", got)
	}
}
