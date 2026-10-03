CREATE TABLE recovery_points (
  id text PRIMARY KEY,
  environment_id text NOT NULL REFERENCES environments(id),
  name text NOT NULL,
  revision integer NOT NULL,
  state text NOT NULL DEFAULT 'capturing' CHECK(state IN ('capturing','ready','failed','deleting')),
  definition jsonb NOT NULL,
  asset_count integer NOT NULL,
  size_bytes bigint NOT NULL DEFAULT 0,
  operation_id text NOT NULL REFERENCES operations(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recovery_points_environment ON recovery_points(environment_id,created_at DESC,id DESC);
