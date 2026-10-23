CREATE TABLE packet_nodes (
 environment_id text NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 quota_bytes bigint NOT NULL DEFAULT 0 CHECK(quota_bytes BETWEEN 0 AND 1073741824),
 observation jsonb NOT NULL DEFAULT '{"limitBytes":0,"usedBytes":0,"status":"idle"}',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(environment_id,node_id)
);
