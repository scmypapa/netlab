CREATE TABLE external_network_leases (
  node_id text NOT NULL REFERENCES nodes(id), interface text NOT NULL,
  vlan integer NOT NULL CHECK (vlan BETWEEN 0 AND 4094),
  environment_id text NOT NULL REFERENCES environments(id), network_id text NOT NULL,
  PRIMARY KEY(node_id,interface,vlan)
);
CREATE INDEX external_network_leases_environment ON external_network_leases(environment_id);
