CREATE TABLE blueprints (
  id text PRIMARY KEY, project_id text NOT NULL REFERENCES projects(id), owner_id text REFERENCES principals(id),
  name text NOT NULL, version integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blueprints_project_updated ON blueprints(project_id,updated_at DESC,id);
CREATE TABLE blueprint_versions (
  id text PRIMARY KEY, blueprint_id text NOT NULL REFERENCES blueprints(id), version integer NOT NULL,
  spec jsonb NOT NULL, view jsonb NOT NULL,
  asset_count integer NOT NULL, network_count integer NOT NULL,
  source_environment_id text NOT NULL REFERENCES environments(id), source_revision integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(blueprint_id,version)
);
ALTER TABLE environments ADD COLUMN blueprint_version_id text REFERENCES blueprint_versions(id);
