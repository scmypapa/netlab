-- name: ListRecoveryPoints :many
SELECT p.id,p.environment_id,p.name,p.revision,p.state,p.asset_count,p.size_bytes,p.operation_id,p.created_at,o.error,
CASE WHEN p.state='ready' AND p.definition->>'includeMemory'='true' THEN
  (SELECT count(*)::int FROM jsonb_array_elements(p.definition->'assets') a
   WHERE a->'execution'->'template'->>'kind'='vm' AND a->>'state' IN ('running','suspended'))
ELSE 0 END::int AS memory_asset_count
FROM recovery_points p JOIN operations o ON o.id=p.operation_id
WHERE p.environment_id=sqlc.arg(environment_id)
AND (sqlc.arg(cursor)::text='' OR (p.created_at,p.id)<(SELECT created_at,id FROM recovery_points WHERE id=sqlc.arg(cursor)))
ORDER BY p.created_at DESC,p.id DESC LIMIT sqlc.arg(page_limit);
-- name: LockRecoveryPoint :one
SELECT * FROM recovery_points WHERE id=$1 AND environment_id=$2 FOR UPDATE;
-- name: GetRecoveryPoint :one
SELECT * FROM recovery_points WHERE id=$1;
-- name: RecoveryPointInUse :one
SELECT EXISTS(SELECT 1 FROM operations o WHERE o.payload->'recovery'->>'id'=$1::text
AND (NOT (o.payload ? 'backupId') OR o.kind='create-backup')
AND (o.state IN ('queued','running') OR (o.phase IN ('rollback','cleanup') AND o.state IN ('failed','partially_applied'))
OR (o.kind='create-backup' AND o.state='failed' AND EXISTS(SELECT 1 FROM backups b WHERE b.operation_id=o.id))));
-- name: CreateRecoveryPoint :exec
INSERT INTO recovery_points(id,environment_id,name,revision,definition,asset_count,operation_id) VALUES($1,$2,$3,$4,$5,$6,$7);
-- name: CompleteRecoveryPoint :exec
UPDATE recovery_points SET state=$2,definition=$3,size_bytes=$4 WHERE id=$1;
-- name: MarkRecoveryDeleting :exec
UPDATE recovery_points SET state='deleting',operation_id=$2 WHERE id=$1;
-- name: MarkRecoveryCapturing :exec
UPDATE recovery_points SET state='capturing' WHERE id=$1;
-- name: DeleteRecoveryPoint :exec
DELETE FROM recovery_points WHERE id=$1;
-- name: SetRecoveryDeleteOperation :exec
UPDATE environments SET operation_id=$2,updated_at=now() WHERE id=$1;
