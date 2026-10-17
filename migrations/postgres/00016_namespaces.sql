-- +goose Up
-- Namespaces: hard isolation between applications sharing one Nimlet instance.
--
-- Identity is (namespace_id, username); internal IDs (users.id, rooms.id, devices.id,
-- events.stream_pos) stay globally unique, so the /sync hot path and its indexes need no
-- namespace column. Isolation is enforced where relations are created: every membership,
-- room and session carries namespace_id, and composite foreign keys make a cross-namespace
-- membership/room/session impossible at the database level, whatever the application does.
CREATE TABLE namespaces (
    id                 TEXT        PRIMARY KEY,
    slug               TEXT        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    status             TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    allow_registration BOOLEAN     NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at        TIMESTAMPTZ
);

-- Everything that existed before namespaces belongs to "default", which keeps the
-- pre-namespace behaviour (public registration on).
INSERT INTO namespaces (id, slug, allow_registration)
VALUES ('00000000-0000-0000-0000-000000000000', 'default', true);

-- ADD COLUMN with a constant default is metadata-only (no table rewrite); the default is
-- dropped again below so no future insert can silently land in "default".
ALTER TABLE users            ADD COLUMN namespace_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000' REFERENCES namespaces(id);
ALTER TABLE rooms            ADD COLUMN namespace_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000' REFERENCES namespaces(id);
ALTER TABLE room_memberships ADD COLUMN namespace_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
ALTER TABLE sessions         ADD COLUMN namespace_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

-- External names are unique per namespace only.
ALTER TABLE users DROP CONSTRAINT users_username_key;
ALTER TABLE users ADD CONSTRAINT users_namespace_username_key UNIQUE (namespace_id, username);
ALTER TABLE rooms DROP CONSTRAINT rooms_alias_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_namespace_alias_key UNIQUE (namespace_id, alias);
ALTER TABLE rooms DROP CONSTRAINT rooms_direct_key_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_namespace_direct_key_key UNIQUE (namespace_id, direct_key);

-- Targets for the composite foreign keys.
ALTER TABLE users ADD CONSTRAINT users_namespace_id_id_key UNIQUE (namespace_id, id);
ALTER TABLE rooms ADD CONSTRAINT rooms_namespace_id_id_key UNIQUE (namespace_id, id);

-- A room's creator, a membership's room and user, and a session's user must all share
-- the row's namespace.
ALTER TABLE rooms DROP CONSTRAINT rooms_creator_id_fkey;
ALTER TABLE rooms ADD CONSTRAINT rooms_creator_ns_fkey
    FOREIGN KEY (namespace_id, creator_id) REFERENCES users (namespace_id, id);

ALTER TABLE room_memberships DROP CONSTRAINT room_memberships_room_id_fkey;
ALTER TABLE room_memberships DROP CONSTRAINT room_memberships_user_id_fkey;
ALTER TABLE room_memberships ADD CONSTRAINT room_memberships_room_ns_fkey
    FOREIGN KEY (namespace_id, room_id) REFERENCES rooms (namespace_id, id) ON DELETE CASCADE;
ALTER TABLE room_memberships ADD CONSTRAINT room_memberships_user_ns_fkey
    FOREIGN KEY (namespace_id, user_id) REFERENCES users (namespace_id, id) ON DELETE CASCADE;

ALTER TABLE sessions DROP CONSTRAINT sessions_user_id_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_user_ns_fkey
    FOREIGN KEY (namespace_id, user_id) REFERENCES users (namespace_id, id) ON DELETE CASCADE;

ALTER TABLE users            ALTER COLUMN namespace_id DROP DEFAULT;
ALTER TABLE rooms            ALTER COLUMN namespace_id DROP DEFAULT;
ALTER TABLE room_memberships ALTER COLUMN namespace_id DROP DEFAULT;
ALTER TABLE sessions         ALTER COLUMN namespace_id DROP DEFAULT;

-- Media metadata: which namespace (and user) an upload belongs to. Downloads are
-- authorised against it; uploads from before this migration have no row and are
-- treated as belonging to "default".
CREATE TABLE media (
    id            TEXT        PRIMARY KEY,
    namespace_id  TEXT        NOT NULL REFERENCES namespaces(id),
    owner_user_id TEXT        NOT NULL,
    content_type  TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (namespace_id, owner_user_id) REFERENCES users (namespace_id, id) ON DELETE CASCADE
);

-- +goose Down
-- Only safe while every row is still in "default".
DROP TABLE media;

ALTER TABLE sessions DROP CONSTRAINT sessions_user_ns_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE room_memberships DROP CONSTRAINT room_memberships_user_ns_fkey;
ALTER TABLE room_memberships DROP CONSTRAINT room_memberships_room_ns_fkey;
ALTER TABLE room_memberships ADD CONSTRAINT room_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE room_memberships ADD CONSTRAINT room_memberships_room_id_fkey FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE;
ALTER TABLE rooms DROP CONSTRAINT rooms_creator_ns_fkey;
ALTER TABLE rooms ADD CONSTRAINT rooms_creator_id_fkey FOREIGN KEY (creator_id) REFERENCES users(id);

ALTER TABLE rooms DROP CONSTRAINT rooms_namespace_id_id_key;
ALTER TABLE users DROP CONSTRAINT users_namespace_id_id_key;
ALTER TABLE rooms DROP CONSTRAINT rooms_namespace_direct_key_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_direct_key_key UNIQUE (direct_key);
ALTER TABLE rooms DROP CONSTRAINT rooms_namespace_alias_key;
ALTER TABLE rooms ADD CONSTRAINT rooms_alias_key UNIQUE (alias);
ALTER TABLE users DROP CONSTRAINT users_namespace_username_key;
ALTER TABLE users ADD CONSTRAINT users_username_key UNIQUE (username);

ALTER TABLE sessions         DROP COLUMN namespace_id;
ALTER TABLE room_memberships DROP COLUMN namespace_id;
ALTER TABLE rooms            DROP COLUMN namespace_id;
ALTER TABLE users            DROP COLUMN namespace_id;
DROP TABLE namespaces;
