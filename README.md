# Nimlet

**Messaging made simple.**

### Your application needs messaging. Not a messaging platform.

Nimlet is a **lightweight, self-hosted, HTTP-first messaging backend** for connecting people, applications, devices, services, and AI agents over the Internet. It provides **authentication, room-level authorization, persistent events, and recoverable synchronization** without requiring a full messaging platform.

Build the experience your product needs. Let Nimlet handle the communication.

[Quick start](#quick-start) · [API overview](#api-overview) · [AppServices](#appservices) · [Benchmarks](docs/benchmarks.md) · [Operations](docs/operations.md)

---

## Why Nimlet?

Many applications need *some* messaging, but messaging is not their product.

A small SaaS might need conversations between users. A connected device might need to exchange commands and replies with a remote service. An AI-powered application might need a persistent chat interface around an existing agent. These are different products, but they run into the same infrastructure problems:

- **Identity and trust:** Who sent an event? Who is allowed to receive it?
- **Unreliable connectivity:** What happens when a client goes offline and reconnects?
- **Persistence and ordering:** How does a participant catch up without missing events?
- **Retries:** How can a client safely retry a send?
- **Integration:** How do bots and external services participate in the same conversation?

Building these primitives into every application is repetitive work. Adopting a complete collaboration suite or federated messaging stack can be more complexity than the feature warrants. A managed messaging service may also introduce usage-based or per-user costs.

**Nimlet targets the middle ground:** the essential messaging capabilities as a focused application backend, with infrastructure you control.

Small requirements do not necessarily mean small applications. The goal is to keep the **messaging surface area proportional to what your product actually needs**.

---

## What Nimlet provides

- **Authenticated participants:** users, devices, sessions, and service identities; the server derives sender identity from the authenticated session.
- **Rooms and permissions:** memberships, roles, and authorization for reading and publishing events.
- **Persistent communication:** append-only room events in PostgreSQL, with idempotent sends and paginated history.
- **Reconnect-friendly synchronization:** incremental `/sync` over HTTP with long polling and opaque cursors; clients can recover visible events after disconnections.
- **Internet-friendly integration:** ordinary HTTP APIs that can be exposed over HTTPS without requiring a permanent WebSocket connection or direct client access to a broker.
- **AppServices:** namespace-scoped bridges for bots and external systems to provision participants, publish events, and receive notifications.
- **Multiple independent applications:** namespaces isolate users, rooms, sessions, events, and media within a shared instance.
- **Optional media and push:** filesystem or S3-compatible media, plus Android push notifications when configured.

The core doesn't need to understand what every event means. It **stores, orders, authorizes, and synchronizes events** while each application owns its UI and business behavior.

---

## Where it fits

| You're building… | Nimlet helps with… |
| --- | --- |
| **An embedded chat feature** in an app or small SaaS | Users, rooms, history, authorization, reconnects, and media without adopting an entire chat product |
| **An AI-powered assistant** | Persistent conversations between users and an external agent, independent of the LLM or orchestration framework |
| **A companion app or connected device** | Authenticated commands and responses across networks, with offline catch-up |
| **A bot or application integration** | Service participants and room events through AppServices |
| **Several small products** | Reusing one deployment with isolated application namespaces |

Nimlet is designed for **application-level communication**, not for replacing a general-purpose message broker or implementing an entire collaboration platform.

### Bring your own AI agent

Connecting an LLM is often straightforward. Building the conversation experience around it — identities, permissions, history, reconnection, and external integration — is a separate problem.

A typical integration looks like this:

1. Your web or mobile app authenticates with Nimlet and sends a message to a room.
2. An external worker or agent integration receives the event and invokes **your** LLM, tools, or workflow engine.
3. The agent publishes its response as an authorized participant in that room.
4. Clients use `/sync` to receive the reply and catch up if they disconnect.

Nimlet deliberately doesn't run agents, call models, or interpret your application-specific events.

**Optional rich interactions:** [BRIAN](https://github.com/thiagoribeiro/brian) defines a transport-independent format for structured messages, such as forms, approvals, and workflow cards. [Agent Blocks](https://github.com/thiagoribeiro/agent-blocks) provides an optional runtime for stateful plugins and commands. These projects can use Nimlet as their event transport; **neither is required** to use Nimlet.

**Delivery boundary:** AppService HTTP callbacks are currently batched and **best-effort**, not a durable job queue. Integrations requiring reliable processing should consume persisted events via an authenticated `/sync` session and manage checkpoints, deduplication, and processing outcomes. Agent Blocks follows this pattern by reconciling event history.

---

## Deliberately focused infrastructure

Nimlet is implemented as a Go server with **PostgreSQL as the source of truth**. For a single instance, Redis is not required for correctness or local wake-ups; multiple instances can use Redis Pub/Sub to lower notification latency. Media can be stored locally or in S3-compatible storage.

This is a **small product scope**, not a claim that infrastructure has zero cost or that Nimlet outperforms every alternative. PostgreSQL still needs to be deployed and operated. Reproducible [benchmarks](docs/benchmarks.md) document performance and engineering trade-offs; comparative end-to-end memory and hosting-cost results are not yet established.

### Design goals

- **Small, reusable core:** identities, rooms, memberships, events, synchronization, auth, and optional media.
- **HTTP-first:** simple integration without mandatory WebSockets.
- **Recoverable by design:** PostgreSQL persists events; cursors provide catch-up; Redis accelerates wake-ups but is not the source of truth.
- **Idempotent send API:** retries with the same device transaction ID don't create duplicate events.
- **Extensible, not AI-specific:** humans, bots, agents, devices, and services can all participate.
- **Application isolation:** independent namespaces share infrastructure without sharing identities or room data.
- **Observable and operable:** explicit configuration, metrics, benchmarks, migrations, and an audited administration CLI.

### Security and reliability boundaries

- Exposing Nimlet on the public Internet requires **HTTPS/TLS termination**, secure secret management, and appropriate network and operational protections; built-in session authentication is not a substitute for deployment security.
- `/sync` recovers **persisted, visible events while they remain retained**. This does not mean every downstream consumer has processed an event.
- Idempotent sends don't imply exactly-once execution of external side effects.
- Namespaces isolate data and authorization, **not CPU, memory, database capacity, or failures**.
- Nimlet does not provide end-to-end encryption.

### What Nimlet is not

Nimlet is intentionally **not** a Matrix homeserver, federation protocol, Slack/Mattermost/Rocket.Chat replacement, general-purpose broker, LLM agent framework, or workflow engine. It is also not a WebSocket-first realtime server or an end-to-end encrypted messenger like Signal.

It borrows useful ideas from event-based chat systems while keeping the implementation focused on **self-hosted messaging inside applications**.

---

## Current implementation

Nimlet is implemented in Go.

The current stack is:

- **Go** for the server
- **PostgreSQL** for persistent state
- **pgx** for database access
- **goose** for database migrations (embedded, run automatically at startup)
- **chi** for HTTP routing
- **Redis Pub/Sub** for cross-instance `/sync` wake-up
- **S3-compatible storage** for optional media storage
- **opaque HMAC bearer tokens** for session authentication

---

## Architecture

```text
+-------------------+
| Client App        |
| Web / Mobile / CLI|
+---------+---------+
          |
          | HTTP
          v
+-------------------+
| Nimlet Server   |
|                   |
| - auth            |
| - rooms           |
| - memberships     |
| - events          |
| - media           |
| - sync            |
+----+---------+----+
     |         |
     |         +----------------+
     |                          |
     v                          v
+------------+           +----------------+
| PostgreSQL |           | Redis Pub/Sub  |
| source of  |           | wake-up only   |
| truth      |           | optional       |
+------------+           +----------------+
     |
     v
+-------------------+
| Media Store       |
| local / S3 / R2   |
| optional          |
+-------------------+
```

PostgreSQL is the source of truth.

Redis is only a wake-up mechanism. If Redis is unavailable or a Pub/Sub message is lost, no chat event is lost. Clients wait until the long-poll timeout and then catch up from PostgreSQL via `/sync`.

---

## Project layout

```text
nimlet/
  cmd/
    nimlet/          ← single binary: `nimlet server`, `nimlet migrate`

  internal/
    domain/          ← pure domain types (Room, Event, Membership, ...)
    port/
      in/            ← use-case interfaces (AuthService, RoomService, SyncService, ...)
      out/           ← store and bus interfaces (EventStore, MembershipStore, EventBus, ...)
    usecase/         ← business logic
    sync/            ← WaiterRegistry, Dispatcher, Coalescer
    adapter/
      postgres/      ← PostgreSQL adapters
      redis/         ← Redis Pub/Sub event bus
      s3/            ← S3-compatible media store
      filesystem/    ← local media store
    transport/
      http/          ← chi router, handlers, middleware
    config/          ← environment-based configuration
    app/             ← wiring for `nimlet server` / `nimlet migrate`

  migrations/
    postgres/        ← goose SQL migrations (embedded in binary)

  testkit/           ← shared mocks and contract tests
  test/
    integration/     ← end-to-end tests (require Postgres + Redis)
```

---

## Core model

```text
User
  owns one or more Devices

Device
  has one or more Sessions

Room
  has Members
  tracks member_count for large-room routing

Member
  has membership state (invited / joined) and role (owner / admin / member) inside a Room

Event
  belongs to a Room
  has a global stream position
  is visible to selected users (small rooms) or pulled by stream_pos (large rooms)

Sync
  returns visible events after a cursor
```

The most important rule:

```text
events say what happened
memberships say who participates
event visibility says who receives what
```

---

## Event model

A canonical event looks like this:

```json
{
  "event_id": "evt_01HX...",
  "room_id": "room_01HX...",
  "type": "m.room.message",
  "sender_id": "user_01HX...",
  "origin_server_ts": 1777050000000,
  "content": {
    "msgtype": "m.text",
    "body": "hello"
  }
}
```

Events are immutable.

To change what clients display, append another event. Examples: `m.room.redaction`, `m.room.reaction`, `m.room.member.joined`.

Nimlet does not need to understand every event type. It stores, orders, authorizes, and syncs events.

---

## Authentication

Nimlet uses bearer-token authentication.

```http
Authorization: Bearer <access_token>
```

The server resolves identity from the session:

```text
access_token → HMAC verify → session → device → user → namespace
```

The session fixes the namespace for its whole lifetime; no request field, path or header can change it.

Clients must never self-declare sender identity. The server always derives `sender_id` and `device_id` from the authenticated session.

---

## Authorization

Authorization is limited to communication concerns:

```text
Can this authenticated participant read or publish in this room?
```

The basic layers:

1. valid session and HMAC token
2. active device, active user, active namespace
3. room membership exists
4. membership state (must be `join` to publish; `invite` or `join` to receive)
5. room role (only `owner` / `admin` can invite)

---

## Namespaces

One Nimlet instance can serve several independent applications (say, an internal team chat and a customer-facing app).
Each one gets a **namespace**, and namespaces are isolated from each other:

- **No access across namespaces.** No credential, user, room, event, session, AppService or media
  file of one namespace grants access to another.
- **Usernames are unique per namespace.** `alice` can exist in both, as two unrelated accounts.
- **Other namespaces look empty.** Users, rooms and media of another namespace are reported as
  not found (404), exactly as if they did not exist.
- **The database enforces it.** Composite foreign keys make a cross-namespace membership, room or
  session impossible, even through a bug or a direct insert.

Everything that existed before namespaces lives in the `default` namespace, which keeps public
registration on. Clients pick a namespace with the optional `namespace` field of `/login` and
`/register`. Without it they get `default`. The field only *selects* which accounts the username
is looked up in; the password is still verified against that namespace's account:

```json
{"namespace":"acme","username":"alice","password":"s3cr3t","device_name":"iPhone"}
```

Rules:

- **Login failures look the same.** An unknown namespace, an unknown user and a wrong password all
  return 401. A disabled namespace (403 `NAMESPACE_DISABLED`) is only reported after the password
  checks out.
- **Registration is off by default.** New namespaces do not accept public `/register`
  (403 `REGISTRATION_CLOSED`) until an operator enables it. Accounts can also be created with the
  admin CLI.
- **Disabling a namespace stops it at once.** Login, every authenticated request (including
  `/sync`) and its AppServices stop working, and the other namespaces are unaffected.
- **Infrastructure is shared.** Namespaces share Postgres, Redis and the process. This is
  isolation of data and authorization, not of resources or failures.

**Opaque cursors.** Internally, events are ordered by one stream position shared by every
namespace. Exposing it would let a client measure other namespaces' traffic from the gaps
between its own events, so clients never see positions.
- `next_batch` and `since` are opaque tokens: AES-GCM-sealed and bound to the user they were
  issued to.
- Events carry a `room_seq` instead: a per-room number (1, 2, 3 …, with occasional gaps) that
  follows the order `/sync` delivers the room's events in. It reveals nothing a room member
  cannot already see. Order and dedupe a room's events by it; `/messages` pages by it.

---

## API overview

### Register

```http
POST /register
Content-Type: application/json

{"username":"alice","password":"s3cr3t","display_name":"Alice","device_name":"iPhone"}
```

Optional: `"namespace":"<slug>"` (see [Namespaces](#namespaces)). Public registration always
creates a `human` account. Clients cannot declare their type, and any other `user_type` is 400.
Agents, bots and service accounts are created with the admin CLI (`nimlet user create --type`)
or by AppServices.

Limits: username 1–64 characters (no spaces), password ≤ 1024 bytes, display name ≤ 256
characters. `/login` and `/register` are rate-limited per IP, and logins also per account
(429 with `Retry-After`).

```json
{"user_id":"...","device_id":"...","access_token":"..."}
```

### Login

```http
POST /login
Content-Type: application/json

{"username":"alice","password":"s3cr3t","device_name":"iPhone","device_id":"<optional>"}
```

```json
{"session_id":"...","user_id":"...","device_id":"...","access_token":"..."}
```

Keep the returned `device_id` and send it on later logins from the same install. The server
reuses that device (if it is yours and enabled) instead of creating a new one per login.
Sessions expire after 90 days and are swept.

Add `"namespace":"<slug>"` to log in to a namespace other than `default`.

### Logout

```http
DELETE /logout
Authorization: Bearer <token>
```

### Register a push token

```http
PUT /me/push-token
Authorization: Bearer <token>
Content-Type: application/json

{"push_token":"<raw FCM device token>"}
```

Registers the calling device's push token. The device is always the one attached to the
session — clients cannot register a token for another device. See
[Push notifications](#push-notifications) below.

### Create a room

```http
POST /rooms
Authorization: Bearer <token>

{"name":"General"}
```

```json
{"room_id":"...","name":"General","creator_id":"..."}
```

### List my rooms

```http
GET /rooms?limit=200[&after=<next>]
Authorization: Bearer <token>
```

```json
{"rooms":[{"room_id":"...","name":"general","join_rule":"invite_only","state":"join","role":"owner"}],"next":"<room_id>"}
```

Every room you have a membership in, any state, ordered by room ID. `limit` defaults to 200
(max 1000). When `next` is present, pass it as `after` to get the following page.

### Invite a user

```http
POST /rooms/{roomId}/invite
Authorization: Bearer <token>

{"user_id":"..."}
```

### Join a room

```http
POST /rooms/{roomId}/join
Authorization: Bearer <token>
```

### Send an event

```http
PUT /rooms/{roomId}/send/{eventType}/{txnId}
Authorization: Bearer <token>
Content-Type: application/json

{"msgtype":"m.text","body":"hello"}
```

```json
{"event_id":"...","room_seq":42}
```

The content must be a JSON object, at most `NIMLET_HTTP_MAX_BODY_BYTES` (64 KiB by default,
413 above). `eventType` and `txnId` are at most 255 bytes.

`txnId` provides idempotency — retrying the same `txnId` from the same device returns the original event without duplication.

### Paginate room history

```http
GET /rooms/{roomId}/messages?dir=b&limit=50[&from=<room_seq>]
Authorization: Bearer <token>
```

```json
{"start":120,"end":71,"chunk":[...events...]}
```

`from` is a `room_seq` and is exclusive. Directions: `dir=b` (backwards, before `from`) or
`dir=f` (forwards, after it). Without `from`, `dir=b` starts at the newest event and `dir=f` at
the oldest. Pass `end` as the next `from` to keep paging. To fetch the single event with
`room_seq` N, ask for `dir=f&from=N-1&limit=1` and check its `room_seq`.

### Sync events

```http
GET /sync?since=<next_batch>&timeout=30000
Authorization: Bearer <token>
```

Behavior:

Omit `since` on the first call (`since=0` is accepted as the same thing, for clients written
before cursors were opaque). Otherwise pass the previous `next_batch` exactly as received:
it is an opaque token, not a number. A token that is malformed, tampered with or issued to
another user is 400 `INVALID_CURSOR`; the client then starts over without `since`.

1. If visible events exist after `since`, return immediately.
2. Otherwise, hold the request until a new event arrives, the timeout expires, or the client disconnects.
3. Return `next_batch` as the next cursor.
4. The client immediately opens another `/sync`.

```json
{
  "next_batch": "c1.v8Jm…",
  "events": [
    {
      "event_id": "...",
      "room_id": "...",
      "type": "m.room.message",
      "sender_id": "...",
      "origin_server_ts": 1777050001000,
      "content": {"msgtype":"m.text","body":"hello"}
    }
  ]
}
```

On timeout with no new events:

```json
{"next_batch": "c1.Qa2e…", "events": []}
```

### Upload media

```http
POST /media/upload
Authorization: Bearer <token>
Content-Type: multipart/form-data
```

```json
{"media_id":"...","content_type":"image/jpeg","size":123456}
```

### Download media

```http
GET /media/{mediaId}
Authorization: Bearer <token>
```

---

## AppServices

AppServices are external bots and bridges, Matrix-style. They are configured in a YAML file
(`NIMLET_APP_SERVICES_FILE`) that is loaded at startup:

```yaml
app_services:
  - id: acme-bridge
    namespace: acme            # optional, "default" when omitted
    endpoint: https://bridge.internal:9000
    hs_token: <secret Nimlet sends to the bridge>
    as_token: <secret the bridge sends to Nimlet>
    sender_username: acme-bot  # service account provisioned in the namespace at startup
    interested_events: ["m.room.message", "brian.*"]
```

- **One namespace per AppService.** Its token, its sender account and the events it receives all
  belong to that namespace, which must exist and be active at startup.
- **Events are filtered by namespace first.** An AppService never receives another namespace's
  events, whatever `interested_events` says.
- **Delivery is batched and fire-and-forget.** Events go out as `POST {endpoint}/events` with
  `Authorization: Bearer <hs_token>`, in batches of up to 100 events or 5 ms.
- **The token must match the URL.** AppService endpoints live under `/appservice/{asId}/...` and
  authenticate with `as_token`. The `{asId}` in the path must be the AppService that owns the
  token, otherwise the request gets 403.
- **Provisioning stays inside the namespace.**
  - `POST /appservice/{asId}/users` returns a session for a `service` account of the namespace,
    creating it on first use. It refuses usernames that belong to human (or other non-service)
    accounts.
  - `POST /appservice/{asId}/rooms` and `.../rooms/{roomId}/members` create rooms and memberships
    in the namespace.
- **Config errors stop startup.** A duplicate `id`, an `as_token` shared by two entries or an
  unknown namespace is rejected.

---

## Administration CLI

Namespaces, users, rooms and memberships are administered with the `nimlet` binary itself. It
works directly against the database; there is **no HTTP admin API** and no global admin key.
The CLI needs only `NIMLET_ADMIN_DB_DSN`: no HMAC secret, Redis or media store. Use a database
role separate from the server's (see [`docs/operations.md`](docs/operations.md)).

```bash
export NIMLET_ADMIN_DB_DSN="postgres://nimlet_operator:...@db/nimlet"

nimlet namespace create acme
nimlet user create alice -n acme --display-name Alice      # prompts for the password twice
printf '%s\n' "$BOB_PW" | nimlet user create bob -n acme --password-stdin
ROOM=$(nimlet room create general -n acme --owner alice -q)
nimlet room member add "$ROOM" bob -n acme
nimlet room member list "$ROOM" -n acme --output json
```

| Group | Commands |
|---|---|
| `namespace` | `create <slug> [--allow-registration]`, `list`, `show`, `disable <slug> --confirm <slug>`, `enable`, `registration <slug> on\|off` |
| `user` | `create <username> [--display-name] [--type] [--password-stdin]`, `list [--limit] [--after]`, `show`, `disable`, `enable`, `password set`, `session list <username>`, `session revoke <session-id>` |
| `room` | `create <name> --owner <username> [--join-rule] [--alias]`, `list`, `show <room-id>`, `member add <room-id> <username> [--role] [--if-not-exists]`, `member list`, `member remove`, `member role set <room-id> <username> <role>` |

Conventions:

- **The namespace is always explicit.** `user` and `room` commands need `-n/--namespace` (or
  `NIMLET_NAMESPACE`). There is no implicit default, and names (usernames, room IDs) resolve
  only inside that namespace.
- **Output** is human-readable by default. Use `--output json` for scripts, or `-q` to print only
  IDs. Data goes to stdout and errors to stderr.
- **Exit codes:**

  | Code | Meaning |
  |---|---|
  | 0 | ok |
  | 1 | other failure |
  | 2 | invalid argument or flag |
  | 3 | not found |
  | 4 | conflict, e.g. already exists |
  | 5 | not allowed |
  | 6 | database unreachable |

- **Passwords** are never accepted as arguments. They come from an echo-less prompt or from
  `--password-stdin` (the first line of stdin).
- **Membership semantics:**
  - `member add` joins directly, with the same effects as a join (the member counter is updated),
    and is rejected for DM rooms.
  - `member remove` moves the member to `leave` and revokes their access to the room's history
    through `/sync`.
  - The last owner of a room can be neither removed nor demoted.
- **Every mutation is audited** in the `admin_audit` table: operator, command, namespace,
  target, outcome and a per-invocation correlation ID. Passwords and tokens are never recorded.
  The operator is `NIMLET_ADMIN_OPERATOR` when set, otherwise `user@host`.

---

## Sync model

```text
Sync(user, since, timeout):
  register waiter for user + user's large rooms   (before querying: no lost wake-ups)

  events = next page after `since`, up to the read ceiling, from
             event_visibility (small rooms, DMs)
             ∪ events of every joined large room
  if events not empty:
    return immediately

  wait until notified or timeout or disconnect
  re-query and return
```

**Ordering guarantee.** A client's cursor (opaque to it) seals the last position it received, so an event must
never become visible after one with a higher position. Sends get their position from a short,
ordered allocation that also writes a marker into `stream_inflight`, and `/sync` never returns or
moves past the lowest position still in flight (the *read ceiling*). Concurrent sends can commit in
any order without a client ever skipping one. Markers left by a crashed writer are ignored after
30 s.

Core rule:

```text
PostgreSQL provides consistency.
Redis provides low-latency wake-up.
The client cursor provides recovery.
```

---

## Large room architecture

Rooms with 1000+ members use a different fan-out strategy to avoid O(N) writes per message.

**Small rooms (< 1000 members):**
- On send: insert one row per member into `event_visibility`
- On sync: query `event_visibility JOIN events`
- Wake-up: `bus.PublishBatch(userIDs)` — one Redis Pipeline round-trip

**Large rooms (≥ 1000 members):**
- On send: insert only the event; no `event_visibility` rows written
- On sync: pull from `events` by `room_id + stream_pos > since` for each joined large room
- Wake-up: `bus.Publish("room:" + roomID)` — single publish regardless of member count

The threshold is 1000 members. The `rooms.member_count` column is incremented atomically when a user joins.

---

## Redis Pub/Sub wake-up

In a multi-instance deployment, Redis Pub/Sub wakes `/sync` waiters across instances.

Channels are partitioned using FNV-1a:

```text
channel = "nimlet:sync:" + (fnv32a(key) % partitions)
```

Where `key` is either a `userID` (small rooms) or `"room:"+roomID` (large rooms).

Each `/sync` waiter subscribes to the partition channel for its user. The Dispatcher goroutine bridges Redis messages to the in-memory `WaiterRegistry`.

Default partition count: 256. Tune with `NIMLET_SYNC_PARTITIONS`.

If Redis is unavailable, clients experience up to `timeout` ms latency before polling. No events are lost.

---

## Push notifications

For clients that aren't actively polling `/sync` (app backgrounded or killed), Nimlet can
deliver a mobile push notification via FCM HTTP v1, authenticated directly with a Firebase
service-account key — no third-party push relay in the delivery path.

**Presence.** Every `/sync` call touches an ephemeral Redis key
(`presence:{userID}`, TTL 90s) marking that user as actively connected. A message send only
queues a push for recipients who are *not* currently present — anyone with an open `/sync`
already gets the event live. Like the wake-up bus, presence is best-effort: a lookup failure
or an expired key fails open (send the push anyway), never silent (skip a push that should
have fired).

**Per-person batching.** Pushes are batched per recipient, not per event — someone with unread
messages across several rooms gets one push, not one per message. Each send appends to a
Redis-backed pending list for that recipient and races to claim (`SET NX`) a 5-second flush
window; only the instance that wins the claim schedules the flush, so multiple server
instances handling different sends for the same recipient still produce exactly one push, not
one per instance. The flush reads the full pending list, checks presence one last time, and
delivers a single digest.

**Payload.** The digest is metadata only — room IDs, room names, and per-room counts — never
message content:

```json
{"type":"digest","rooms":[{"room_id":"...","room_name":"...","count":3}],"total_count":4}
```

No message body ever passes through FCM/Google's servers. The push exists to wake the client
up; real content is always fetched via `/sync` once the app opens.

Configure `NIMLET_FCM_CREDENTIALS_FILE` to enable delivery (unset means push is a no-op —
tokens are still accepted and stored, they're just never used). Currently Android-only.

---

## PostgreSQL schema (simplified)

```sql
CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  display_name TEXT NOT NULL,
  user_type TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id),
  name TEXT NOT NULL,
  push_token TEXT,          -- raw FCM device token; NULL until the client registers one
  created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id),
  device_id TEXT NOT NULL REFERENCES devices(id),
  token_hash TEXT UNIQUE NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ
);

CREATE TABLE rooms (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  creator_id TEXT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL,
  member_count INT NOT NULL DEFAULT 0   -- drives large-room routing
);

CREATE TABLE room_memberships (
  room_id TEXT NOT NULL REFERENCES rooms(id),
  user_id TEXT NOT NULL REFERENCES users(id),
  state TEXT NOT NULL,      -- 'invite' | 'join'
  role TEXT NOT NULL,       -- 'owner' | 'admin' | 'member'
  updated_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (room_id, user_id)
);

-- Global sequence for total ordering across partitions
CREATE SEQUENCE events_stream_pos_seq;

CREATE TABLE events (
  stream_pos BIGINT PRIMARY KEY DEFAULT nextval('events_stream_pos_seq'),
  id TEXT UNIQUE NOT NULL,
  room_id TEXT NOT NULL,
  sender_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  content JSONB NOT NULL,
  txn_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

-- Per-user visibility for small rooms and direct messages
CREATE TABLE event_visibility (
  user_id TEXT NOT NULL,
  stream_pos BIGINT NOT NULL,
  PRIMARY KEY (user_id, stream_pos)   -- covering index for /sync hot path
);
```

The `/sync` query (simplified; see `EventStore.GetVisible`). Each source is an ordered index scan
limited *before* merging, so a page costs O(limit × sources) whatever the history size:

```sql
WITH ceiling AS (SELECT coalesce(min(pos) - 1, max_bigint) AS pos FROM stream_inflight ...),
page AS (
  (SELECT stream_pos FROM event_visibility
   WHERE user_id = $1 AND stream_pos > $2 AND stream_pos <= (SELECT pos FROM ceiling)
   ORDER BY stream_pos LIMIT $3)
  UNION
  (SELECT x.stream_pos FROM room_memberships m JOIN rooms r ON r.id = m.room_id
   CROSS JOIN LATERAL (SELECT stream_pos FROM events
                       WHERE room_id = m.room_id AND stream_pos > $2 AND stream_pos <= (SELECT pos FROM ceiling)
                       ORDER BY stream_pos LIMIT $3) x
   WHERE m.user_id = $1 AND m.state = 'join' AND r.member_count >= 1000)
  ORDER BY 1 LIMIT $3)
SELECT e.*, u.display_name FROM page JOIN events e USING (stream_pos) JOIN users u ON u.id = e.sender_id
ORDER BY stream_pos;
```

---

## Idempotency

Clients send events with a transaction ID:

```http
PUT /rooms/{roomId}/send/{eventType}/{txnId}
```

The idempotency key is `deviceID + ":" + txnId`. If a client retries after a network error, Nimlet returns the previously created event without duplication. The key is stored in `event_idempotency` with the `stream_pos` of the original event.

---

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `NIMLET_HTTP_ADDR` | `:8080` | HTTP listen address |
| `NIMLET_PPROF_ADDR` | | If set, serves `net/http/pprof` on this separate address (bind to localhost/private) |
| `NIMLET_METRICS_ADDR` | | If set, serves `/metrics` on this separate address instead of the API port |
| `NIMLET_HTTP_MAX_BODY_BYTES` | `65536` | Max JSON request body, event content included (413 above) |
| `NIMLET_MEDIA_MAX_BYTES` | `52428800` | Max size of one media upload (413 above) |
| `NIMLET_AUTH_RATE_PER_MIN` | `30` | `/login` + `/register` requests per minute per client IP (429 above) |
| `NIMLET_HTTP_TRUST_PROXY_HEADERS` | `false` | Take the client IP from `X-Forwarded-For`/`X-Real-IP`; enable only behind a proxy that sets them |
| `NIMLET_DB_DSN` | *(required)* | PostgreSQL connection string |
| `NIMLET_AUTO_MIGRATE` | `true` | Apply pending migrations at server startup; set `false` when the server's DB role has no DDL rights |
| `NIMLET_DB_MAX_CONNS` | `max(50, NumCPU×4)` | Max DB pool connections per instance |
| `NIMLET_AUTH_HMAC_SECRET` | *(required, ≥32 chars)* | Secret for HMAC token signing |
| `NIMLET_REDIS_ADDR` | `localhost:6379` | Redis address |
| `NIMLET_REDIS_PASSWORD` | | Redis password |
| `NIMLET_REDIS_DB` | `0` | Redis database index |
| `NIMLET_SYNC_PARTITIONS` | `256` | Pub/Sub partition count |
| `NIMLET_MEDIA_DRIVER` | `filesystem` | `filesystem` or `s3` |
| `NIMLET_MEDIA_PATH` | `./media` | Local media directory (filesystem driver) |
| `NIMLET_S3_BUCKET` | | S3 bucket name (s3 driver) |
| `NIMLET_S3_PATH_STYLE` | `false` | Path-style bucket addressing, needed by most S3-compatible stores (MinIO, SeaweedFS, Garage); set their endpoint with the AWS SDK's `AWS_ENDPOINT_URL_S3` |
| `NIMLET_FCM_CREDENTIALS_FILE` | | Path to a Firebase service-account JSON key; unset disables push delivery |

Integer variables must parse: a typo such as `NIMLET_DB_MAX_CONNS=abc` stops startup instead
of silently using the default.

Admin CLI only (never set these on the server): `NIMLET_ADMIN_DB_DSN` (required), `NIMLET_NAMESPACE`
(default for `-n`), `NIMLET_ADMIN_OPERATOR` (identity recorded in the audit log). Database roles,
Kubernetes and the threat model: [`docs/operations.md`](docs/operations.md).

**Multi-instance note:** set `NIMLET_DB_MAX_CONNS` to `max_postgres_connections / num_instances` to avoid exhausting the Postgres connection limit.

### Metrics

Prometheus metrics are served on `GET /metrics`:

| Metric | Meaning |
|---|---|
| `nimlet_http_requests_total{method,path,status}` / `nimlet_http_request_duration_seconds{method,path}` | Per route; `path` is the route pattern (`/rooms/{roomId}/...`), never the raw URL |
| `nimlet_sync_active_connections` | Open `/sync` long-polls |
| `nimlet_db_pool_empty_acquires_total`, `nimlet_db_pool_acquire_wait_seconds_total` | Requests queueing for a DB connection; a rising rate means the pool or the database is saturated |
| `nimlet_db_pool_acquired_connections`, `nimlet_db_pool_max_connections` | Pool usage |
| `nimlet_bus_dropped_notifications_total` | Wake-ups dropped by a full subscriber buffer (adds latency, never loses events) |
| `nimlet_events_sent_total`, `nimlet_rooms_created_total`, `nimlet_users_registered_total` | Business counters |

Benchmarks and how to run them: [`docs/benchmarks.md`](docs/benchmarks.md), [`bench/README.md`](bench/README.md).

---

## Quick start

Start PostgreSQL and Redis:

```bash
docker run --rm --name nimlet-postgres \
  -e POSTGRES_USER=nimlet \
  -e POSTGRES_PASSWORD=nimlet \
  -e POSTGRES_DB=nimlet \
  -p 5432:5432 \
  postgres:16

docker run --rm --name nimlet-redis -p 6379:6379 redis:7
```

Start the server (migrations run automatically at startup):

```bash
export NIMLET_DB_DSN="postgres://nimlet:nimlet@localhost:5432/nimlet?sslmode=disable"
export NIMLET_AUTH_HMAC_SECRET="$(openssl rand -hex 32)"

go run ./cmd/nimlet server
```

Register and send a message:

```bash
# Register
TOKEN=$(curl -s -X POST http://localhost:8080/register \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret","display_name":"Alice","device_name":"cli"}' \
  | jq -r .access_token)

# Create a room
ROOM=$(curl -s -X POST http://localhost:8080/rooms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"general"}' | jq -r .room_id)

# Send an event
curl -X PUT "http://localhost:8080/rooms/$ROOM/send/m.room.message/txn1" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"msgtype":"m.text","body":"hello"}'

# Sync
curl "http://localhost:8080/sync?timeout=30000" \
  -H "Authorization: Bearer $TOKEN"
```

---

## Scaling model

```text
1 instance:
  PostgreSQL + in-memory waiter wake-up (no Redis needed)

multiple instances:
  PostgreSQL + Redis Pub/Sub wake-up

media-heavy deployment:
  PostgreSQL + Redis + S3-compatible object storage
```

PostgreSQL is always the source of truth. Redis improves latency, not correctness.

For horizontal scaling, each instance subscribes to all `NIMLET_SYNC_PARTITIONS` channels. Notifications received by instances without matching local waiters are silently discarded — this is correct Pub/Sub fan-out behavior. Future work: Redis Streams with consumer groups to avoid duplicate processing.

---

## Running tests

```bash
# Unit tests + race detector
go test -race -count=1 ./...

# Integration tests (requires running Postgres and Redis)
export TEST_POSTGRES_DSN="postgres://nimlet:nimlet@localhost:5432/nimlet_test?sslmode=disable"
export TEST_REDIS_ADDR="localhost:6379"
go test -tags integration -race -count=1 ./test/integration/...
```

---

## Roadmap

### Implemented

- PostgreSQL-backed users, devices, sessions
- private rooms with invites
- room memberships with roles (owner / admin / member)
- send event API with idempotent transaction IDs
- event visibility read model
- `/sync` with long polling and cursor-based catch-up
- message history pagination
- basic media upload/download (filesystem + S3)
- HMAC bearer-token auth
- Redis Pub/Sub cross-instance wake-up (partitioned, 256 channels)
- large-room fan-out (≥ 1000 members: single room-level publish, pull-based sync)
- database migrations (embedded, auto-applied at startup)
- push notifications (FCM HTTP v1, presence-gated, per-recipient batched digests — Android)
- isolated namespaces (multi-tenant on one instance; namespace-scoped login, AppServices and media)
- admin CLI (`nimlet namespace|user|room`): direct DB access, audited, no HTTP admin API

### Next

- redactions
- reactions
- read receipts
- Docker Compose for local development
- Go client package

### Later

- richer moderation model
- Redis Streams (replace Pub/Sub for more efficient multi-instance processing)
- SDKs for common platforms
- observability dashboards

---

## Status

Nimlet is early-stage software. The protocol and implementation are still evolving. Expect breaking changes before the first stable release.

---

## License

[Apache License 2.0](LICENSE).
