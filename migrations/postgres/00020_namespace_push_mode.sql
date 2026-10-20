-- +goose Up
-- How a namespace's members are notified while offline:
--   digest  — one coalesced push with room names and counts, never message content (default);
--   message — one push per message with a short preview (the preview transits FCM).
ALTER TABLE namespaces
    ADD COLUMN push_mode TEXT NOT NULL DEFAULT 'digest'
    CHECK (push_mode IN ('digest', 'message'));

-- +goose Down
ALTER TABLE namespaces DROP COLUMN push_mode;
