ALTER TABLE environments ADD COLUMN gateway_address inet;
CREATE UNIQUE INDEX environments_gateway_address ON environments(network_node_id,gateway_address) WHERE gateway_address IS NOT NULL;

CREATE TABLE service_ports (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  node_id text NOT NULL REFERENCES nodes(id),
  protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
  port integer CHECK (port BETWEEN 1 AND 65535),
  environment_id text NOT NULL REFERENCES environments(id),
  service_id text NOT NULL,
  operation_id text NOT NULL REFERENCES operations(id),
  state text NOT NULL CHECK (state IN ('reserved','applied')),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(node_id,protocol,port),
  CHECK (state <> 'applied' OR port IS NOT NULL)
);
CREATE INDEX service_ports_environment ON service_ports(environment_id,service_id);

CREATE FUNCTION service_change_assets(payload jsonb) RETURNS text[] LANGUAGE sql IMMUTABLE AS $$
  WITH changed AS (
    (SELECT value FROM jsonb_array_elements(COALESCE(payload->'spec'->'services','[]'))
     EXCEPT SELECT value FROM jsonb_array_elements(COALESCE(payload->'beforeSpec'->'services','[]')))
    UNION
    (SELECT value FROM jsonb_array_elements(COALESCE(payload->'beforeSpec'->'services','[]'))
     EXCEPT SELECT value FROM jsonb_array_elements(COALESCE(payload->'spec'->'services','[]')))
  )
  SELECT COALESCE(array_agg(DISTINCT value->>'assetId'),'{}'::text[]) FROM changed;
$$;
