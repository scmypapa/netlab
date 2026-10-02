CREATE TABLE projects (id text PRIMARY KEY, name text NOT NULL);
INSERT INTO projects VALUES ('default', '默认项目');
CREATE TABLE principals (
  id text PRIMARY KEY, name text NOT NULL UNIQUE, kind text NOT NULL CHECK (kind IN ('user','token')),
  password_hash bytea, administrator boolean NOT NULL DEFAULT false
);
CREATE TABLE credentials (
  hash bytea PRIMARY KEY, principal_id text NOT NULL REFERENCES principals(id),
  expires_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE grants (
  principal_id text NOT NULL REFERENCES principals(id), scope_kind text NOT NULL CHECK(scope_kind IN ('project','environment','asset')),
  scope_id text NOT NULL, permissions text[] NOT NULL, PRIMARY KEY(principal_id,scope_kind,scope_id)
);
CREATE TABLE templates (id text PRIMARY KEY, definition jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE nodes (
  id text PRIMARY KEY, name text NOT NULL, endpoint text NOT NULL UNIQUE, info jsonb NOT NULL,
  capacity_override jsonb, state text NOT NULL DEFAULT 'ready', observed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE environments (
  id text PRIMARY KEY, project_id text NOT NULL REFERENCES projects(id), owner_id text REFERENCES principals(id),
  name text NOT NULL, external_reference text, revision integer NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'draft', spec jsonb NOT NULL, applied_spec jsonb,
  view jsonb NOT NULL DEFAULT '{}', draft jsonb, network_node_id text REFERENCES nodes(id),
  operation_id text, error text, client_request_id text,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,client_request_id)
);
CREATE INDEX environments_project_updated ON environments(project_id,updated_at DESC,id);
CREATE TABLE operations (
  id text PRIMARY KEY, environment_id text REFERENCES environments(id),
  scope_kind text NOT NULL, scope_id text NOT NULL,
  kind text NOT NULL, asset_id text, state text NOT NULL DEFAULT 'queued', phase text NOT NULL DEFAULT 'queued',
  payload jsonb NOT NULL, results jsonb NOT NULL DEFAULT '[]', error text,
  expected_revision integer NOT NULL, lease_owner text, lease_until timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  client_request_id text, UNIQUE(scope_kind,scope_id,client_request_id)
);
CREATE INDEX operations_queue ON operations(created_at) WHERE state IN ('queued','running');
CREATE UNIQUE INDEX operations_one_running ON operations(scope_kind,scope_id) WHERE state='running';
CREATE INDEX operations_environment ON operations(environment_id,created_at DESC,id);
CREATE TABLE runtime_assets (
  environment_id text NOT NULL REFERENCES environments(id), asset_id text NOT NULL, instance_id text NOT NULL UNIQUE,
  node_id text NOT NULL REFERENCES nodes(id), execution jsonb NOT NULL, state text NOT NULL DEFAULT 'reserved',
  cpu integer NOT NULL, memory_mib bigint NOT NULL, disk_gib bigint NOT NULL,
  current boolean NOT NULL DEFAULT false,
  error text, observed_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(environment_id,asset_id,instance_id)
);
CREATE INDEX runtime_assets_node ON runtime_assets(node_id);
CREATE UNIQUE INDEX runtime_assets_current ON runtime_assets(environment_id,asset_id) WHERE current;
CREATE TABLE events (
  cursor bigserial PRIMARY KEY, environment_id text REFERENCES environments(id),
  kind text NOT NULL, payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_environment ON events(environment_id,cursor);
