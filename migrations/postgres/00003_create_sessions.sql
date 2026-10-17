-- +goose Up
CREATE TABLE sessions (
    id         TEXT        PRIMARY KEY,
    device_id  TEXT        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id    TEXT        NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    token_hash TEXT        NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Auth middleware looks up sessions by token_hash on every request — must be fast.
CREATE INDEX idx_sessions_token_hash ON sessions (token_hash);
CREATE INDEX idx_sessions_user_id    ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
