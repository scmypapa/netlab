-- name: PendingEnvironmentOperations :one
SELECT EXISTS(SELECT 1 FROM operations o WHERE o.scope_kind='environment' AND o.scope_id=$1
 AND (o.state IN ('queued','running') OR (
  o.state IN ('failed','partially_applied')
  AND o.id=(SELECT operation_id FROM environments WHERE id=$1)
  AND o.phase NOT IN ('complete','rolled-back','cancelled')
  AND (o.phase IN ('rollback','migration-rollback') OR o.payload->'committed'='true'::jsonb
   OR o.kind='migrate' AND o.payload->'migration'->'target' IS NOT NULL
   OR o.kind='move-network' AND o.phase<>'queued')
 )));

-- name: CommitAssetMigration :execrows
UPDATE runtime_assets SET node_id=$3,execution=$4,state=$5,error=NULL,observed_at=$6,instance_id=sqlc.arg(target_instance)::text
WHERE instance_id=$1 AND node_id=$2 AND current;

-- name: MoveMigratedVolume :exec
UPDATE persistent_volumes SET node_id=$2,storage_pool_id=$3 WHERE id=$1;

-- name: CommitMigrationSpec :exec
UPDATE environments SET spec=$2,applied_spec=$3,revision=revision+1,updated_at=now() WHERE id=$1;
