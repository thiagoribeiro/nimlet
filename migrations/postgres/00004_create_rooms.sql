-- +goose Up
CREATE TABLE rooms (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    creator_id TEXT        NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE rooms;
