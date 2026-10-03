-- name: ListStoragePools :many
SELECT s.*,o.error AS operation_error FROM storage_pools s LEFT JOIN operations o ON o.id=s.operation_id ORDER BY s.node_id,s.id;
-- name: GetStoragePool :one
SELECT * FROM storage_pools WHERE id=$1;
-- name: LockStoragePools :many
SELECT * FROM storage_pools WHERE id=ANY($1::text[]) ORDER BY id FOR KEY SHARE;
-- name: LockStoragePool :one
SELECT * FROM storage_pools WHERE id=$1 FOR UPDATE;
-- name: CreateStoragePool :exec
INSERT INTO storage_pools(id,node_id,name,directory,path) VALUES($1,$2,$3,$4,$5);
-- name: DeleteStoragePool :exec
DELETE FROM storage_pools WHERE id=$1;
-- name: MarkStorageDeleting :exec
UPDATE storage_pools SET state='deleting',operation_id=$2 WHERE id=$1;
-- name: StorageReservations :many
WITH assets AS (
 SELECT node_id,environment_id,asset_id,execution,disk_gib,COALESCE(execution->>'storagePoolId','')::text AS pool_id
 FROM runtime_assets WHERE node_id=ANY($1::text[])
), allocations AS (
 SELECT node_id,pool_id,sum(disk_gib)::bigint AS disk_gib FROM assets GROUP BY node_id,pool_id
 UNION ALL
 SELECT node_id,pool_id,-sum(reused)::bigint FROM (
  SELECT a.node_id,a.pool_id,a.environment_id,a.asset_id,v->>'id' AS volume_id,sum((v->>'sizeGiB')::bigint)-max((v->>'sizeGiB')::bigint) AS reused
  FROM assets a CROSS JOIN LATERAL jsonb_array_elements(a.execution->'asset'->'volumes') v
  GROUP BY a.node_id,a.pool_id,a.environment_id,a.asset_id,v->>'id'
 ) volumes GROUP BY node_id,pool_id
)
SELECT node_id,pool_id,sum(disk_gib)::bigint AS disk_gib FROM allocations GROUP BY node_id,pool_id;
-- name: StorageReferences :many
SELECT name::text FROM (
 SELECT '环境：'||e.name AS name FROM environments e WHERE e.status<>'destroyed' AND (
  e.spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))) OR
  e.applied_spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))) OR
  e.draft->'spec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))))
 UNION SELECT '运行资产：'||e.name FROM runtime_assets a JOIN environments e ON e.id=a.environment_id
 WHERE a.execution->>'storagePoolId'=sqlc.arg(pool_id)
 UNION SELECT '环境模板：'||b.name||' v'||v.version::text FROM blueprint_versions v JOIN blueprints b ON b.id=v.blueprint_id
 WHERE v.spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text)))
 UNION SELECT '待执行任务：'||COALESCE(e.name,o.kind) FROM operations o LEFT JOIN environments e ON e.id=o.environment_id
 WHERE o.state IN ('queued','running') AND (
  o.payload->'spec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))) OR
  o.payload->'beforeSpec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))))
) refs ORDER BY name;
