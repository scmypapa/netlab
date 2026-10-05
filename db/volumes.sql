-- name: ListPersistentVolumes :many
SELECT v.*,o.error AS operation_error FROM persistent_volumes v LEFT JOIN operations o ON o.id=v.operation_id
WHERE sqlc.arg(volume_id)::text='' OR v.id=sqlc.arg(volume_id) ORDER BY v.name,v.id;
-- name: GetPersistentVolume :one
SELECT * FROM persistent_volumes WHERE id=$1;
-- name: GetPersistentVolumes :many
SELECT * FROM persistent_volumes WHERE id=ANY($1::text[]);
-- name: LockPersistentVolumes :many
SELECT * FROM persistent_volumes WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE;
-- name: CreatePersistentVolume :exec
INSERT INTO persistent_volumes(id,node_id,storage_pool_id,name,kind,size_gib,state,operation_id) VALUES($1,$2,$3,$4,$5,$6,'creating',$7);
-- name: SetVolumeOperation :exec
UPDATE persistent_volumes SET state=$2,operation_id=$3 WHERE id=$1;
-- name: FinishVolume :exec
UPDATE persistent_volumes SET state=$2,size_gib=$3 WHERE id=$1;
-- name: DeletePersistentVolume :exec
DELETE FROM persistent_volumes WHERE id=$1;
-- name: PersistentVolumeReferences :many
WITH volumes AS (SELECT id FROM persistent_volumes WHERE id=ANY($1::text[]))
SELECT DISTINCT volume_id,name::text FROM (
 SELECT v.id AS volume_id,'环境：'||e.name AS name FROM volumes v JOIN environments e ON e.status<>'destroyed' AND (
 e.spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id))))) OR
 e.applied_spec @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id))))) OR
 e.draft->'spec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id))))))
 UNION SELECT v.id,'环境：'||e.name FROM volumes v JOIN runtime_assets a ON
 a.execution->'asset' @> jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id))) JOIN environments e ON e.id=a.environment_id
 UNION SELECT v.id,'环境：'||e.name FROM volumes v JOIN operations o ON o.state IN ('queued','running') AND
 o.payload->'spec' @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id))))) JOIN environments e ON e.id=o.environment_id
 UNION SELECT v.id,'恢复点：'||p.name FROM volumes v JOIN recovery_points p ON
 p.definition @> jsonb_build_object('assets',jsonb_build_array(jsonb_build_object('execution',jsonb_build_object('asset',jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id)))))))
) refs ORDER BY volume_id,name;
-- name: PersistentVolumeUses :many
WITH volumes AS (SELECT id FROM persistent_volumes WHERE id=ANY($1::text[]))
SELECT DISTINCT volume_id,environment_id,asset_id FROM (
 SELECT v.id AS volume_id,a.environment_id,a.asset_id FROM volumes v JOIN runtime_assets a ON
 a.execution->'asset' @> jsonb_build_object('volumes',jsonb_build_array(jsonb_build_object('persistentVolumeId',v.id)))
 UNION SELECT v.id,o.environment_id,a.value->>'id' AS asset_id FROM volumes v CROSS JOIN operations o
 CROSS JOIN LATERAL jsonb_path_query(o.payload,'$.spec.assets[*] ? (@.volumes[*].persistentVolumeId == $volume)',jsonb_build_object('volume',v.id)) a(value)
 WHERE o.state IN ('queued','running') AND o.scope_kind='environment'
) uses;
