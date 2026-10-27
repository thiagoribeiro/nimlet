# Nimlet benchmarks

How to run: [`bench/README.md`](../bench/README.md). Raw outputs: `bench/results/<phase>/`.

**Environment (all runs):** AMD Ryzen AI 5 330 (8 threads), 14 GB RAM, Linux. Postgres 16.15 (18 in phase 04) and Redis 7
in rootless podman (vfs storage driver, so expect slower Postgres I/O than bare metal). Go benchmarks
show the median of 6 runs. Load-test client and server share the machine.

Compare **phases against each other**, not against other hardware.

## 00 — baseline (`bench/results/00-baseline`)

Commit `193b262`: the rebranded code before any tuning.

### Store and registry micro-benchmarks

| Benchmark | time/op | B/op | allocs/op |
|---|---:|---:|---:|
| `EventStore_Create/small_members=10` | 829.4 µs | 7794 | 165 |
| `EventStore_Create/small_members=200` | 3.52 ms | 189235 | 2459 |
| `EventStore_Create/large` | 457.6 µs | 1223 | 31 |
| `EventStore_GetVisible/large_rooms=0/initial` | 20.59 ms | 54138 | 915 |
| `EventStore_GetVisible/large_rooms=0/incremental` | 4.85 ms | 445 | 5 |
| `EventStore_GetVisible/large_rooms=5/initial` | 23.32 ms | 54263 | 923 |
| `EventStore_GetVisible/large_rooms=5/incremental` | 4.87 ms | 581 | 13 |
| `EventStore_GetVisible/large_rooms=50/initial` | 45.39 ms | 61167 | 974 |
| `EventStore_GetVisible/large_rooms=50/incremental` | 4.90 ms | 7445 | 64 |
| `MembershipStore_ListLargeRooms/large_rooms=0` | 45.4 µs | 387 | 4 |
| `MembershipStore_ListLargeRooms/large_rooms=50` | 64.8 µs | 5339 | 161 |
| `AuthLookup` | 47.8 µs | 1755 | 29 |
| `Registry_Churn/waiters=10` | 164 ns | 120 | 2 |
| `Registry_Churn/waiters=1000` | 435 ns | 120 | 2 |
| `Registry_Churn/waiters=10000` | 2.5 µs | 120 | 2 |
| `Registry_ChurnOldest` | 1.4 µs | 120 | 2 |
| `Registry_NotifyRoom/waiters=10` | 338 ns | 80 | 1 |
| `Registry_NotifyRoom/waiters=1000` | 31.6 µs | 8192 | 1 |
| `Registry_NotifyRoom/waiters=10000` | 330.1 µs | 81920 | 1 |
| `Registry_NotifyUserParallel` | 17 ns | 8 | 1 |

Fixture: 5k users. 40 small rooms (50 members, 500 events each), giving 20k events and 1M
`event_visibility` rows. 50 large rooms with 1k events each. Readers belong to every small room plus
0, 5 or 50 large rooms. `incremental` means a caught-up client asking for anything after the last
~50 positions, which returns nothing: the query every long-poll runs before it blocks.

### End-to-end (`bench/loadgen`)

Scenarios marked *saturating* keep every listener long-polling in a tight loop. They measure capacity
under the wake-up herd, not the latency a user sees.

| Scenario | delivered | send p50 / p99 | delivery p50 / p99 |
|---|---:|---:|---:|
| 200 members, 200 listeners, 20 msg/s | 100% | 26.4 / 89.0 ms | 46.9 / 131.9 ms |
| chat: 50 members, 50 listeners, 100 msg/s | **99.22%** | 16.6 / 41.2 ms | 29.4 / 59.2 ms |
| *saturating* small room: 200 members, 200 listeners, 200 msg/s | **90.66%** | 226 / 302 ms | 272 / 360 ms |
| *saturating* large room: 1100 members, 300 listeners, 50 msg/s | **99.71%** | 155 / 207 ms | 203 / 285 ms |

### Findings

1. **Events are lost under concurrent sends (correctness).** The DB holds every event and every
   visibility row, but clients skip ~9% (small room) / ~0.3% (large room) for good.
   - `stream_pos` comes from `nextval()` before the inserts, and the rows commit in
     whatever order the sends finish.
   - `/sync` returns `next_batch` = the highest position it saw.
   - So an event with a *lower* position that commits a moment later lands behind the
     client's cursor.
   - The send path is not even transactional: `EventStore.Create` runs four autocommit
     statements, so the event row is visible before its visibility rows.
2. **Generic-plan trap on the hot `/sync` query.**
   - pgx prepares statements, and after 5 executions Postgres may switch to a generic plan
     that cannot use the `stream_pos > $since` selectivity.
   - With `plan_cache_mode=force_custom_plan` the `incremental` case drops from **4.85 ms to
     0.45 ms** (10×). That case is the query every long-poll runs.
3. **Initial sync sorts everything.**
   - The `UNION ALL ... ORDER BY stream_pos LIMIT 100` has no per-branch limit.
   - EXPLAIN (`explain-getvisible-initial.txt`) shows it reads all 20k visibility rows plus
     81k large-room rows, then a top-N sort.
   - 20–45 ms for 100 rows.
4. **Send fan-out is N statements.** One `INSERT ... ON CONFLICT` per member in a `pgx.Batch`
   (200 members: 3.5 ms, 2.4k allocs).
5. **Wake-up storm.**
   - Every message wakes every listener, and each one runs ~5 round trips: auth with 2
     queries, presence, `ListLargeRooms`, `GetVisible`.
   - 64k `/sync` calls in 30 s for 200 listeners.
   - Server CPU stays around 1.2 of 8 cores; the database round trips are the bottleneck.
6. **Registry churn is O(waiters).** Register/unregister in a room with 10k waiters costs 2.5 µs,
   against 164 ns with 10 waiters, because `removeChan` scans linearly.

7. **Metrics label cardinality (found during tuning).** `nimlet_http_*` used the raw URL path as a
   label, so every send created new time series. Memory grew without bound, and `/metrics` reached
   1.3 MB after one 20 s run.

## 01 — tuning (`bench/results/01-tuning`)

| Fix | Finding |
|---|---|
| Ordered stream allocation: `stream_inflight` plus a read ceiling. Create runs as one atomic CTE statement and fans out with `unnest` | 1, 4 |
| Per-branch ordered `LIMIT` in `GetVisible`, which also makes the generic plan fast | 2, 3 |
| `GetVisible` resolves joined large rooms in SQL, in the same snapshot | 5, plus a stale-list race |
| `/sync` registers its waiter before the first query; the large-room list for wake-ups is cached for 10 s | 5, plus a lost-wake-up race |
| Auth reads session and device in one query | 5 |
| `WaiterRegistry` uses sets instead of slices; notify sends under the lock without copying | 6 |
| Metrics labelled by chi route pattern; new pool and bus-drop metrics | 7 |
| Pipelined push enqueue and atomic flush; precomputed bus channel names; dropped 3 unused indexes | — |

### Micro-benchmarks, baseline → tuned

| Benchmark | baseline | tuned | speedup | allocs |
|---|---:|---:|---:|---:|
| `EventStore_Create/small_members=10` | 829.4 µs | 470.5 µs | 1.8× | 165 → 103 |
| `EventStore_Create/small_members=200` | 3.52 ms | 2.23 ms | 1.6× | 2459 → 301 |
| `EventStore_Create/large` | 457.6 µs | 337.7 µs | 1.4× | 31 → 91 |
| `EventStore_GetVisible/large_rooms=0/initial` | 20.59 ms | 457.1 µs | 45.1× | 915 → 916 |
| `EventStore_GetVisible/large_rooms=0/incremental` | 4.85 ms | 179.5 µs | 27.0× | 5 → 6 |
| `EventStore_GetVisible/large_rooms=5/initial` | 23.32 ms | 530.1 µs | 44.0× | 923 → 916 |
| `EventStore_GetVisible/large_rooms=5/incremental` | 4.87 ms | 165.3 µs | 29.4× | 13 → 6 |
| `EventStore_GetVisible/large_rooms=50/initial` | 45.39 ms | 1.07 ms | 42.4× | 974 → 916 |
| `EventStore_GetVisible/large_rooms=50/incremental` | 4.90 ms | 273.5 µs | 17.9× | 64 → 6 |
| `MembershipStore_ListLargeRooms/large_rooms=0` | 45.4 µs | 44.9 µs | 1.0× | 4 → 5 |
| `MembershipStore_ListLargeRooms/large_rooms=50` | 64.8 µs | 63.6 µs | 1.0× | 161 → 162 |
| `AuthLookup` | 47.8 µs | 25.1 µs | 1.9× | 29 → 16 |
| `Registry_Churn/waiters=10` | 164 ns | 222 ns | 0.7× | 2 → 3 |
| `Registry_Churn/waiters=1000` | 435 ns | 236 ns | 1.8× | 2 → 3 |
| `Registry_Churn/waiters=10000` | 2.5 µs | 266 ns | 9.4× | 2 → 3 |
| `Registry_ChurnOldest` | 1.4 µs | 354 ns | 4.0× | 2 → 3 |
| `Registry_NotifyRoom/waiters=10` | 338 ns | 354 ns | 1.0× | 1 → 0 |
| `Registry_NotifyRoom/waiters=1000` | 31.6 µs | 32.8 µs | 1.0× | 1 → 0 |
| `Registry_NotifyRoom/waiters=10000` | 330.1 µs | 321.6 µs | 1.0× | 1 → 0 |
| `Registry_NotifyUserParallel` | 17 ns | 17 ns | 0.9× | 1 → 0 |

Notes on the table:

- `GetVisible` now also resolves the reader's large rooms, which used to be a separate
  `ListLargeRooms` round trip of 45–65 µs on every `/sync`. That query now runs only when the
  10 s wake-up cache misses.
- `Create/large` allocates more (the CTE statement) but saves round trips.
- `Registry_Churn/waiters=10` is ~60 ns slower because a set is allocated per key. That buys
  constant cost at 10k waiters.

### End-to-end

| Scenario | delivered | send p50 / p99 | delivery p50 / p99 |
|---|---:|---:|---:|
| 200 members, 200 listeners, 20 msg/s | 100% | **8.9** / 35.7 ms (3.0×) | **32.5** / 62.7 ms |
| chat: 50 members, 50 listeners, 100 msg/s | **100%** | **4.3** / 15.6 ms (3.8×) | **16.1** / 32.1 ms |
| *saturating* small room, 200 msg/s | **100%** | 104 / 164 ms | 130 / 207 ms |
| *saturating* large room, 50 msg/s | **100%** | 40 / 105 ms | 73 / 146 ms |

### Where the saturating scenarios are bound now

Pool metrics during the saturating small-room run:

- 99% of connection acquires found the pool empty.
- Postgres used ~1.4 cores and the server ~1.3. Postgres, the server and the load generator share
  one laptop, and rootless-podman networking adds latency to every query.
- Raising `NIMLET_DB_MAX_CONNS` to 100 made p50 and p99 *worse* (120 / 366 ms). The limit is the
  shared machine, not the pool size, so the default stays at 50.

The remaining cost is structural, the wake-up herd. Every message wakes every listener in the room,
and each listener runs its own `GetVisible`. The next lever would be a per-room shared read: one
query serving every waiter of a large room. That is a separate design change and is not done here.

## 02 — namespaces (`bench/results/02-namespaces`)

Namespaces add no column, filter or join to the `/sync`, send or idempotency paths. Isolation
comes from creating relations only inside a namespace, enforced by composite foreign keys. The only
hot-path change is authentication: every request now also checks that the user and the namespace
are enabled.

### Micro-benchmarks, 01 → 02

| Benchmark | 01 tuning | 02 namespaces | speedup | allocs |
|---|---:|---:|---:|---:|
| `EventStore_Create/small_members=10` | 470.5 µs | 488.3 µs | 1.0× | 103 → 103 |
| `EventStore_Create/small_members=200` | 2.23 ms | 2.32 ms | 1.0× | 301 → 301 |
| `EventStore_Create/large` | 337.7 µs | 354.9 µs | 1.0× | 91 → 91 |
| `EventStore_GetVisible/large_rooms=0/initial` | 457.1 µs | 466.8 µs | 1.0× | 916 → 916 |
| `EventStore_GetVisible/large_rooms=0/incremental` | 179.5 µs | 184.8 µs | 1.0× | 6 → 6 |
| `EventStore_GetVisible/large_rooms=5/initial` | 530.1 µs | 546.2 µs | 1.0× | 916 → 916 |
| `EventStore_GetVisible/large_rooms=5/incremental` | 165.3 µs | 170.4 µs | 1.0× | 6 → 6 |
| `EventStore_GetVisible/large_rooms=50/initial` | 1.07 ms | 1.11 ms | 1.0× | 916 → 916 |
| `EventStore_GetVisible/large_rooms=50/incremental` | 273.5 µs | 271.2 µs | 1.0× | 6 → 6 |
| `MembershipStore_ListLargeRooms/large_rooms=0` | 44.9 µs | 44.9 µs | 1.0× | 5 → 5 |
| `MembershipStore_ListLargeRooms/large_rooms=50` | 63.6 µs | 63.2 µs | 1.0× | 162 → 162 |
| `AuthLookup` | 25.1 µs | 40.8 µs | 0.6× | 16 → 17 |

- Sync and send paths are within ±4% (noise).
- **`AuthLookup` +16 µs** (25 → 41 µs): the session query now also joins `users` and
  `namespaces`. This is accepted on purpose. It is what makes disabling a user or a namespace
  revoke its sessions on the next request; before, a disabled user was only stopped at login.
  If it ever matters, cache namespace status in-process for a few seconds.

### End-to-end, A/B in identical conditions

Both binaries were run back to back on fresh infrastructure. The machine was noisier than during
phase 01: the same tuning binary measured 20.4 ms here against 8.9 ms then. Compare the two
columns with each other, not with earlier tables.

| Scenario | send p50: 01 → 02 | delivery p50: 01 → 02 | delivered |
|---|---:|---:|---:|
| 200 members, 200 listeners, 20 msg/s | 20.4 → 24.6 ms | 42.5 → 46.6 ms | 100% / 100% |
| chat: 50 members, 50 listeners, 100 msg/s | 5.2 → 5.7 ms | 16.8 → 16.1 ms | 100% / 100% |

The 200-listener herd scenario makes about 3.8k authenticated `/sync` calls per second, so it is
where the auth join shows up (+10–20%, within this run's noise). The chat scenario is flat.

## 03 — review fixes (`bench/results/03-review`)

These are the security and scale fixes from the code review: membership state machine, request
limits, opaque cursors, partition maintenance, visibility fan-out inside the insert, packed
wake-ups and batched push. All four load scenarios still deliver 100%. Store micro-benchmarks
moved by ±5% (noise).

| Scenario | send p50 / p99 | delivery p50 / p99 |
|---|---:|---:|
| 200 members, 200 listeners, 20 msg/s | 4.9 / 18.3 ms | 25.8 / 44.3 ms |
| chat: 50 members, 50 listeners, 100 msg/s | 3.6 / 12.3 ms | 15.9 / 28.8 ms |
| *saturating* small room, 200 msg/s | 56.6 / 109.7 ms | 74.5 / 137.3 ms |
| *saturating* large room, 50 msg/s | 26.5 / 73.6 ms | 53.7 / 107.4 ms |

What the numbers do not show:

- **Wake-up messages.** PUBLISH calls for 200 sends to a 200-member room went from 40,000 to
  200. Each instance now receives one message per send instead of one per member, which matters
  more the more instances share Redis.
- **Partition count matters under load.**
  - A first version of partition maintenance kept three blocks ahead (5 partitions per table
    instead of 3).
  - In the 200-member scenario that raised send p50 from ~6ms to ~24ms on PostgreSQL 16 and on
    18. The cause was lock-manager contention: every `/sync` touches every partition.
  - A single-threaded `GetVisible` did not slow down, so only concurrency exposes it.
  - Maintenance now keeps one block ahead. The long-term fix (retention of old blocks) is an
    open item in `docs/operations.md`.
- **SQL fan-out is latency-neutral.** In interleaved A/B runs it measured 5.9ms vs 5.8ms send
  p50. It saves a round trip and the member-list transfer, and fixes a membership race.

## 04 — horizontal (`bench/results/04-horizontal`)

Branch `bench/horizontal`. This phase checks whether Nimlet scales out, and how it behaves when
instances join, leave or fail. Harness: `bench/cluster.sh` and `bench/horizontal.sh`
([`bench/README.md`](../bench/README.md#4-cluster-horizontal-scaling-elasticity-failures)).

**Environment.** Same laptop. Everything is pinned to its own cores:

| Component | Cores |
|---|---|
| Postgres 18 (`max_locks_per_transaction=256`, data dir in `/dev/shm`) | 0-2 |
| HAProxy (consistent hash on `Authorization`, so a user's requests stick to one instance) | 3 |
| loadgen | 4, 7 |
| Redis 7 | 7 |
| Nimlet instances (host networking, CFS quota with a 10ms period) | 5-6 |

All instances share the same two cores, so adding instances never adds CPU. What these runs show
is the cost of splitting the same CPU across more processes, and where the shared Postgres stops
the curve. They can't show absolute scale-out on more hardware.

**Workloads.**
- *sync-heavy*: 2000 users in 100 rooms of 20, all long-polling, 100 senders. Every send wakes
  20 `/sync` calls, so most of the cost lands on the instances.
- *send-heavy*: 200 senders in 20 rooms, one listener per room. Most of the cost lands on
  Postgres' serialised position allocation.

A step lasts 20s after a 10s warm-up. It passes when it reaches ≥98% of its rate, has no failed
send, delivers everything, and keeps delivery p99 under 500ms. *Capacity* is the highest passing
step. Every run starts from an empty database. Three repetitions give the median (min–max).

### Code changes in this phase

Measured on sync-heavy with one unlimited instance. The phase-03 code passed **200 msg/s**; this
branch passes **2500 msg/s** (50k deliveries/s).

| Commit | Change |
|---|---|
| `perf(events)` | Sends are group-committed: concurrent sends share one transaction for position allocation and one for the insert, instead of one WAL fsync each. Concurrent `/sync` reads run as one `unnest` + `LATERAL` statement. Inserts into `event_visibility` dropped `ON CONFLICT DO NOTHING`, which cost 4× on the partitioned table. |
| `perf(auth)` | Session lookups are cached for 1s (`NIMLET_AUTH_CACHE_TTL_MS`). A revoked token stops working on the other instances within a second. |
| `feat(config)` | `NIMLET_PROFILE=small\|medium\|large` sizes batch waits, workers and pool per instance size (see [Sizing](../README.md#sizing)). |
| `fix(sync)` | Waiting long-polls are released on shutdown (see elasticity below). |

Two correctness bugs surfaced on the way and were fixed before these runs:
- **Wake-ups lost behind the read ceiling.** When an event was still hidden by another batch's
  in-flight position, the woken reader found nothing and slept its full 30s timeout. It now waits
  for the ceiling to pass.
- **Grouped reads failing silently.** A table alias collision made the grouped read fail and fall
  back to one query per reader, which throttled `/sync`.

### Profiles on the same two cores

| Profile | Instances × CPU | Capacity msg/s | send p50 / p99 ms | delivery p50 / p99 ms | lost / dup / reordered |
|---|---|---:|---:|---:|---:|
| `small` | 8 × 0.25 | 1600 | 128 / 334 | 210 / 478 | 0 / 0 / 0 |
| `medium` | 2 × 1 | 2500 (2500–3000) | 62 / 210 | 137 / 335 | 0 / 0 / 0 |
| `large` | 1 × 2 (no quota) | 2500 | 183 / 312 | 253 / 417 | 0 / 0 / 0 |

Latency is at each run's capacity step. Splitting two cores into eight quarter-CPU instances
keeps 64% of the single instance's throughput; splitting into two halves loses nothing.

### Scalability: 1, 2, 4, 8 instances of 0.25 CPU

| N | sync-heavy capacity | × N=1 | send-heavy capacity | × N=1 | Postgres busy at capacity (sync / send) |
|---:|---:|---:|---:|---:|---:|
| 1 | 150 | 1.0 | 1600 (1600–2000) | 1.0 | 20% / 29% |
| 2 | 300 | 2.0 | 3000 (2500–3000) | 1.9 | 36% / 51% |
| 4 | 1300 (1000–1300) | 8.7 | 4000 (4000–5000) | 2.5 | 69% / 87% |
| 8 | 1600 | 10.7 | 5000 | 3.1 | 96% / 99% |

At a fixed 150 msg/s, sync-heavy delivery p99 fell from 242ms (N=1) to 53ms (N=8). At a fixed
1600 msg/s, send-heavy delivery p99 fell from 131ms to 56ms. Loss, duplication and reordering
were 0 in all 24 runs (25M deliveries).

- **sync-heavy scales with instances until Postgres saturates.** It grows faster than linearly
  up to N=4: one quarter-CPU instance spends most of its quota on long-poll bookkeeping, and
  each added instance brings more group-commit and grouped-read batching. At N=8 Postgres is at
  96% of its three cores, and that is the ceiling.
- **send-heavy is bound by Postgres from the start.** Instances mostly forward sends to Postgres.
  Throughput follows Postgres CPU and stops at about 5000 msg/s when it reaches 99%.
- Past that point the next step is a larger Postgres, not more instances.

### Elasticity: 2 → 3 → 4 → 3 → 2 instances at 180 msg/s

Instances join and leave once a minute under constant sync-heavy load. *drain* takes an instance
out of HAProxy before SIGTERM; *no drain* just sends SIGTERM.

| Event | drain: delivery p99 max | no drain: delivery p99 max | no drain, before `fix(sync)` |
|---|---:|---:|---:|
| start 3 | 130ms | 158ms | 147ms |
| start 4 | 124ms | 85ms | 79ms |
| stop 4 | 77ms | 202ms | **29,474ms** |
| stop 3 | 142ms | 343ms | **29,475ms** |

Both modes completed with no failed send, no sync error, and nothing lost, duplicated or
reordered. A new instance receives its fair share of requests within 1–2s.

Before the fix, SIGTERM stopped the instance's Redis subscriptions, but its open long-polls kept
waiting for a wake-up that could no longer arrive. The graceful shutdown then held those clients
(about a quarter of the users) for the whole 30s long-poll timeout. Now the long-polls answer at
once and the clients re-poll another instance. Raw run: `elastic-before-fix/`.

### Redistribution and reconnection

Nimlet has no rendezvous or discovery: instances are stateless and every instance subscribes to
all Redis partitions. At 20 msg/s, an instance was added and removed (drained) five times, with
long-poll timeouts of 30s and of 5s. If a long-poll could land on a new instance before its
subscriptions were up, the run would show deliveries as slow as the timeout.

| Long-poll timeout | Delivery max in the 30s after each start (5 cycles) | LB fair share after | sync errors |
|---|---|---:|---:|
| 30s | 63–80ms | 1–3s | 0 |
| 5s | 51–112ms | 1–2s | 0 |

No missed wake-up showed up. HAProxy's 1s health check only sends traffic once the instance
listens. The subscriptions start just before the listener opens, but nothing waits for Redis to
confirm them, so a narrow race remains possible in principle; these runs never hit it.

### Failures at 400 msg/s on 4 instances

| Fault | Failed sends | Send retries | Sync errors | Reconnect max | Delivery max | Lost / late / dup (client, Postgres) / reordered |
|---|---:|---:|---:|---:|---:|---|
| `kill -9` one instance | 0 | 2 | 27 | 388ms | 537ms | 0 / 0 / 0, 0 / 0 |
| Redis paused 10s | 0 | 0 | 0 | – | 10.9s | 0 / 0 / 0, 0 / 0 |
| Redis restarted | 0 | 0 | 0 | – | 433ms | 0 / 0 / 0, 0 / 0 |
| Postgres restarted | 0 | 142 | 187 | 443ms | 678ms | 0 / 0 / 0, 0 / 0 |
| Instance booted while Redis is down | 0 | 0 | 0 | – | 11.0s | 0 / 0 / 0, 0 / 0 |

- **Instance killed.** HAProxy re-routes its users and the retried sends reuse their txnId. No
  duplicates in Postgres either (`verify.sql`).
- **Redis paused.** Wake-ups stall for the pause, and deliveries arrive once Redis is back.
  Redis is only a latency path: nothing was lost.
- **Redis restarted.** Subscriptions reconnect on their own.
- **Postgres restarted.** Sends fail during the restart, and the client retries them with the
  same txnId. 0 failed after retries, 0 duplicates.
- **Instance booted while Redis is down.** The instance refuses to start (`redis: ping: i/o
  timeout`) instead of running without subscriptions, so an orchestrator just restarts it. The
  11s delivery max comes from the Redis pause the test needs, not from the new instance.

### Disk fsync on this laptop (`fsync/`)

The first full run used Postgres on the laptop's NVMe. Results swung from 0 to 1000 msg/s
between repetitions of the same configuration. Every run had one or two seconds where all cores
went idle at once and every commit waited about 1s. Postgres logged no statement over 250ms.
Same run (one `large` instance, 600 msg/s for 90s), three storage setups:

| Postgres storage | send p99 / max | delivery p99 / max |
|---|---:|---:|
| data dir on disk | 311 / 924ms | 370 / 1044ms |
| disk, `synchronous_commit=off` | 55 / 128ms | 158 / 267ms |
| data dir in `/dev/shm` (fsync still on) | 47 / 95ms | 109 / 229ms |

The stalls are fsync tail latency on this disk (ext4, 89% full), not Nimlet. The phase-04 runs
use `/dev/shm` so they measure CPU and coordination. In production, Postgres' fsync latency goes
straight into send and delivery p99: use storage with consistent sync latency.

### Recommended configurations

| System size | Layout | Profile | Notes |
|---|---|---|---|
| Small (≤ ~1k concurrent users, a few hundred msg/s) | 1–2 instances of 0.5–1 CPU, Postgres 2 cores | `small` (≤0.5 CPU) or `medium` | A second instance is for availability; one 1-CPU `medium` instance handles this load. |
| Medium (~10k users, ~1–2k msg/s) | 2–4 instances of 1–2 CPUs, Postgres 4 cores | `medium` | The tested 2 × 1 CPU split reached 2500 msg/s on a 3-core Postgres. |
| Large (tens of thousands of users, several k msg/s) | 4+ instances of 2+ CPUs, Postgres 8+ cores on fast storage | `large` | Postgres CPU and fsync latency are the ceiling. Size Postgres first and add instances while its CPU has room. |

For all sizes:
- Postgres 18 with `max_locks_per_transaction=256`.
- `NIMLET_DB_MAX_CONNS` = Postgres `max_connections` / instances (profiles default to 12–16 for
  small and medium).
- Load-balance on a hash of the `Authorization` header, so each instance's 1s session cache
  stays warm.
- Drain an instance in the LB before stopping it. It is no longer required for correctness or
  latency, but it avoids a burst of re-routed requests.
