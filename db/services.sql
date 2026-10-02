-- name: GetServicePorts :many
SELECT * FROM service_ports WHERE environment_id=$1 ORDER BY service_id,protocol,port;

-- name: ReserveServicePort :one
INSERT INTO service_ports(node_id,protocol,port,environment_id,service_id,operation_id,state)
VALUES(sqlc.arg(node_id),sqlc.arg(protocol),NULLIF(sqlc.arg(requested_port)::integer,0),sqlc.arg(environment_id),sqlc.arg(service_id),sqlc.arg(operation_id),'reserved') RETURNING *;

-- name: DeleteUnusedServicePorts :exec
DELETE FROM service_ports p WHERE p.environment_id=sqlc.arg(environment_id)
 AND NOT EXISTS(SELECT 1 FROM jsonb_to_recordset(sqlc.arg(bindings)::jsonb) AS b(id text,protocol text,"listenPort" integer)
 WHERE b.id=p.service_id AND b.protocol=p.protocol AND (b."listenPort"=p.port OR p.port IS NULL));

-- name: ApplyServicePorts :exec
UPDATE service_ports p SET state='applied',port=b."listenPort",updated_at=now()
FROM jsonb_to_recordset(sqlc.arg(bindings)::jsonb) AS b(id text,protocol text,"listenPort" integer)
WHERE p.environment_id=sqlc.arg(environment_id) AND b.id=p.service_id AND b.protocol=p.protocol AND (b."listenPort"=p.port OR p.port IS NULL);

-- name: ReserveAppliedServicePorts :exec
UPDATE service_ports SET state='reserved',operation_id=sqlc.arg(operation_id) WHERE environment_id=sqlc.arg(environment_id)
 AND service_id=ANY(sqlc.arg(service_ids)::text[]);

-- name: ReleaseServicePorts :exec
DELETE FROM service_ports WHERE environment_id=$1;

-- name: GetAppliedServicePorts :many
SELECT p.*,n.endpoint FROM service_ports p JOIN nodes n ON n.id=p.node_id WHERE p.environment_id=$1 AND p.state='applied' ORDER BY p.service_id;

-- name: ListGatewayAddresses :many
SELECT host(gateway_address)::text AS address FROM environments WHERE network_node_id=$1 AND gateway_address IS NOT NULL;

-- name: SetGatewayAddress :exec
UPDATE environments SET gateway_address=sqlc.narg(address)::inet WHERE id=sqlc.arg(environment_id);
