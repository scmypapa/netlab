CREATE TABLE storage_pools (
  id text PRIMARY KEY,
  node_id text NOT NULL REFERENCES nodes(id),
  name text NOT NULL,
  directory text NOT NULL,
  path text NOT NULL,
  state text NOT NULL DEFAULT 'ready' CHECK(state IN ('ready','deleting')),
  operation_id text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(node_id,directory)
);
CREATE INDEX runtime_assets_storage ON runtime_assets(node_id,(execution->>'storagePoolId'));
