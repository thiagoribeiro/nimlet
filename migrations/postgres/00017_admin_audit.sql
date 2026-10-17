-- +goose Up
-- Administrative actions performed with the nimlet CLI. Never stores passwords or tokens.
CREATE TABLE admin_audit (
    id             TEXT        PRIMARY KEY,
    operator       TEXT        NOT NULL,
    command        TEXT        NOT NULL,
    namespace_id   TEXT,
    target         TEXT        NOT NULL,
    outcome        TEXT        NOT NULL,
    correlation_id TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_admin_audit_created ON admin_audit (created_at);

-- +goose Down
DROP TABLE admin_audit;
