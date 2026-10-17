-- +goose Up
ALTER TABLE rooms ADD COLUMN member_count INT NOT NULL DEFAULT 0;
CREATE INDEX idx_rooms_large ON rooms (id) WHERE member_count >= 1000;

UPDATE rooms SET member_count = (
    SELECT COUNT(*) FROM room_memberships
    WHERE room_id = rooms.id AND state = 'join'
);

-- +goose Down
ALTER TABLE rooms DROP COLUMN member_count;
