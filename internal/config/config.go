package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
)

type Config struct {
	Server      ServerConfig
	HTTP        HTTPConfig
	DB          DBConfig
	Redis       RedisConfig
	Auth        AuthConfig
	Sync        SyncConfig
	Media       MediaConfig
	AppServices AppServicesConfig
	Push        PushConfig
}

type HTTPConfig struct {
	Addr string // default :8080
	// PprofAddr, when set, serves net/http/pprof on a separate listener. Keep it bound to
	// localhost or a private interface; it is never mounted on the public router.
	PprofAddr string
	// MetricsAddr, when set, serves /metrics on its own listener instead of the API port,
	// so the public API does not expose operational detail.
	MetricsAddr string
	// MaxBodyBytes caps every JSON request body, event content included (default 64 KiB).
	MaxBodyBytes int64
	// TrustProxyHeaders takes the client IP from X-Forwarded-For / X-Real-IP (only behind
	// a proxy that sets them).
	TrustProxyHeaders bool
	// AuthPerIPPerMinute limits /login + /register per client IP (default 30).
	AuthPerIPPerMinute int
}

type ServerConfig struct {
	// Name is the authority in content URIs handed to clients (mxc://<Name>/<mediaID>).
	Name string // default "nimlet"
}

type DBConfig struct {
	DSN      string // postgres connection string
	MaxConns int32  // max open connections (default max(50, NumCPU*4))
	// AutoMigrate applies pending migrations at server startup (default true). Turn it off
	// when the server's role has no DDL rights and `nimlet migrate up` runs separately.
	AutoMigrate bool
	// Sends are written in group-committed batches and concurrent /sync queries run as one
	// statement (postgres.NewBatchedEventStore). MAX 1 disables either.
	EventBatchMax     int // events per write transaction (default 64)
	EventBatchWaitUs  int // µs a write batch waits for more events after its first (default 0)
	EventBatchWorkers int // write batches in flight (default 4)
	// EventBatchSingleTx writes a batch in one transaction that holds the allocation lock
	// until its commit (NIMLET_EVENT_BATCH_MODE=single), instead of allocating in a short
	// transaction and inserting in a second one (split, the default).
	EventBatchSingleTx bool
	SyncReadMax        int // /sync queries per statement (default 64)
	SyncReadWaitUs     int // µs a read group waits for more queries after its first (default 0)
	SyncReadWorkers    int // read groups in flight (default 8)
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type AuthConfig struct {
	HMACSecret []byte // must be 32+ bytes
	// CacheTTLMs: how long an instance reuses a session lookup (default 1000; 0 disables).
	// Revoking a session or disabling a device, user or namespace reaches the other
	// instances within this time.
	CacheTTLMs int
}

type SyncConfig struct {
	Partitions int // default 256
}

type MediaConfig struct {
	Driver   string // "filesystem" or "s3"
	BasePath string // for filesystem driver
	S3Bucket string // for s3 driver
	// S3PathStyle addresses buckets as host/bucket/key (S3-compatible stores need it).
	S3PathStyle bool
	// MaxUploadBytes caps one media upload (default 50 MiB).
	MaxUploadBytes int64
}

type AppServicesConfig struct {
	File string // path to YAML file; empty means no AppServices
}

type PushConfig struct {
	// FCMCredentialsFile is a Firebase service-account JSON key; empty disables
	// push entirely (roomService gets push.NopDispatcher instead).
	FCMCredentialsFile string
}

// Load reads the full server configuration.
func Load() (*Config, error) {
	db, err := LoadDB()
	if err != nil {
		return nil, err
	}

	secret := os.Getenv("NIMLET_AUTH_HMAC_SECRET")
	if len(secret) < 32 {
		return nil, fmt.Errorf("config: NIMLET_AUTH_HMAC_SECRET must be at least 32 characters")
	}

	var errs []error
	cfg := &Config{
		Server: ServerConfig{
			Name: envOr("NIMLET_SERVER_NAME", "nimlet"),
		},
		HTTP: HTTPConfig{
			Addr:               envOr("NIMLET_HTTP_ADDR", ":8080"),
			PprofAddr:          os.Getenv("NIMLET_PPROF_ADDR"),
			MetricsAddr:        os.Getenv("NIMLET_METRICS_ADDR"),
			MaxBodyBytes:       int64(envInt(&errs, "NIMLET_HTTP_MAX_BODY_BYTES", 64<<10)),
			TrustProxyHeaders:  os.Getenv("NIMLET_HTTP_TRUST_PROXY_HEADERS") == "true",
			AuthPerIPPerMinute: envInt(&errs, "NIMLET_AUTH_RATE_PER_MIN", 30),
		},
		DB: *db,
		Redis: RedisConfig{
			Addr:     envOr("NIMLET_REDIS_ADDR", "localhost:6379"),
			Password: os.Getenv("NIMLET_REDIS_PASSWORD"),
			DB:       envInt(&errs, "NIMLET_REDIS_DB", 0),
		},
		Auth: AuthConfig{
			HMACSecret: []byte(secret),
			CacheTTLMs: envInt(&errs, "NIMLET_AUTH_CACHE_TTL_MS", 1000),
		},
		Sync: SyncConfig{
			Partitions: envInt(&errs, "NIMLET_SYNC_PARTITIONS", 256),
		},
		Media: MediaConfig{
			Driver:         envOr("NIMLET_MEDIA_DRIVER", "filesystem"),
			BasePath:       envOr("NIMLET_MEDIA_PATH", "./media"),
			S3Bucket:       os.Getenv("NIMLET_S3_BUCKET"),
			S3PathStyle:    os.Getenv("NIMLET_S3_PATH_STYLE") == "true",
			MaxUploadBytes: int64(envInt(&errs, "NIMLET_MEDIA_MAX_BYTES", 50<<20)),
		},
		AppServices: AppServicesConfig{
			File: os.Getenv("NIMLET_APP_SERVICES_FILE"),
		},
		Push: PushConfig{
			FCMCredentialsFile: os.Getenv("NIMLET_FCM_CREDENTIALS_FILE"),
		},
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadDB reads only the database settings, for commands (migrate, admin) that must not
// require the server's secrets.
func LoadDB() (*DBConfig, error) {
	dsn := os.Getenv("NIMLET_DB_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("config: NIMLET_DB_DSN is required")
	}
	var errs []error
	p, err := profileFromEnv()
	if err != nil {
		return nil, err
	}
	maxConns := p.dbMaxConns
	if maxConns == 0 {
		maxConns = defaultDBMaxConns()
	}
	db := &DBConfig{
		DSN:         dsn,
		MaxConns:    int32(envInt(&errs, "NIMLET_DB_MAX_CONNS", maxConns)),
		AutoMigrate: envOr("NIMLET_AUTO_MIGRATE", "true") != "false",

		EventBatchMax:     envInt(&errs, "NIMLET_EVENT_BATCH_MAX", 64),
		EventBatchWaitUs:  envInt(&errs, "NIMLET_EVENT_BATCH_WAIT_US", p.batchWaitUs),
		EventBatchWorkers: envInt(&errs, "NIMLET_EVENT_BATCH_WORKERS", p.batchWorkers),
		SyncReadMax:       envInt(&errs, "NIMLET_SYNC_READ_MAX", 64),
		SyncReadWaitUs:    envInt(&errs, "NIMLET_SYNC_READ_WAIT_US", p.readWaitUs),
		SyncReadWorkers:   envInt(&errs, "NIMLET_SYNC_READ_WORKERS", p.readWorkers),
	}
	switch mode := envOr("NIMLET_EVENT_BATCH_MODE", "split"); mode {
	case "split":
	case "single":
		db.EventBatchSingleTx = true
	default:
		errs = append(errs, fmt.Errorf("config: NIMLET_EVENT_BATCH_MODE=%q: want split or single", mode))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return db, nil
}

// LoadAdminDSN returns the database DSN for the admin CLI. It is deliberately a separate
// variable from the server's NIMLET_DB_DSN: the CLI should run with a role that may
// provision namespaces and users, which the server's runtime role should not have.
func LoadAdminDSN() (string, error) {
	dsn := os.Getenv("NIMLET_ADMIN_DB_DSN")
	if dsn == "" {
		return "", fmt.Errorf("config: NIMLET_ADMIN_DB_DSN is required for admin commands")
	}
	return dsn, nil
}

// profile holds the defaults NIMLET_PROFILE selects; any explicit variable still wins.
// The values come from docs/benchmarks.md (phase 04). Per instance, grouping only pays off
// when requests queue up: a small instance sees little concurrent load, so it waits a little
// to fill groups and keeps few workers and connections (Postgres serialises allocations
// anyway, and every connection costs it planning and memory); a large one fills groups by
// itself and needs the parallelism.
type profile struct {
	batchWorkers, batchWaitUs int
	readWorkers, readWaitUs   int
	dbMaxConns                int // 0: defaultDBMaxConns()
}

var profiles = map[string]profile{
	// Up to ~0.5 CPU per instance, many instances.
	"small": {batchWorkers: 1, batchWaitUs: 2000, readWorkers: 1, readWaitUs: 2000, dbMaxConns: 12},
	// About 1-2 CPUs per instance.
	"medium": {batchWorkers: 1, batchWaitUs: 1000, readWorkers: 1, readWaitUs: 1000, dbMaxConns: 16},
	// 2+ CPUs per instance, few instances.
	"large": {batchWorkers: 4, batchWaitUs: 0, readWorkers: 8, readWaitUs: 0},
}

func profileFromEnv() (profile, error) {
	name := envOr("NIMLET_PROFILE", "large")
	p, ok := profiles[name]
	if !ok {
		return profile{}, fmt.Errorf("config: NIMLET_PROFILE=%q: want small, medium or large", name)
	}
	return p, nil
}

func defaultDBMaxConns() int {
	n := runtime.NumCPU() * 4
	if n < 50 {
		return 50
	}
	return n
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt reads an integer variable. An unparsable value is recorded in errs instead of
// silently falling back to the default: a typo in NIMLET_DB_MAX_CONNS must not go unnoticed.
func envInt(errs *[]error, key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("config: %s=%q is not an integer", key, v))
		return fallback
	}
	return n
}
