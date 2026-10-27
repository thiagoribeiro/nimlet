// Package app wires adapters, use cases and transport into runnable commands
// (the HTTP server and migrations) for cmd/nimlet.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thiagoribeiro/nimlet/internal/adapter/fcm"
	"github.com/thiagoribeiro/nimlet/internal/adapter/filesystem"
	"github.com/thiagoribeiro/nimlet/internal/adapter/postgres"
	"github.com/thiagoribeiro/nimlet/internal/adapter/redis"
	s3adapter "github.com/thiagoribeiro/nimlet/internal/adapter/s3"
	"github.com/thiagoribeiro/nimlet/internal/appservice"
	appcfg "github.com/thiagoribeiro/nimlet/internal/config"
	"github.com/thiagoribeiro/nimlet/internal/cursor"
	"github.com/thiagoribeiro/nimlet/internal/domain"
	"github.com/thiagoribeiro/nimlet/internal/metrics"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
	"github.com/thiagoribeiro/nimlet/internal/port/out"
	"github.com/thiagoribeiro/nimlet/internal/push"
	nimsync "github.com/thiagoribeiro/nimlet/internal/sync"
	transphttp "github.com/thiagoribeiro/nimlet/internal/transport/http"
	"github.com/thiagoribeiro/nimlet/internal/usecase"
)

// pushCoalesceWindow is how long push.Dispatcher waits per recipient before
// flushing a batched digest — see internal/push.Dispatcher's doc comment.
const pushCoalesceWindow = 5 * time.Second

// roomSweepInterval is how often the background sweeper reclaims rooms past their TTL.
const roomSweepInterval = 10 * time.Minute

// initAppServices loads the AppService registry, provisions a system user+device for each AS,
// and starts the async event dispatcher. Returns an empty registry+nop dispatcher if no file configured.
func initAppServices(
	ctx context.Context,
	cfg *appcfg.Config,
	namespaceStore out.NamespaceStore,
	userStore out.UserStore,
	deviceStore out.DeviceStore,
) (*appservice.Registry, in.AppServiceDispatcher, error) {
	if cfg.AppServices.File == "" {
		return appservice.Empty(), appservice.NopDispatcher{}, nil
	}

	reg, err := appservice.LoadFromFile(cfg.AppServices.File)
	if err != nil {
		return nil, nil, err
	}

	for _, as := range reg.All() {
		// The AS lives in exactly one namespace, which must exist and be active: its
		// token, sender user and event delivery are all scoped to it.
		ns, err := namespaceStore.GetBySlug(ctx, as.NamespaceSlug)
		if err != nil {
			return nil, nil, fmt.Errorf("appservice %q: namespace %q: %w", as.ID, as.NamespaceSlug, err)
		}
		if !ns.IsActive() {
			return nil, nil, fmt.Errorf("appservice %q: namespace %q is disabled", as.ID, as.NamespaceSlug)
		}
		as.NamespaceID = ns.ID

		user, _, err := userStore.GetByUsername(ctx, ns.ID, as.SenderUsername)
		if err != nil {
			if as.SenderUsername == "" {
				return nil, nil, fmt.Errorf("appservice %q: empty sender_username", as.ID)
			}
			// Provision a system user for this AppService.
			user = &domain.User{
				ID:          uuid.NewString(),
				NamespaceID: ns.ID,
				Username:    as.SenderUsername,
				DisplayName: as.ID,
				Type:        domain.UserTypeService,
				CreatedAt:   time.Now().UTC(),
			}
			if createErr := userStore.Create(ctx, user, ""); createErr != nil {
				return nil, nil, fmt.Errorf("appservice %q: create user: %w", as.ID, createErr)
			}
			slog.Info("appservice: provisioned user", "as_id", as.ID, "namespace", ns.Slug, "user_id", user.ID)
		} else if user.Type != domain.UserTypeService {
			return nil, nil, fmt.Errorf("appservice %q: sender_username %q belongs to a %s account", as.ID, as.SenderUsername, user.Type)
		}
		as.SenderUserID = user.ID

		// Provision a device for the AS user (deterministic ID: "as-device-<asID>").
		deviceID := "as-device-" + as.ID
		device, err := deviceStore.GetByID(ctx, deviceID)
		if err != nil {
			device = &domain.Device{
				ID:          deviceID,
				UserID:      user.ID,
				DisplayName: as.ID + " AppService",
				CreatedAt:   time.Now().UTC(),
			}
			if createErr := deviceStore.Create(ctx, device); createErr != nil {
				return nil, nil, fmt.Errorf("appservice %q: create device: %w", as.ID, createErr)
			}
		}
		as.SenderDeviceID = device.ID
	}

	d := appservice.NewDispatcher(reg)
	d.Start(ctx)
	return reg, d, nil
}

// RunServer starts the HTTP server and its background workers, applying pending
// migrations first. It blocks until ctx is cancelled or the server fails.
func RunServer(ctx context.Context, cfg *appcfg.Config) error {
	// --- Database ---
	pool, err := postgres.Open(ctx, cfg.DB.DSN, cfg.DB.MaxConns)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	metrics.RegisterDBPool(pool.Stat)

	if cfg.DB.AutoMigrate {
		if err := Migrate(ctx, cfg.DB.DSN, "up"); err != nil {
			return err
		}
		slog.Info("migrations applied")
	} else {
		slog.Info("migrations: auto-migrate disabled (NIMLET_AUTO_MIGRATE=false)")
	}

	maintainPartitions(ctx, pool)

	// --- Redis ---
	redisClient, err := redis.NewClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer redisClient.Close()

	// --- Adapters ---
	userStore := postgres.NewUserStore(pool)
	deviceStore := postgres.NewDeviceStore(pool)
	sessionStore := postgres.NewSessionStore(pool)
	roomStore := postgres.NewRoomStore(pool)
	membershipStore := postgres.NewMembershipStore(pool)
	eventStore := postgres.NewEventStore(pool)
	if cfg.DB.EventBatchMax > 1 || cfg.DB.SyncReadMax > 1 {
		us := func(n int) time.Duration { return time.Duration(n) * time.Microsecond }
		batched := postgres.NewBatchedEventStore(pool, postgres.BatchConfig{
			Writes: postgres.GroupLimits{MaxItems: cfg.DB.EventBatchMax, MaxWait: us(cfg.DB.EventBatchWaitUs), Workers: cfg.DB.EventBatchWorkers},
			Reads:  postgres.GroupLimits{MaxItems: cfg.DB.SyncReadMax, MaxWait: us(cfg.DB.SyncReadWaitUs), Workers: cfg.DB.SyncReadWorkers},
		})
		// Deferred, so it runs after the HTTP server's graceful shutdown below: in-flight
		// sends are written before the workers stop.
		defer batched.Close()
		eventStore = batched
	}
	namespaceStore := postgres.NewNamespaceStore(pool)
	mediaIndex := postgres.NewMediaIndex(pool)
	txr := postgres.NewTransactor(pool)
	bus := redis.NewBus(redisClient, cfg.Sync.Partitions)
	if d, ok := bus.(interface{ DroppedNotifications() int64 }); ok {
		metrics.RegisterBusDropped(d.DroppedNotifications)
	}

	mediaStore, err := newMediaStore(ctx, cfg)
	if err != nil {
		return err
	}

	// --- AppServices ---
	asRegistry, asDispatcher, err := initAppServices(ctx, cfg, namespaceStore, userStore, deviceStore)
	if err != nil {
		return fmt.Errorf("appservices: %w", err)
	}

	// --- Presence + Push ---
	// presenceStore backs both /sync (marks a user as actively connected) and push
	// delivery (skips anyone who is). pushDispatcher stays a no-op unless a Firebase
	// service-account key is configured.
	presenceStore := redis.NewPresenceStore(redisClient)
	pushQueue := redis.NewPushQueue(redisClient)

	pushDispatcher, err := newPushDispatcher(ctx, cfg, pushQueue, presenceStore, deviceStore, namespaceStore, userStore)
	if err != nil {
		return err
	}

	// --- Use cases ---
	authSvc := usecase.NewAuthService(userStore, deviceStore, sessionStore, namespaceStore, txr, cfg.Auth.HMACSecret)
	roomSvc := usecase.NewRoomService(roomStore, membershipStore, eventStore, bus, txr, asDispatcher, pushDispatcher)
	mediaSvc := usecase.NewMediaService(mediaStore, mediaIndex, cfg.Server.Name)

	registry := nimsync.NewWaiterRegistry()
	syncSvc := usecase.NewSyncService(eventStore, membershipStore, registry, presenceStore)

	// --- Dispatcher: bridge Redis → WaiterRegistry ---
	dispatcher := nimsync.NewDispatcher(bus, registry)
	go func() {
		slog.Info("dispatcher starting", "partitions", cfg.Sync.Partitions)
		dispatcher.Run(ctx)
		slog.Info("dispatcher stopped")
	}()

	go runSweeper(ctx, pool, roomStore, sessionStore)

	if cfg.HTTP.PprofAddr != "" {
		go servePprof(ctx, cfg.HTTP.PprofAddr)
	}
	if cfg.HTTP.MetricsAddr != "" {
		go serveMetrics(ctx, cfg.HTTP.MetricsAddr)
	}

	cursors, err := cursor.New(cfg.Auth.HMACSecret)
	if err != nil {
		return fmt.Errorf("cursor codec: %w", err)
	}

	// --- HTTP server ---
	authMiddleware := transphttp.NewAuthMiddleware(sessionStore, cfg.Auth.HMACSecret)
	asAuthMiddleware := transphttp.NewAppServiceAuthMiddleware(asRegistry)
	srv := transphttp.NewServer(cfg.HTTP.Addr, authMiddleware, asAuthMiddleware, authSvc, roomSvc, syncSvc, mediaSvc, transphttp.Options{
		Cursors:            cursors,
		MaxBodyBytes:       cfg.HTTP.MaxBodyBytes,
		MaxUploadBytes:     cfg.Media.MaxUploadBytes,
		MetricsOnAPI:       cfg.HTTP.MetricsAddr == "",
		TrustProxyHeaders:  cfg.HTTP.TrustProxyHeaders,
		AuthPerIPPerMinute: cfg.HTTP.AuthPerIPPerMinute,
	})

	// Start server in background.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", cfg.HTTP.Addr)
		errCh <- srv.ListenAndServe()
	}()

	// Block until signal or server error.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	}

	// Graceful shutdown: 30s for in-flight requests (including long-poll /sync).
	// /sync goroutines unblock immediately via ctx.Done().
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	slog.Info("server stopped")
	return nil
}

// newMediaStore returns the S3 store when NIMLET_MEDIA_DRIVER=s3, else the filesystem one.
func newMediaStore(ctx context.Context, cfg *appcfg.Config) (out.MediaStore, error) {
	if cfg.Media.Driver != "s3" {
		return filesystem.NewMediaStore(cfg.Media.BasePath), nil
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	// Endpoint overrides for S3-compatible stores come from the SDK's own
	// AWS_ENDPOINT_URL_S3; most of them (MinIO, SeaweedFS, Garage…) also need path-style.
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) { o.UsePathStyle = cfg.Media.S3PathStyle })
	return s3adapter.NewMediaStore(client, cfg.Media.S3Bucket), nil
}

// newPushDispatcher is a no-op unless a Firebase service-account key is configured.
func newPushDispatcher(ctx context.Context, cfg *appcfg.Config, queue out.PushQueue, presence out.PresenceStore, devices out.DeviceStore, namespaces out.NamespaceStore, users out.UserStore) (in.PushDispatcher, error) {
	if cfg.Push.FCMCredentialsFile == "" {
		slog.Info("push: disabled (NIMLET_FCM_CREDENTIALS_FILE not set)")
		return push.NopDispatcher{}, nil
	}
	sender, err := fcm.NewSender(ctx, cfg.Push.FCMCredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("fcm: %w", err)
	}
	slog.Info("push: FCM dispatcher enabled")
	return push.NewDispatcher(queue, presence, devices, sender, pushCoalesceWindow).WithMessageMode(namespaces, users), nil
}

// runSweeper periodically reclaims rooms past their TTL, expired sessions and abandoned
// stream allocations, and keeps partitions ahead. It returns when ctx is cancelled.
func runSweeper(ctx context.Context, pool *pgxpool.Pool, rooms out.RoomStore, sessions out.SessionStore) {
	ticker := time.NewTicker(roomSweepInterval)
	defer ticker.Stop()
	slog.Info("room sweeper starting", "interval", roomSweepInterval)
	for {
		select {
		case <-ctx.Done():
			slog.Info("room sweeper stopped")
			return
		case <-ticker.C:
			bg := context.WithoutCancel(ctx)
			if n, err := rooms.DeleteExpired(bg); err != nil {
				slog.Error("room sweeper: delete expired", "error", err)
			} else if n > 0 {
				slog.Info("room sweeper: reclaimed expired rooms", "count", n)
			}
			maintainPartitions(bg, pool)
			if n, err := sessions.DeleteExpired(bg); err != nil {
				slog.Error("sweeper: expired sessions", "error", err)
			} else if n > 0 {
				slog.Info("sweeper: removed expired sessions", "count", n)
			}
			if n, err := postgres.SweepStreamInflight(bg, pool); err != nil {
				slog.Error("sweeper: stream_inflight", "error", err)
			} else if n > 0 {
				slog.Warn("sweeper: removed abandoned stream allocations", "count", n)
			}
		}
	}
}

// maintainPartitions keeps stream_pos partitions ahead of the stream and raises an alarm
// metric if the DEFAULT partitions are in use. Failures are logged, never fatal.
func maintainPartitions(ctx context.Context, pool *pgxpool.Pool) {
	created, defaultInUse, err := postgres.EnsureEventPartitions(ctx, pool)
	if err != nil {
		slog.Error("partitions: maintenance failed", "error", err)
		return
	}
	if created > 0 {
		slog.Info("partitions: created stream_pos partitions ahead", "count", created)
	}
	if defaultInUse {
		metrics.EventsDefaultPartitionInUse.Set(1)
		slog.Error("partitions: rows in DEFAULT partitions — run the repartition runbook (docs/operations.md)")
	} else {
		metrics.EventsDefaultPartitionInUse.Set(0)
	}
}
