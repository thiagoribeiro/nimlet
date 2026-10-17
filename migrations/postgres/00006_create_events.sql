-- +goose Up

-- Explicit sequence: BIGSERIAL is not supported on partitioned tables.
-- The sequence value is the global, monotonic stream_pos that drives /sync.
CREATE SEQUENCE events_stream_pos_seq START 1;

CREATE TABLE events (
    stream_pos  BIGINT      NOT NULL DEFAULT nextval('events_stream_pos_seq'),
    id          TEXT        NOT NULL,
    room_id     TEXT        NOT NULL REFERENCES rooms(id),
    sender_id   TEXT        NOT NULL REFERENCES users(id),
    event_type  TEXT        NOT NULL,
    content     JSONB       NOT NULL DEFAULT '{}',
    txn_key     TEXT        NOT NULL,  -- unicidade global gerenciada via event_idempotency
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (stream_pos)           -- PK must include the partition key
) PARTITION BY RANGE (stream_pos);

-- Indexes on the parent are inherited by every partition (PG 11+).
CREATE INDEX idx_events_room_pos   ON events (room_id, stream_pos);
CREATE INDEX idx_events_sender_pos ON events (sender_id, stream_pos);
CREATE INDEX idx_events_txn_key    ON events (txn_key);

-- +goose Down
DROP TABLE events;
DROP SEQUENCE events_stream_pos_seq;
