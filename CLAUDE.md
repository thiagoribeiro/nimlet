# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Nimlet is a small, self-hostable HTTP chat/event backend (Go), providing users/devices/sessions,
rooms/memberships, append-only events, idempotent sends, media attachments, and an incremental
`/sync` long-poll endpoint. PostgreSQL is the source of truth; Redis Pub/Sub is only a cross-instance
wake-up mechanism (never a source of truth — if a pub/sub message is lost, clients just fall back to
their long-poll timeout and re-sync from Postgres). See README.md for the full protocol/API reference
and schema — it is comprehensive and should be treated as authoritative for HTTP contract details not
repeated here.

## Common commands

```bash
make build              # go build -o bin/nimlet ./cmd/nimlet
make test-unit          # go test -race -count=1 ./...
make test-integration   # go test -race -count=1 -tags integration -timeout 2m ./...
make lint               # golangci-lint run ./...
make fmt                # gofmt -w .
make tidy               # go mod tidy
make docker-up          # start local Postgres + Redis (deploy/docker-compose.dev.yml)
make docker-down        # tear down local Postgres + Redis
make run                # run the server against docker-up infra (auto-runs migrations)
make bench              # go test -bench=. -benchmem -count=3 -tags integration ./internal/adapter/postgres/...
```

Admin CLI (namespaces/users/rooms/memberships; talks to the DB directly, no HTTP admin API):

```bash
NIMLET_ADMIN_DB_DSN=... ./bin/nimlet namespace create acme
NIMLET_ADMIN_DB_DSN=... ./bin/nimlet user create alice -n acme --password-stdin
```

Migrations (goose, embedded in the binary, auto-applied at server startup unless
`NIMLET_AUTO_MIGRATE=false` — manual commands only needed for out-of-band operations):

```bash
make migrate-up
make migrate-down
make migrate-status
```

### Running a single test

Unit tests have no build tag and run without external services:

```bash
go test -race -run TestName ./internal/usecase/...
```

Integration and contract tests are gated behind `//go:build integration` and require live Postgres
and Redis (`make docker-up` first), plus env vars:

```bash
export TEST_POSTGRES_DSN="postgres://nimlet:nimlet@localhost:5432/nimlet_test?sslmode=disable"
export TEST_REDIS_ADDR="localhost:6379"
# optional: S3 adapter tests against any S3-compatible store (bench/infra.sh starts SeaweedFS)
export TEST_S3_ENDPOINT="http://127.0.0.1:59000" TEST_S3_ACCESS_KEY=any TEST_S3_SECRET_KEY=any
go test -race -tags integration -run TestName ./test/integration/...
```

`internal/adapter/postgres/stores_integration_test.go` and `internal/adapter/redis/bus_integration_test.go`
run the shared contract suites in `testkit/contract_*.go` against real adapters — when adding a new
store/bus method, add it to the contract test first so both the real adapter and any mock in
`testkit/mocks.go` are exercised the same way.

## Architecture

Hexagonal (ports & adapters). Dependency direction is always inward — adapters and transport depend
on ports/domain, never the reverse.

```
internal/
  domain/          pure types: Namespace, User, Device, Session, Room, Membership, Event, Media,
                   AppService — no I/O
  port/in/         use-case interfaces the transport layer calls (AuthService, RoomService,
                   SyncService, MediaService, AppServiceDispatcher) + their Cmd/Result structs
  port/out/        interfaces usecases depend on (EventStore, MembershipStore, ..., EventBus)
  usecase/         business logic implementing port/in, depending only on port/out + domain
  sync/            WaiterRegistry (in-memory long-poll waiters), Dispatcher (bridges bus → waiters),
                   Coalescer
  appservice/      Registry (loads YAML config of external bot/bridge services) + Dispatcher
                   (batches and HTTP-delivers events to interested AppServices — see below)
  adapter/
    postgres/      port/out implementations backed by Postgres (pgx)
    redis/         EventBus implementation using Redis Pub/Sub
    s3/            MediaStore implementation for S3-compatible storage
    filesystem/    MediaStore implementation for local disk
  transport/http/  chi router, handlers, middleware, request/response DTOs
  config/          env-var driven Config, loaded once by `nimlet server` (cmd/nimlet → internal/app)
```

To trace a request: `transport/http` handler → `port/in` service method → `usecase` implementation →
`port/out` store/bus interface → concrete `adapter/*`.

### Read/write model for `/sync`

- Small rooms (<1000 members): sends fan out by writing one `event_visibility` row per member;
  `/sync` reads from `event_visibility JOIN events`.
- Large rooms (>=1000 members, tracked via `rooms.member_count`): sends write only to `events`;
  `/sync` instead pulls directly by `room_id + stream_pos > since` (UNION with the small-room
  query). This avoids O(N) fan-out writes for big rooms.
- Ordering: `/sync` cursors are "last stream_pos seen", so sends allocate positions in a short
  ordered transaction that marks them in `stream_inflight`, and reads never pass the lowest
  in-flight position (read ceiling). Don't bypass `EventStore.Create` for event inserts, and keep
  `GetVisible`'s per-source `ORDER BY ... LIMIT` shape — it keeps Postgres' generic plan fast.
- Cursors: clients never see `stream_pos` (it is global across namespaces and would leak
  other tenants' volume). `internal/cursor` seals it into an opaque, user-bound token for
  `/sync` (`since`/`next_batch`). Never add `stream_pos` to a response.
- `room_seq`: a per-room counter (`room_seqs` table) assigned in the same serialised allocation
  as `stream_pos`, so within a room it matches delivery order (gaps are possible). It is safe to
  expose: events (`internal/wire.Event`) and the send response carry it, and `/messages` pages
  by it (`from`/`start`/`end` are plain room_seq numbers).
- Wake-up: Redis Pub/Sub channels are partitioned via `fnv32a(key) % NIMLET_SYNC_PARTITIONS`
  (default 256), where `key` is a userID (small rooms) or `"room:"+roomID` (large rooms). The
  `sync.Dispatcher` goroutine bridges incoming Redis messages to the in-memory `WaiterRegistry`.
  Redis is purely a latency optimization — correctness never depends on it.

### Memberships

Every membership change goes through `membershipOps.change` (`internal/usecase/membership.go`):
`domain.NextMembership` decides (bans are final, invites never touch existing members, DMs only
admit their two participants, leaving an invite-only room needs a new invite), and
`MembershipStore.Transition` applies it under a row lock so `member_count` moves only on real
join/leave transitions. Don't write memberships with `Set` except for a new room's first members
or an explicit role change.

### Events are immutable

State changes (redactions, reactions, membership changes) are represented as new events, never
mutations of existing rows. `sender_id`/`device_id` are always derived server-side from the
authenticated session — clients never self-declare identity.

### Idempotent sends

`PUT /rooms/{roomId}/send/{eventType}/{txnId}` keys idempotency on `deviceID + ":" + txnId`, recorded
in `event_idempotency` alongside the resulting `stream_pos`; a retried txnId returns the original
event rather than creating a duplicate.

### Admin CLI

`cmd/nimlet/admin*.go` (cobra) → `in.AdminService` (`internal/usecase/admin.go`) → the same
stores the server uses; wiring in `internal/app/admin.go`. Every mutation goes through
`adminService.audit` (writes `admin_audit`, outside the operation's tx so failures are recorded
too). Exit codes are a contract (`cmd/nimlet/admin.go`). `cmd/nimlet/admin_integration_test.go`
drives the CLI in-process against Postgres.

### Namespaces (multi-tenant isolation)

Users, rooms, memberships and sessions carry `namespace_id`; usernames, room aliases and DM keys
are unique per namespace. Composite FKs (`(namespace_id, user_id)`, `(namespace_id, room_id)`)
make cross-namespace memberships/rooms/sessions impossible in the DB — a membership write for a
user or room of another namespace fails with ErrNotFound (FK violation mapped in the store).
Internal IDs stay globally unique, so `events`, `event_visibility`, `event_idempotency`, Redis keys
and the `/sync` SQL have **no** namespace column/filter; isolation comes from relations being
created only within a namespace. Rules when changing code:

- The namespace comes only from the session (`Actor.NamespaceID`) or the AppService registration —
  never from request fields. `/login` and `/register`'s `namespace` is a selector, not authority.
- Anything resolving an external name (username, alias, DM key) takes a namespace ID.
- Use cases that create rooms/memberships take `in.Actor`; a resource of another namespace must
  behave exactly like a missing one (ErrNotFound → 404), before any permission check.
- `test/integration/namespace_isolation_test.go` is the A/B regression matrix — extend it with
  every new endpoint.

### AppServices (bot/bridge integration)

`internal/appservice` implements a Matrix-style Application Service bridge, configured via a YAML
file (`NIMLET_APP_SERVICES_FILE`) loaded into an `appservice.Registry` at startup. Each configured
AppService gets a dedicated worker goroutine in `appservice.Dispatcher` that batches interested events
(5ms window or 100 events, whichever first) and HTTP-POSTs them to `{Endpoint}/events` with the
`hs_token`. Delivery is fire-and-forget and non-blocking: a full worker channel drops the event with a
logged warning rather than blocking event publication. Each AppService belongs to one namespace
(YAML `namespace`, default `default`); the dispatcher drops events of other namespaces before
matching `interested_events`, and `/appservice/{asId}/...` requires the token's own `asId`.

### Multi-instance deployment

Each instance subscribes to all Redis partitions. `NIMLET_DB_MAX_CONNS` must be set to
`max_postgres_connections / num_instances` to avoid exhausting the Postgres connection limit —
the default (`max(50, NumCPU*4)`) assumes a single instance.

## Repo housekeeping

`READMEmd` at the repo root is a stray duplicate of `README.md` (identical content, no dot) — not a
real doc, don't treat it as a separate source of truth.
