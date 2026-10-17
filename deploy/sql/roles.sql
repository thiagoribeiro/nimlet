-- Database roles for a Nimlet deployment (see docs/operations.md). Run as the role that owns
-- the schema (the one `nimlet migrate up` uses), after migrations. Re-run after upgrades that
-- add tables. Replace the passwords.
--
--   owner (migrations)  DDL; used only by `nimlet migrate`.
--   nimlet_runtime      the HTTP server: reads/writes chat data; cannot create, change or
--                       delete namespaces, and cannot read or write the admin audit log.
--   nimlet_operator     the admin CLI: everything the runtime can, plus namespaces and audit.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'nimlet_runtime') THEN
        CREATE ROLE nimlet_runtime LOGIN PASSWORD 'change-me-runtime';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'nimlet_operator') THEN
        CREATE ROLE nimlet_operator LOGIN PASSWORD 'change-me-operator';
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO nimlet_runtime, nimlet_operator;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nimlet_runtime, nimlet_operator;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO nimlet_runtime, nimlet_operator;

REVOKE INSERT, UPDATE, DELETE ON namespaces FROM nimlet_runtime;
REVOKE ALL ON admin_audit FROM nimlet_runtime;
REVOKE ALL ON goose_db_version FROM nimlet_runtime, nimlet_operator;
GRANT SELECT ON goose_db_version TO nimlet_runtime, nimlet_operator;
