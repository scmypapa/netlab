CREATE TABLE backup_repositories (
 id text PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 name text NOT NULL,
 location text NOT NULL,
 credentials bytea NOT NULL,
 native_id text,
 state text NOT NULL DEFAULT 'connecting' CHECK(state IN ('connecting','ready','failed')),
 operation_id text REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE backups (
 id text PRIMARY KEY,
 environment_id text NOT NULL REFERENCES environments(id),
 repository_id text NOT NULL REFERENCES backup_repositories(id),
 name text NOT NULL,
 definition jsonb NOT NULL,
 result jsonb,
 size_bytes bigint NOT NULL DEFAULT 0,
 state text NOT NULL DEFAULT 'creating' CHECK(state IN ('creating','ready','failed','deleting')),
 operation_id text NOT NULL REFERENCES operations(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX backups_environment ON backups(environment_id,created_at DESC,id DESC);
CREATE INDEX backups_repository ON backups(repository_id);
