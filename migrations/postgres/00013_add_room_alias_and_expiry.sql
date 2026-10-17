-- +goose Up

-- alias      — optional stable external reference for a room (e.g. an AppService's
--              "merchant:<id>"). NULL for ordinary rooms; UNIQUE when set so an
--              alias'd create is idempotent.
-- expires_at — optional TTL. Rooms with expires_at < now() are reclaimed by the
--              background sweeper in cmd/server (DeleteExpired). NULL = never expires.
ALTER TABLE rooms
    ADD COLUMN alias      TEXT UNIQUE,
    ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_rooms_expires_at ON rooms (expires_at) WHERE expires_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_rooms_expires_at;
ALTER TABLE rooms
    DROP COLUMN IF EXISTS alias,
    DROP COLUMN IF EXISTS expires_at;
