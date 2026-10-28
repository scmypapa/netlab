-- name: EnsurePacketNodes :exec
INSERT INTO packet_nodes(environment_id,node_id)
SELECT unnest(@environment_ids::text[]),unnest(@node_ids::text[])
ON CONFLICT DO NOTHING;

-- name: ListPacketNodes :many
SELECT p.*,n.endpoint,n.name,n.state FROM packet_nodes p JOIN nodes n ON n.id=p.node_id ORDER BY p.environment_id,p.node_id;

-- name: ListEnvironmentPacketNodes :many
SELECT p.*,n.endpoint,n.name,n.state FROM packet_nodes p JOIN nodes n ON n.id=p.node_id WHERE p.environment_id=$1 ORDER BY p.node_id;

-- name: SavePacketNode :exec
UPDATE packet_nodes SET quota_bytes=$3,observation=$4,updated_at=now() WHERE environment_id=$1 AND node_id=$2;

-- name: GrantPacketQuotas :batchexec
UPDATE packet_nodes p SET quota_bytes=$3
WHERE p.environment_id=$1 AND p.node_id=$2 AND $3>p.quota_bytes
AND $3-p.quota_bytes<=1073741824-(SELECT COALESCE(sum(other.quota_bytes),0) FROM packet_nodes other WHERE other.environment_id=$1);

-- name: DeleteEnvironmentPacketNodes :exec
DELETE FROM packet_nodes WHERE environment_id=$1;
