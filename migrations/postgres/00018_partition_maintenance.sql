-- +goose Up
-- events / event_visibility are range-partitioned by stream_pos in fixed blocks (00009), but
-- only the first two blocks were ever created: past position 20M every row lands in the
-- DEFAULT partitions, and from then on a new block can no longer be added (Postgres refuses
-- to create a partition whose range already has rows in DEFAULT). ensure_event_partitions
-- keeps `ahead` blocks created beyond the current position; the server calls it at startup
-- and periodically.
--
-- SECURITY DEFINER: it runs with the schema owner's rights, so a server using a runtime role
-- without DDL privileges (deploy/sql/roles.sql) can still maintain partitions — and only this
-- exact DDL. search_path is pinned so the definer's rights cannot be hijacked.
-- +goose StatementBegin
CREATE FUNCTION ensure_event_partitions(ahead INT DEFAULT 1, partition_size BIGINT DEFAULT 10000000)
RETURNS INT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    cur     BIGINT;
    first_n BIGINT;
    created INT := 0;
    n       BIGINT;
BEGIN
    SELECT CASE WHEN is_called THEN last_value ELSE 0 END INTO cur FROM events_stream_pos_seq;
    first_n := GREATEST(cur - 1, 0) / partition_size + 1;
    FOR n IN first_n .. first_n + ahead LOOP
        IF to_regclass(format('events_p%s', n)) IS NULL THEN
            PERFORM create_event_partitions(n, partition_size);
            created := created + 1;
        END IF;
    END LOOP;
    RETURN created;
END
$$;
-- +goose StatementEnd

-- Lets the runtime role (and anyone else) call it; it only ever creates the next blocks.
GRANT EXECUTE ON FUNCTION ensure_event_partitions(INT, BIGINT) TO PUBLIC;

-- Catch up existing installs now.
SELECT ensure_event_partitions();

-- stream_inflight takes an insert and a delete per send: vacuum it by dead-row count, not by
-- the (tiny) table size, so min(pos) reads stay cheap under load.
ALTER TABLE stream_inflight SET (
    autovacuum_vacuum_scale_factor = 0,
    autovacuum_vacuum_threshold    = 1000,
    autovacuum_vacuum_cost_delay   = 0
);

-- +goose Down
ALTER TABLE stream_inflight RESET (autovacuum_vacuum_scale_factor, autovacuum_vacuum_threshold, autovacuum_vacuum_cost_delay);
DROP FUNCTION ensure_event_partitions(INT, BIGINT);
