CREATE TABLE security_sensors (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 cursor text NOT NULL DEFAULT '', observation jsonb NOT NULL,
 checked_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE security_alerts (
 id bigserial PRIMARY KEY,
 environment_id text NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 bucket timestamptz NOT NULL, network_id text NOT NULL, interface_id text NOT NULL,
 generator_id bigint NOT NULL, signature_id bigint NOT NULL,
 source text NOT NULL, destination text NOT NULL,
 source_port integer NOT NULL, destination_port integer NOT NULL, protocol text NOT NULL,
 severity integer NOT NULL CHECK(severity BETWEEN 1 AND 255),
 count bigint NOT NULL, first_seen timestamptz NOT NULL, last_seen timestamptz NOT NULL,
 detail jsonb NOT NULL,
 UNIQUE(environment_id,node_id,bucket,network_id,interface_id,generator_id,signature_id,source,destination,source_port,destination_port,protocol)
);
CREATE INDEX security_alerts_environment ON security_alerts(environment_id,id DESC);
CREATE INDEX security_alerts_environment_time ON security_alerts(environment_id,last_seen DESC);
CREATE INDEX security_alerts_retention ON security_alerts(last_seen);
