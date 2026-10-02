-- name: ListVPNAccess :many
SELECT v.*,o.kind AS operation_kind,o.state AS operation_state,o.error AS operation_error
FROM vpn_access v JOIN operations o ON o.id=v.operation_id
WHERE v.environment_id=$1 ORDER BY v.created_at,v.id;

-- name: GetVPNAccess :one
SELECT * FROM vpn_access WHERE environment_id=$1 AND id=$2;

-- name: CreateVPNAccess :exec
INSERT INTO vpn_access(id,environment_id,definition,operation_id) VALUES($1,$2,$3,$4);

-- name: SetVPNAccessOperation :exec
UPDATE vpn_access SET operation_id=$3 WHERE environment_id=$1 AND id=$2;

-- name: ApplyVPNAccess :exec
UPDATE vpn_access SET definition=$3,addresses=$4,applied=true WHERE environment_id=$1 AND id=$2;

-- name: DeleteVPNAccess :exec
DELETE FROM vpn_access WHERE environment_id=$1 AND id=$2;

-- name: DeleteEnvironmentVPN :exec
WITH peers AS (DELETE FROM vpn_access WHERE environment_id=$1), aliases AS (DELETE FROM vpn_aliases WHERE environment_id=$1)
UPDATE environments e SET vpn_public_key=NULL,vpn_mtu=NULL WHERE e.id=$1;

-- name: ListVPNAliases :many
SELECT * FROM vpn_aliases ORDER BY prefix;

-- name: ReserveVPNAlias :exec
INSERT INTO vpn_aliases(environment_id,network_id,prefix) VALUES($1,$2,$3);

-- name: DeleteUnusedVPNAliases :exec
DELETE FROM vpn_aliases a WHERE a.environment_id=$1 AND NOT EXISTS (
  SELECT 1 FROM vpn_access v,jsonb_array_elements(v.definition->'routes') r
  WHERE v.environment_id=a.environment_id AND r->>'accessCidr'=a.prefix::text
);

-- name: GetVPNPort :one
SELECT p.*,n.endpoint,n.info FROM service_ports p JOIN nodes n ON n.id=p.node_id
WHERE p.environment_id=$1 AND p.purpose='vpn';

-- name: ReserveVPNPort :exec
INSERT INTO service_ports(node_id,protocol,environment_id,service_id,operation_id,state,purpose)
VALUES($1,'udp',$2,'vpn',$3,'reserved','vpn');

-- name: ApplyVPNPort :exec
UPDATE service_ports SET port=$2,state='applied',updated_at=now() WHERE environment_id=$1 AND purpose='vpn';

-- name: DeleteVPNPort :exec
DELETE FROM service_ports WHERE environment_id=$1 AND purpose='vpn';

-- name: SetVPNGateway :exec
UPDATE environments SET vpn_public_key=$2,vpn_mtu=$3 WHERE id=$1;

-- name: ReleaseAllAccessPorts :exec
DELETE FROM service_ports WHERE environment_id=$1;
