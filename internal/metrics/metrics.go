package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimlet_http_requests_total",
		Help: "Total HTTP requests by method, path pattern, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nimlet_http_request_duration_seconds",
		Help:    "HTTP request latency by method and path pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	ActiveSyncConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "nimlet_sync_active_connections",
		Help: "Number of active /sync long-poll connections.",
	})

	EventsSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nimlet_events_sent_total",
		Help: "Total events sent.",
	})

	RoomsCreatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nimlet_rooms_created_total",
		Help: "Total rooms created.",
	})

	UsersRegisteredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nimlet_users_registered_total",
		Help: "Total user registrations.",
	})
)

// RegisterBusDropped exposes the event bus's dropped wake-up counter. Drops are not data
// loss (clients re-sync on long-poll timeout) but show up as delivery latency.
func RegisterBusDropped(dropped func() int64) {
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "nimlet_bus_dropped_notifications_total",
		Help: "Wake-up notifications dropped because a partition subscriber buffer was full.",
	}, func() float64 { return float64(dropped()) })
}

// EventsDefaultPartitionInUse is 1 when rows have landed in the DEFAULT partitions of
// events/event_visibility: the stream outran partition maintenance — see docs/operations.md.
var EventsDefaultPartitionInUse = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "nimlet_events_default_partition_in_use",
	Help: "1 if any row is stored in the DEFAULT stream_pos partitions (needs operator action).",
})

// DBPoolStats is the subset of pgxpool.Stat the pool collectors read; keeping it an
// interface keeps this package free of a pgx dependency.
type DBPoolStats interface {
	AcquireCount() int64
	AcquireDuration() time.Duration
	EmptyAcquireCount() int64
	AcquiredConns() int32
	TotalConns() int32
	MaxConns() int32
}

// RegisterDBPool exposes connection-pool pressure. A rising empty-acquire rate or acquire
// wait means requests are queueing for a connection (raise NIMLET_DB_MAX_CONNS or cut queries).
func RegisterDBPool[S DBPoolStats](stat func() S) {
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "nimlet_db_pool_acquires_total",
		Help: "Connections acquired from the pool.",
	}, func() float64 { return float64(stat().AcquireCount()) })
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "nimlet_db_pool_empty_acquires_total",
		Help: "Acquires that had to wait because no idle connection was available.",
	}, func() float64 { return float64(stat().EmptyAcquireCount()) })
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "nimlet_db_pool_acquire_wait_seconds_total",
		Help: "Total time spent acquiring connections.",
	}, func() float64 { return stat().AcquireDuration().Seconds() })
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "nimlet_db_pool_acquired_connections",
		Help: "Connections currently checked out.",
	}, func() float64 { return float64(stat().AcquiredConns()) })
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "nimlet_db_pool_max_connections",
		Help: "Configured pool size.",
	}, func() float64 { return float64(stat().MaxConns()) })
}

// Handler returns the Prometheus metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// ObserveHTTP records one request. pattern must be the route pattern (e.g.
// "/rooms/{roomId}/send/{eventType}/{txnId}"), never the raw path: raw paths carry IDs and
// would create a new time series per room/txn, growing memory without bound.
func ObserveHTTP(method, pattern string, status int, elapsed time.Duration) {
	code := strconv.Itoa(status)
	HTTPRequestsTotal.WithLabelValues(method, pattern, code).Inc()
	HTTPRequestDuration.WithLabelValues(method, pattern).Observe(elapsed.Seconds())
}
