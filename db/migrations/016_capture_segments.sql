-- Only diagnostic ownership is stored here; the node owns capture state and packet data.
CREATE TABLE capture_segments (
  capture_id text NOT NULL,
  environment_id text NOT NULL REFERENCES environments(id),
  node_id text NOT NULL REFERENCES nodes(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (capture_id,node_id)
);
CREATE INDEX capture_segments_environment ON capture_segments(environment_id,node_id);
