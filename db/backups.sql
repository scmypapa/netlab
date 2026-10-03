-- name: ListBackupRepositories :many
SELECT sqlc.embed(r),o.error FROM backup_repositories r LEFT JOIN operations o ON o.id=r.operation_id ORDER BY r.created_at,r.id;
-- name: GetBackupRepository :one
SELECT * FROM backup_repositories WHERE id=$1;
-- name: LockBackupRepository :one
SELECT * FROM backup_repositories WHERE id=$1 FOR UPDATE;
-- name: CreateBackupRepository :exec
INSERT INTO backup_repositories(id,node_id,name,location,credentials,operation_id) VALUES($1,$2,$3,$4,$5,$6);
-- name: CompleteBackupRepository :exec
UPDATE backup_repositories SET state=$2,native_id=$3 WHERE id=$1;
-- name: SetBackupRepositoryNativeID :exec
UPDATE backup_repositories SET native_id=$2 WHERE id=$1;
-- name: MarkBackupRepositoryConnecting :exec
UPDATE backup_repositories SET state='connecting',operation_id=$2 WHERE id=$1;
-- name: DeleteBackupRepository :exec
DELETE FROM backup_repositories WHERE id=$1;
-- name: BackupRepositoryInUse :one
SELECT EXISTS(SELECT 1 FROM backups b JOIN operations own ON own.id=b.operation_id WHERE b.repository_id=$1
AND (b.environment_id IS NOT NULL OR own.state IN ('queued','running') OR EXISTS
(SELECT 1 FROM operations o WHERE o.payload->>'backupId'=b.id AND o.scope_kind='environment'
AND (o.state IN ('queued','running') OR (o.phase IN ('rollback','cleanup') AND o.state IN ('failed','partially_applied'))))));
-- name: LockRepositoryBackups :many
SELECT id FROM backups WHERE repository_id=$1 ORDER BY id FOR UPDATE;
-- name: DeleteRepositoryCatalog :exec
DELETE FROM backups WHERE repository_id=$1 AND environment_id IS NULL;
-- name: ImportRepositoryBackups :exec
INSERT INTO backups(id,repository_id,name,definition,result,size_bytes,state,operation_id,created_at)
SELECT x.id,sqlc.arg(repository_id),x.name,x.definition,x.result,x.size_bytes,'ready',sqlc.arg(operation_id),x.created_at
FROM jsonb_to_recordset(sqlc.arg(records)::jsonb) AS x(id text,name text,definition jsonb,result jsonb,size_bytes bigint,created_at timestamptz)
ON CONFLICT(id) DO NOTHING;
-- name: RegisterBackupTemplates :exec
INSERT INTO templates(id,definition)
SELECT x->>'id',x FROM jsonb_array_elements(sqlc.arg(records)::jsonb) x
ON CONFLICT(id) DO UPDATE SET definition=jsonb_set(templates.definition,'{artifactNodeId}',EXCLUDED.definition->'artifactNodeId')
WHERE templates.definition->>'version'=EXCLUDED.definition->>'version' AND templates.definition->>'state'='ready';
-- name: ListBackups :many
SELECT b.id,b.environment_id,b.repository_id,b.name,b.state,b.size_bytes,b.operation_id,b.created_at,o.error
FROM backups b JOIN operations o ON o.id=b.operation_id
WHERE (sqlc.arg(environment_id)::text='' OR b.environment_id=sqlc.arg(environment_id)::text)
AND (sqlc.arg(repository_id)::text='' OR b.repository_id=sqlc.arg(repository_id)::text)
AND (sqlc.arg(cursor)::text='' OR (b.created_at,b.id)<(SELECT created_at,id FROM backups WHERE id=sqlc.arg(cursor)))
ORDER BY b.created_at DESC,b.id DESC LIMIT sqlc.arg(page_limit);
-- name: GetBackup :one
SELECT * FROM backups WHERE id=$1;
-- name: LockBackup :one
SELECT * FROM backups WHERE id=$1 FOR UPDATE;
-- name: CreateBackup :exec
INSERT INTO backups(id,environment_id,repository_id,name,definition,operation_id) VALUES($1,$2,$3,$4,$5,$6);
-- name: CompleteBackup :exec
UPDATE backups SET state=$2,result=$3,size_bytes=$4 WHERE id=$1;
-- name: MarkBackupDeleting :exec
UPDATE backups SET state='deleting',operation_id=$2 WHERE id=$1;
-- name: DeleteBackup :exec
DELETE FROM backups WHERE id=$1;
-- name: BackupInUse :one
SELECT EXISTS(SELECT 1 FROM operations WHERE payload->>'backupId'=$1::text AND scope_kind='environment'
AND (state IN ('queued','running') OR (phase IN ('rollback','cleanup') AND state IN ('failed','partially_applied'))));
-- name: MarkBackupCreating :exec
UPDATE backups SET state='creating' WHERE id=$1;
