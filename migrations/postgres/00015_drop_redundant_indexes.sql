-- +goose Up
-- Write amplification without readers (docs/benchmarks.md):
--   idx_users_username      duplicates the index behind users.username UNIQUE
--   idx_sessions_token_hash duplicates the index behind sessions.token_hash UNIQUE
--   idx_events_txn_key      events are never looked up by txn_key (event_idempotency is)
-- idx_events_sender_pos stays: nothing reads by sender today, but it backs the
-- events.sender_id → users FK check, which would otherwise scan every partition.
DROP INDEX IF EXISTS idx_users_username;
DROP INDEX IF EXISTS idx_sessions_token_hash;
DROP INDEX IF EXISTS idx_events_txn_key;

-- +goose Down
CREATE INDEX idx_events_txn_key ON events (txn_key);
CREATE INDEX idx_sessions_token_hash ON sessions (token_hash);
CREATE INDEX idx_users_username ON users (username);
