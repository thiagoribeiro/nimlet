-- +goose Up

-- event_visibility is the /sync hot-path table.
-- Partitioned by stream_pos (RANGE), co-located with events:
--   same ranges, so dropping an old partition is instant DDL on both.
-- No FK to events: with co-located partitions, integrity is enforced by the
-- application (EventStore.Create inserts both in the same transaction).
CREATE TABLE event_visibility (
    user_id    TEXT   NOT NULL,
    stream_pos BIGINT NOT NULL,
    PRIMARY KEY (user_id, stream_pos)  -- stream_pos is the partition key
) PARTITION BY RANGE (stream_pos);

-- +goose Down
DROP TABLE event_visibility;
