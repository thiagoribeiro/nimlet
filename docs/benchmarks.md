# Nimlet benchmarks

How to run: [`bench/README.md`](../bench/README.md). Raw outputs: `bench/results/<phase>/`.

**Environment (all runs):** AMD Ryzen AI 5 330 (8 threads), 14 GB RAM, Linux. Postgres 16.15 and Redis 7
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
