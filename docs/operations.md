# Operating Nimlet

This guide covers database roles, running the admin CLI safely, and Kubernetes.

## Database roles

Use three Postgres roles, so that a compromised server process cannot create namespaces or
rewrite the audit log:

| Role | Used by | Can |
|---|---|---|
| owner (e.g. `nimlet`) | `nimlet migrate` only | DDL; owns the schema |
| `nimlet_runtime` | `nimlet server` (`NIMLET_DB_DSN`) | read/write chat data. Can read `namespaces` but not change them; no access to `admin_audit` |
| `nimlet_operator` | admin CLI (`NIMLET_ADMIN_DB_DSN`) | everything the runtime role can, plus namespaces and the audit log |

None of them needs `SUPERUSER`.

1. Run migrations as the owner: `NIMLET_DB_DSN=<owner dsn> nimlet migrate up`.
2. Apply [`deploy/sql/roles.sql`](../deploy/sql/roles.sql) as the owner, after replacing the
   passwords. Re-apply it after any upgrade that adds tables, because the grants cover existing
   tables only.
3. Start the server with the runtime role and `NIMLET_AUTO_MIGRATE=false`, since that role
   cannot run DDL.

`roles.sql` was tested this way: the server served register, join, send and sync under
`nimlet_runtime`, and `nimlet namespace create` under that role failed with exit code 5
(permission denied).

With a single role (the default Docker Compose setup), everything keeps working. You just lose
this separation.

## Running the admin CLI

The CLI connects straight to the database. There is no admin HTTP endpoint to protect. What you
protect is the operator DSN.

- **Never put `NIMLET_ADMIN_DB_DSN` in the server's environment or image.** Keep it only where
  admin commands run.
- **Don't `kubectl exec` into a server pod to run admin commands.** That pod has the server's
  identity and secrets, not the operator's. Run a short-lived Job (or an operator pod) with its
  own ServiceAccount and Secret instead.
- **Set `NIMLET_ADMIN_OPERATOR`** to a meaningful identity, such as the person or pipeline running
  the Job. It is what the audit log records. Without it, the log gets the container's
  `user@hostname`.
- **Pass passwords only on stdin** (`--password-stdin`), e.g. from a mounted Secret. Never pass
  them as arguments.

Example Kubernetes Job that bootstraps a namespace:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: nimlet-bootstrap-acme
spec:
  ttlSecondsAfterFinished: 600
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: nimlet-operator     # no RBAC beyond reading its own Secret
      restartPolicy: Never
      containers:
        - name: nimlet
          image: ghcr.io/thiagoribeiro/nimlet:<version>
          command: ["/bin/sh", "-ec"]
          args:
            - |
              nimlet namespace create acme
              nimlet user create alice -n acme --password-stdin < /secrets/alice-password
          env:
            - name: NIMLET_ADMIN_DB_DSN
              valueFrom: { secretKeyRef: { name: nimlet-operator-db, key: dsn } }
            - name: NIMLET_ADMIN_OPERATOR
              value: "job/nimlet-bootstrap-acme"
          volumeMounts:
            - { name: secrets, mountPath: /secrets, readOnly: true }
      volumes:
        - name: secrets
          secret: { secretName: nimlet-bootstrap-acme }
```

`namespace create` exits with code 4 when the namespace already exists. Use `user create`'s exit
code, or `room member add --if-not-exists`, to make bootstrap scripts re-runnable.

## Audit log

Every mutating CLI command writes a row to `admin_audit`, whatever its outcome. Each row has the
operator, command, namespace, target, outcome and a correlation ID shared by one CLI invocation.
Passwords and tokens are never written. Example query:

```sql
SELECT created_at, operator, command, target, outcome
FROM admin_audit ORDER BY created_at DESC LIMIT 50;
```

## Event partitions

`events` and `event_visibility` are range-partitioned by stream position, in blocks of 10M.
The server keeps the next block created ahead of the stream: at startup and every sweep, it
calls `ensure_event_partitions()`, a `SECURITY DEFINER` function, so this works under
`nimlet_runtime` too.

**Alarm.** `nimlet_events_default_partition_in_use = 1` means rows reached the DEFAULT
partitions, so the stream outran maintenance (for example, the server was down while another
writer kept going). New blocks cannot be created over a range that has rows in DEFAULT. Repair
it in a maintenance window, as the schema owner:

```sql
BEGIN;
ALTER TABLE events DETACH PARTITION events_default;
ALTER TABLE event_visibility DETACH PARTITION event_visibility_default;
SELECT ensure_event_partitions(ahead => 1);   -- creates blocks up to the current position + 1
-- create any missing block covering the rows parked in the defaults, e.g.:
-- SELECT create_event_partitions(<n>);
INSERT INTO events SELECT * FROM events_default;
INSERT INTO event_visibility SELECT * FROM event_visibility_default;
TRUNCATE events_default, event_visibility_default;
ALTER TABLE events ATTACH PARTITION events_default DEFAULT;
ALTER TABLE event_visibility ATTACH PARTITION event_visibility_default DEFAULT;
COMMIT;
```

**Open scale item: partition count.** Every `/sync` touches every partition of both tables.
Measured under load (200 listeners, PostgreSQL 16 and 18), going from 3 to 5 partitions per
table raised send p50 from 6ms to 24ms through lock-manager contention. Because of this,
maintenance keeps only one block ahead. History still adds a block every 10M events, though, so
a long-lived busy install will slow down this way. The fix is a retention policy: detach and
archive or drop old blocks once their events are no longer served. That is a product decision
(how long history stays available) and is not implemented yet.

## Multiple instances

- **Every instance subscribes to every Redis partition.** That is enough for cross-instance
  wake-ups.
- **Size the pool per instance.** Set `NIMLET_DB_MAX_CONNS` to
  `max_connections / instances`, and watch `nimlet_db_pool_empty_acquires_total`: a rising
  rate means requests are queueing for a connection.
- **All instances must run the same version.** `stream_inflight` ordering and AppService
  routing assume it, so stop old instances before starting new ones across a schema change.

## Threat model (namespaces)

| Threat | Mitigation | Limit |
|---|---|---|
| A client of namespace A reads or writes B | Namespace comes only from the session; names resolve within it; composite FKs | Long-poll timing only: cursors are opaque and `room_seq` counts one room |
| A compromised AppService | Its token is confined to its namespace and its own `/appservice/{asId}` path; it can only mint sessions for `service` accounts | Everything inside its own namespace |
| A server bug that skips a check | Composite FKs reject cross-namespace memberships, rooms and sessions; media IDs are UUID-only | Reads that go through only global IDs rely on application code and the isolation test suite |
| A compromised server process | Runtime role cannot change namespaces or the audit log | It can still read and write every namespace's chat data |
| A privileged operator (DB/S3 access) | Audit log for CLI actions | Direct SQL or S3 access bypasses everything; isolation is not protection from operators |
| Noisy neighbour | None in v1 | Shared Postgres, Redis and process: no per-namespace quotas or rate limits |
