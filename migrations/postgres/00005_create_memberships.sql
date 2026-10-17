-- +goose Up
CREATE TABLE room_memberships (
    room_id    TEXT        NOT NULL REFERENCES rooms(id)   ON DELETE CASCADE,
    user_id    TEXT        NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    state      TEXT        NOT NULL DEFAULT 'invite',
    role       TEXT        NOT NULL DEFAULT 'member',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (room_id, user_id)
);

CREATE INDEX idx_memberships_user_id    ON room_memberships (user_id);
CREATE INDEX idx_memberships_room_state ON room_memberships (room_id, state);

-- +goose Down
DROP TABLE room_memberships;
