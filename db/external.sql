-- name: ReserveExternalNetwork :one
INSERT INTO external_network_leases(node_id,interface,vlan,environment_id,network_id)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT(node_id,interface,vlan) DO UPDATE SET network_id=EXCLUDED.network_id
WHERE external_network_leases.environment_id=EXCLUDED.environment_id
RETURNING *;

-- name: ListExternalNetworks :many
SELECT * FROM external_network_leases WHERE environment_id=$1 ORDER BY node_id,interface,vlan;

-- name: ReleaseExternalNetworks :exec
DELETE FROM external_network_leases WHERE environment_id=$1
AND NOT ((node_id||'/'||interface||'/'||vlan::text)=ANY(sqlc.arg(retained)::text[]));
