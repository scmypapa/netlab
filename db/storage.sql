-- name: ListStoragePools :many
SELECT s.*,o.error AS operation_error,o.state AS operation_state FROM storage_pools s LEFT JOIN operations o ON o.id=s.operation_id ORDER BY s.id;
-- name: GetStoragePool :one
SELECT * FROM storage_pools WHERE id=$1;
-- name: LockStoragePools :many
SELECT * FROM storage_pools WHERE id=ANY($1::text[]) ORDER BY id FOR KEY SHARE;
-- name: LockStoragePool :one
SELECT * FROM storage_pools WHERE id=$1 FOR UPDATE;
-- name: CreateStoragePool :exec
INSERT INTO storage_pools(id,node_ids,name,driver,directory,path) VALUES($1,$2,$3,$4,$5,$6);
-- name: DeleteStoragePool :exec
DELETE FROM storage_pools WHERE id=$1;
-- name: MarkStorageDeleting :exec
UPDATE storage_pools SET state='deleting',operation_id=$2 WHERE id=$1;
-- name: CreateManagedStorage :exec
INSERT INTO storage_pools(id,node_ids,name,driver,directory,path,state,operation_id,managed)
VALUES($1,$2,'共享存储','rbd','','','preparing',$3,true);
-- name: ConfigureManagedStorage :exec
UPDATE storage_pools SET operation_id=$2 WHERE id=$1;
-- name: FinishManagedStorage :exec
UPDATE storage_pools SET node_ids=$2,path=$3,state='ready' WHERE id=$1;
-- name: StorageReservations :many
WITH assets AS (
 SELECT node_id,environment_id,asset_id,execution,disk_gib,COALESCE(execution->>'storagePoolId','')::text AS pool_id
 FROM runtime_assets WHERE node_id=ANY($1::text[])
), allocations AS (
 SELECT node_id,pool_id,sum(disk_gib)::bigint AS disk_gib FROM assets GROUP BY node_id,pool_id
 UNION ALL
 SELECT node_id,COALESCE(execution->>'storagePoolId','')::text,(disk_gib+volume_gib)::bigint
 FROM migration_asset_reservations WHERE node_id=ANY($1::text[])
 UNION ALL
 SELECT node_id,pool_id,-sum(reused)::bigint FROM (
  SELECT a.node_id,a.pool_id,a.environment_id,a.asset_id,v->>'id' AS volume_id,sum((v->>'sizeGiB')::bigint)-max((v->>'sizeGiB')::bigint) AS reused
  FROM assets a CROSS JOIN LATERAL jsonb_array_elements(a.execution->'asset'->'volumes') v WHERE v->>'persistentVolumeId' IS NULL
  GROUP BY a.node_id,a.pool_id,a.environment_id,a.asset_id,COALESCE(a.execution->>'dataSetId',''),v->>'id'
 ) volumes GROUP BY node_id,pool_id
 UNION ALL SELECT v.node_id,CASE WHEN v.storage_pool_id LIKE 'default:%' THEN '' ELSE v.storage_pool_id END,
 sum(CASE WHEN o.kind='resize-volume' AND v.state<>'ready' THEN GREATEST(v.size_gib,(o.payload->'volume'->>'sizeGiB')::bigint) ELSE v.size_gib END)::bigint
 FROM persistent_volumes v LEFT JOIN operations o ON o.id=v.operation_id
 WHERE v.node_id=ANY($1::text[]) GROUP BY v.node_id,v.storage_pool_id
)
SELECT node_id,pool_id,sum(disk_gib)::bigint AS disk_gib FROM allocations GROUP BY node_id,pool_id;
-- name: NodeStorageOperation :one
SELECT * FROM operations WHERE scope_kind='node' AND scope_id=$1 AND kind='configure-node-storage' ORDER BY created_at DESC,id DESC LIMIT 1;
-- name: StoragePoolAssets :many
SELECT e.id AS environment_id,e.name AS environment_name,e.revision,a.asset_id,
(a.execution->'asset'->>'name')::text AS asset_name,a.node_id,a.disk_gib AS size_gib,a.state
FROM runtime_assets a JOIN environments e ON e.id=a.environment_id
WHERE a.current AND COALESCE(a.execution->>'storagePoolId','default:'||a.node_id)=sqlc.arg(pool_id)::text
ORDER BY e.name,a.asset_id;
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
 UNION SELECT '恢复点：'||p.name FROM recovery_points p
 CROSS JOIN LATERAL jsonb_array_elements(p.definition->'assets') a(value)
 WHERE a.value->'execution'->>'storagePoolId'=sqlc.arg(pool_id)
 UNION SELECT '持久卷：'||v.name FROM persistent_volumes v WHERE v.storage_pool_id=sqlc.arg(pool_id)
 UNION SELECT '迁移任务：'||e.name FROM operations o JOIN environments e ON e.id=o.environment_id
 CROSS JOIN LATERAL jsonb_array_elements(COALESCE(o.payload->'before','[]')||jsonb_build_array(o.payload->'migration'->'target')) t
 WHERE o.kind='migrate' AND o.phase NOT IN ('complete','rolled-back') AND (
  t->'execution'->>'storagePoolId'=sqlc.arg(pool_id) OR EXISTS(
   SELECT 1 FROM jsonb_each(t->'execution'->'volumeSources') volume
   WHERE volume.value->'storage'->'rbd'->>'secretId'=sqlc.arg(pool_id)))
 UNION SELECT '待执行任务：'||COALESCE(e.name,o.kind) FROM operations o LEFT JOIN environments e ON e.id=o.environment_id
 WHERE o.state IN ('queued','running') AND (
  o.payload->'spec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))) OR
  o.payload->'beforeSpec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('storagePoolId',sqlc.arg(pool_id)::text))))
) refs ORDER BY name;
