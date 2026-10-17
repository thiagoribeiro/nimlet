-- +goose Up

-- join_rule controls who can join: 'invite_only' (default) or 'public' (open-join).
-- direct_key is non-null only for DM rooms: 'direct:userA:userB' (sorted user IDs).
ALTER TABLE rooms
    ADD COLUMN join_rule   TEXT NOT NULL DEFAULT 'invite_only',
    ADD COLUMN direct_key  TEXT UNIQUE;

-- +goose Down
ALTER TABLE rooms
    DROP COLUMN IF EXISTS join_rule,
    DROP COLUMN IF EXISTS direct_key;
