CREATE TABLE persistent_volumes (
 id text PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 storage_pool_id text NOT NULL,
 name text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('vm','container')),
 size_gib bigint NOT NULL CHECK (size_gib > 0),
 state text NOT NULL,
 operation_id text REFERENCES operations(id)
);
CREATE INDEX persistent_volumes_pool ON persistent_volumes(storage_pool_id);
