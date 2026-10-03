CREATE TABLE guest_connections (
 principal_id text NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
 environment_id text NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
 asset_id text NOT NULL,
 encrypted bytea NOT NULL,
 PRIMARY KEY (principal_id, environment_id, asset_id)
);
CREATE INDEX guest_connections_environment ON guest_connections(environment_id,asset_id);
