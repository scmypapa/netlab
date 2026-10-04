-- name: GetGuestConnection :one
SELECT encrypted FROM guest_connections WHERE principal_id=$1 AND environment_id=$2 AND asset_id=$3 AND protocol=$4;
-- name: PutGuestConnection :exec
INSERT INTO guest_connections (principal_id,environment_id,asset_id,protocol,encrypted) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (principal_id,environment_id,asset_id,protocol) DO UPDATE SET encrypted=EXCLUDED.encrypted;
-- name: DeleteGuestConnection :exec
DELETE FROM guest_connections WHERE principal_id=$1 AND environment_id=$2 AND asset_id=$3 AND protocol=$4;
-- name: GetEnvironmentNetworkEndpoint :one
SELECT n.endpoint FROM environments e JOIN nodes n ON n.id=e.network_node_id WHERE e.id=$1;
-- name: DeleteUnusedGuestConnections :exec
DELETE FROM guest_connections g WHERE g.environment_id=$1 AND NOT EXISTS (
 SELECT 1 FROM environments e, jsonb_array_elements(COALESCE(e.applied_spec,e.spec)->'assets') a
 WHERE e.id=g.environment_id AND e.status<>'destroyed' AND a->>'id'=g.asset_id
);
