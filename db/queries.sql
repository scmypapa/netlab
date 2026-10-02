-- name: GetPrincipalByName :one
SELECT * FROM principals WHERE name=$1;
-- name: GetCredential :one
SELECT p.* FROM credentials c JOIN principals p ON p.id=c.principal_id WHERE c.hash=$1 AND (c.expires_at IS NULL OR c.expires_at>now());
-- name: CreatePrincipal :exec
INSERT INTO principals(id,name,kind,password_hash,administrator) VALUES ($1,$2,$3,$4,$5);
-- name: CountPrincipals :one
SELECT count(*) FROM principals;
-- name: CreateCredential :exec
INSERT INTO credentials(hash,principal_id,expires_at) VALUES($1,$2,$3);
-- name: DeleteCredential :exec
DELETE FROM credentials WHERE hash=$1;
-- name: GetGrants :many
SELECT * FROM grants WHERE principal_id=$1;
-- name: PutGrant :exec
INSERT INTO grants(principal_id,scope_kind,scope_id,permissions) VALUES($1,$2,$3,$4) ON CONFLICT(principal_id,scope_kind,scope_id) DO UPDATE SET permissions=EXCLUDED.permissions;
-- name: ListEnvironments :many
SELECT * FROM environments WHERE status<>'destroyed' AND
 (sqlc.arg(is_admin)::boolean OR owner_id=sqlc.arg(principal_id) OR EXISTS
 (SELECT 1 FROM grants g WHERE g.principal_id=sqlc.arg(principal_id) AND 'read'=ANY(g.permissions) AND
 ((g.scope_kind='project' AND g.scope_id=project_id) OR (g.scope_kind='environment' AND g.scope_id=environments.id))))
 AND (sqlc.arg(cursor)::text='' OR id<sqlc.arg(cursor)) ORDER BY id DESC LIMIT sqlc.arg(page_limit);
-- name: GetEnvironment :one
SELECT * FROM environments WHERE id=$1;
-- name: GetEnvironmentByRequest :one
SELECT * FROM environments WHERE project_id=$1 AND client_request_id=$2;
-- name: CreateEnvironment :one
INSERT INTO environments(id,project_id,owner_id,name,external_reference,spec,client_request_id)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING *;
-- name: SaveView :exec
UPDATE environments SET view=$2,updated_at=now() WHERE id=$1;
-- name: SaveDraft :exec
UPDATE environments SET draft=$2,updated_at=now() WHERE id=$1;
-- name: SaveDesiredSpec :exec
UPDATE environments SET spec=$2,updated_at=now() WHERE id=$1;
-- name: SetEnvironmentOperation :exec
UPDATE environments SET operation_id=$2,status=$3,error=NULL,updated_at=now() WHERE id=$1;
-- name: LockEnvironment :one
SELECT * FROM environments WHERE id=$1 FOR UPDATE;
-- name: CommitEnvironment :exec
UPDATE environments SET applied_spec=$2,spec=$2,revision=revision+1,status=$3,draft=NULL,error=$4,updated_at=now() WHERE id=$1;
-- name: SetEnvironmentState :exec
UPDATE environments SET status=$2,error=$3,updated_at=now() WHERE id=$1;
-- name: SetNetworkOwner :exec
UPDATE environments SET network_node_id=$2 WHERE id=$1;
-- name: ListTemplates :many
SELECT * FROM templates ORDER BY created_at DESC,id;
-- name: GetTemplates :many
SELECT * FROM templates WHERE id=ANY($1::text[]);
-- name: CreateTemplate :exec
INSERT INTO templates(id,definition) VALUES($1,$2);
-- name: ListNodes :many
SELECT n.*,COALESCE(sum(a.cpu),0)::bigint AS reserved_cpu,COALESCE(sum(a.memory_mib),0)::bigint AS reserved_memory,COALESCE(sum(a.disk_gib),0)::bigint AS reserved_disk
FROM nodes n LEFT JOIN runtime_assets a ON a.node_id=n.id GROUP BY n.id ORDER BY n.id;
-- name: PutNode :exec
INSERT INTO nodes(id,name,endpoint,info) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,endpoint=EXCLUDED.endpoint,info=EXCLUDED.info,state='ready',observed_at=now();
-- name: SetNodeState :exec
UPDATE nodes SET state=$2,observed_at=CASE WHEN $2='ready' THEN now() ELSE observed_at END WHERE id=$1;
-- name: SetNodeCapacity :exec
UPDATE nodes SET capacity_override=$2 WHERE id=$1;
-- name: LockNode :one
SELECT * FROM nodes WHERE id=$1 FOR UPDATE;
-- name: GetReservedResources :one
SELECT COALESCE(sum(cpu),0)::bigint AS cpu,COALESCE(sum(memory_mib),0)::bigint AS memory_mib,COALESCE(sum(disk_gib),0)::bigint AS disk_gib FROM runtime_assets WHERE node_id=$1;
-- name: ListRuntimeAssets :many
SELECT * FROM runtime_assets WHERE environment_id=$1 ORDER BY asset_id,instance_id;
-- name: SetCurrentAsset :exec
UPDATE runtime_assets SET current=(instance_id=$3) WHERE environment_id=$1 AND asset_id=$2;
-- name: UpdateAssetExecution :exec
UPDATE runtime_assets SET execution=$2,cpu=$3,memory_mib=$4,disk_gib=$5 WHERE instance_id=$1;
-- name: ReserveAsset :exec
INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib)
VALUES($1,$2,$3,$4,$5,$6,$7,$8);
-- name: UpdateAssetResult :exec
UPDATE runtime_assets SET state=$2,error=$3,observed_at=$4 WHERE instance_id=$1;
-- name: ReleaseAsset :exec
DELETE FROM runtime_assets WHERE instance_id=$1;
-- name: GetOperation :one
SELECT * FROM operations WHERE id=$1;
-- name: GetOperationByRequest :one
SELECT * FROM operations WHERE environment_id=$1 AND client_request_id=$2;
-- name: ListOperations :many
SELECT * FROM operations WHERE environment_id=$1 ORDER BY created_at DESC LIMIT 100;
-- name: CreateOperation :one
INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,asset_id,payload,expected_revision,client_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *;
-- name: ClaimOperation :one
WITH candidate AS (
 SELECT o.id FROM operations o WHERE (o.state='queued' OR (o.state='running' AND o.lease_until<now()))
 AND NOT EXISTS(SELECT 1 FROM operations live WHERE live.scope_kind=o.scope_kind AND live.scope_id=o.scope_id AND live.id<>o.id AND live.state='running')
 ORDER BY o.created_at FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE operations o SET state='running',lease_owner=$1,lease_until=now()+interval '30 seconds',updated_at=now()
FROM candidate c WHERE o.id=c.id RETURNING o.*;
-- name: RenewLease :execrows
UPDATE operations SET lease_until=now()+interval '30 seconds' WHERE id=$1 AND lease_owner=$2 AND state='running';
-- name: SetOperationPhase :execrows
UPDATE operations SET phase=$3,payload=$4,updated_at=now() WHERE id=$1 AND lease_owner=$2;
-- name: FinishOperation :execrows
UPDATE operations SET state=$3,phase=$4,results=$5,error=$6,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_owner=$2;
-- name: AddEvent :one
INSERT INTO events(environment_id,kind,payload) VALUES($1,$2,$3) RETURNING cursor;
-- name: ReadEvents :many
SELECT * FROM events WHERE environment_id=$1 AND cursor>$2 ORDER BY cursor LIMIT 200;
-- name: UpdateTemplate :exec
UPDATE templates SET definition=$2 WHERE id=$1;
-- name: LockNodes :many
SELECT * FROM nodes WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE;
-- name: ReserveAssets :exec
INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib)
SELECT r."environmentId",r."assetId",r."instanceId",r."nodeId",r.execution,r.cpu,r."memoryMiB",r."diskGiB"
FROM jsonb_to_recordset($1::jsonb) AS r("environmentId" text,"assetId" text,"instanceId" text,"nodeId" text,execution jsonb,cpu integer,"memoryMiB" bigint,"diskGiB" bigint);
-- name: ApplyAssetResults :exec
UPDATE runtime_assets a SET state=r.state,error=r.error,observed_at=r."observedAt"
FROM jsonb_to_recordset($1::jsonb) AS r("instanceId" text,state text,error text,"observedAt" timestamptz)
WHERE a.instance_id=r."instanceId" AND a.observed_at<=r."observedAt";
-- name: ReleaseAssets :exec
DELETE FROM runtime_assets WHERE instance_id=ANY($1::text[]);
-- name: ClearCurrentAssets :exec
UPDATE runtime_assets SET current=false WHERE environment_id=$1 AND asset_id=ANY($2::text[]);
-- name: MakeCurrentAssets :exec
UPDATE runtime_assets SET current=true WHERE instance_id=ANY($1::text[]);
-- name: UpdateExecutions :exec
UPDATE runtime_assets a SET execution=r.execution,cpu=r.cpu,memory_mib=r."memoryMiB",disk_gib=r."diskGiB"
FROM jsonb_to_recordset($1::jsonb) AS r("instanceId" text,execution jsonb,cpu integer,"memoryMiB" bigint,"diskGiB" bigint)
WHERE a.instance_id=r."instanceId";
