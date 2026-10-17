-- +goose Up
CREATE TABLE users (
    id           TEXT        PRIMARY KEY,
    username     TEXT        NOT NULL UNIQUE,
    display_name TEXT        NOT NULL,
    type         TEXT        NOT NULL DEFAULT 'human',
    password_hash TEXT       NOT NULL,
    disabled     BOOLEAN     NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_users_username ON users (username);

-- +goose Down
DROP TABLE users;
