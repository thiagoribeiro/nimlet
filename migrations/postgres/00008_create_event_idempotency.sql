-- +goose Up

-- Global idempotency table: guarantees txn_key uniqueness across every
-- events partition, which a UNIQUE constraint cannot do on a table
-- partitioned by another column.
-- Size: one row per event. Purge it together with old partitions:
-- DELETE FROM event_idempotency WHERE stream_pos < <partition_bound>.
CREATE TABLE event_idempotency (
    txn_key    TEXT        PRIMARY KEY,
    stream_pos BIGINT      NOT NULL,
    event_id   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index for efficient purging by stream_pos range.
CREATE INDEX idx_event_idempotency_pos ON event_idempotency (stream_pos);

-- +goose Down
DROP TABLE event_idempotency;
