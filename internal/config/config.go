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
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type AuthConfig struct {
	HMACSecret []byte // must be 32+ bytes
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
	db := &DBConfig{
		DSN:         dsn,
		MaxConns:    int32(envInt(&errs, "NIMLET_DB_MAX_CONNS", defaultDBMaxConns())),
		AutoMigrate: envOr("NIMLET_AUTO_MIGRATE", "true") != "false",
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
