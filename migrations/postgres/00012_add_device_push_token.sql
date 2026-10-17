-- +goose Up
ALTER TABLE devices ADD COLUMN push_token TEXT;

-- +goose Down
ALTER TABLE devices DROP COLUMN push_token;
