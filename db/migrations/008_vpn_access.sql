ALTER TABLE environments ADD COLUMN vpn_public_key text, ADD COLUMN vpn_mtu integer;
ALTER TABLE service_ports ADD COLUMN purpose text NOT NULL DEFAULT 'service' CHECK (purpose IN ('service','vpn'));
CREATE UNIQUE INDEX service_ports_vpn_environment ON service_ports(environment_id) WHERE purpose='vpn';
CREATE TABLE vpn_access (
  id text PRIMARY KEY, environment_id text NOT NULL REFERENCES environments(id),
  definition jsonb NOT NULL, addresses inet[], applied boolean NOT NULL DEFAULT false,
  operation_id text NOT NULL REFERENCES operations(id), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX vpn_access_environment ON vpn_access(environment_id,created_at,id);
CREATE UNIQUE INDEX vpn_access_public_key ON vpn_access(environment_id,(definition->>'publicKey'));
CREATE TABLE vpn_aliases (
  environment_id text NOT NULL REFERENCES environments(id), network_id text NOT NULL,
  prefix cidr PRIMARY KEY
);
