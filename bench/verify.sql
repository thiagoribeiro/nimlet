-- Ground truth for one loadgen run, straight from Postgres (the loadgen only sees what /sync
-- returned). Pass the run tag that the loadgen prints in <out>.summary.json ("run"):
--   psql "$DSN" -v run=<tag> -f bench/verify.sql
-- The send endpoint is idempotent per (device, txnId), and the loadgen retries with the same
-- txnId, so every lg_n must be stored exactly once however many attempts it took.

\set QUIET on
\pset footer off

\echo 'stored events for this run, and lg_n stored more than once (must be 0):'
SELECT count(*)                                    AS stored,
       count(DISTINCT content->>'lg_n')            AS distinct_sends,
       count(*) - count(DISTINCT content->>'lg_n') AS duplicates
FROM events
WHERE content->>'lg_run' = :'run';

\echo 'examples of duplicated sends (empty is good):'
SELECT content->>'lg_n' AS lg_n, count(*) AS copies, array_agg(id) AS event_ids
FROM events
WHERE content->>'lg_run' = :'run'
GROUP BY 1
HAVING count(*) > 1
LIMIT 10;
