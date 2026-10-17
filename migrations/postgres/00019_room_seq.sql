-- +goose Up
-- room_seq: a per-room event number (1, 2, 3 … with occasional gaps from replayed sends).
--
-- Clients need a key to order and dedupe events within a room and to page its history.
-- The global stream_pos cannot be exposed (it reveals other namespaces' traffic volume);
-- a number scoped to one room reveals nothing its members cannot already see. It is assigned
-- in the same serialised allocation as stream_pos, so within a room it follows exactly the
-- order /sync delivers events in.
--
-- Counters live in their own table rather than on rooms: sends would otherwise contend on
-- the rooms row with joins/leaves (member_count) while holding the global allocation lock.
CREATE TABLE room_seqs (
    room_id  TEXT   PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    last_seq BIGINT NOT NULL
);

ALTER TABLE events ADD COLUMN room_seq BIGINT;

-- Backfill (runs in the upgrade window): number existing events per room in stream order.
UPDATE events e SET room_seq = n.seq
FROM (
    SELECT stream_pos, row_number() OVER (PARTITION BY room_id ORDER BY stream_pos) AS seq
    FROM events
) n
WHERE e.stream_pos = n.stream_pos;

INSERT INTO room_seqs (room_id, last_seq)
SELECT room_id, max(room_seq) FROM events GROUP BY room_id;

ALTER TABLE events ALTER COLUMN room_seq SET NOT NULL;

-- /messages pages by (room_id, room_seq). The unused (sender_id, stream_pos) index goes, so
-- each partition keeps the same number of indexes: every /sync locks every partition index,
-- and more relations per query measurably added lock contention (docs/benchmarks.md, 03).
-- It only backed the events.sender_id FK check on user deletion, and users are never
-- deleted (they are disabled).
CREATE INDEX idx_events_room_seq ON events (room_id, room_seq);
DROP INDEX idx_events_sender_pos;

-- +goose Down
CREATE INDEX idx_events_sender_pos ON events (sender_id, stream_pos);
DROP INDEX idx_events_room_seq;
ALTER TABLE events DROP COLUMN room_seq;
DROP TABLE room_seqs;
