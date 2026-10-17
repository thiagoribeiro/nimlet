-- +goose Up
CREATE TABLE devices (
    id           TEXT        PRIMARY KEY,
    user_id      TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT        NOT NULL,
    disabled     BOOLEAN     NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_devices_user_id ON devices (user_id);

-- +goose Down
DROP TABLE devices;
