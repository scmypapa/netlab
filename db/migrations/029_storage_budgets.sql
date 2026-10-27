DROP VIEW node_asset_reservations;
DROP VIEW migration_asset_reservations;

ALTER TABLE runtime_assets ADD COLUMN system_gib bigint NOT NULL DEFAULT 0;
UPDATE runtime_assets SET system_gib=COALESCE((execution->'asset'->'resources'->>'diskGiB')::bigint,0);

-- Pending updates own both physical layouts until cleanup, not a fictitious larger guest disk.
WITH changes AS (
 SELECT o.kind,COALESCE((o.payload->>'committed')::boolean,false) AS committed,
  u->'execution' AS target,b->'execution' AS source,
  o.environment_id
 FROM environments e JOIN operations o ON o.id=e.operation_id
 CROSS JOIN LATERAL jsonb_array_elements(o.payload->'updates') u
 CROSS JOIN LATERAL jsonb_array_elements(o.payload->'before') b
 WHERE u->'execution'->>'instanceId'=b->'execution'->>'instanceId'
 AND o.phase NOT IN ('queued','complete','rolled-back','cancelled')
), budgets AS (
 SELECT *,CASE WHEN kind IN ('restore-recovery','clone-recovery')
  THEN (source->'asset'->'resources'->>'diskGiB')::bigint+(target->'asset'->'resources'->>'diskGiB')::bigint
  ELSE GREATEST((source->'asset'->'resources'->>'diskGiB')::bigint,(target->'asset'->'resources'->>'diskGiB')::bigint)
 END AS system_gib,
 (SELECT COALESCE(sum(size),0)::bigint FROM (
  SELECT CASE WHEN kind IN ('restore-recovery','clone-recovery') THEN sum((v->>'sizeGiB')::bigint) ELSE max((v->>'sizeGiB')::bigint) END AS size
  FROM jsonb_array_elements(COALESCE(source->'asset'->'volumes','[]')||COALESCE(target->'asset'->'volumes','[]')) v
  WHERE v->>'persistentVolumeId' IS NULL
  GROUP BY v->>'id'
 ) volumes) AS volume_gib
 FROM changes
)
UPDATE runtime_assets a SET system_gib=b.system_gib,disk_gib=b.system_gib+b.volume_gib,
 execution=CASE WHEN b.committed THEN b.target ELSE a.execution END
FROM budgets b WHERE a.environment_id=b.environment_id AND a.instance_id=b.target->>'instanceId';

ALTER TABLE runtime_assets ADD CONSTRAINT runtime_assets_system_budget CHECK (system_gib>=0 AND system_gib<=disk_gib);

CREATE VIEW migration_asset_reservations AS
WITH moves AS (
 SELECT o.environment_id,o.asset_id,o.phase,COALESCE((o.payload->>'committed')::boolean,false) AS committed,
  COALESCE((o.payload->'migration'->>'live')::boolean,false) AS live,
  s AS source,o.payload->'migration'->'target' AS target,
  COALESCE((o.payload->'recovery'->'captures'->o.asset_id->>'sizeBytes')::bigint,0) AS capture_bytes
 FROM operations o CROSS JOIN LATERAL jsonb_array_elements(o.payload->'before') s
 WHERE o.kind='migrate' AND o.payload->'migration'->'target' IS NOT NULL
 AND s->'execution'->'asset'->>'id'=o.asset_id
 AND o.phase NOT IN ('complete','rolled-back','cancelled')
), sides AS (
 SELECT *,CASE WHEN committed THEN source ELSE target END AS extra,
  CASE WHEN committed THEN target ELSE source END AS other
 FROM moves WHERE (NOT committed AND phase<>'migration-rollback')
 OR (committed AND phase IN ('migration-release-capture','migration-cleanup'))
), allocations AS (
 SELECT *,extra->'execution' AS execution,
  extra->'execution'->'rbd'->>'fsid' IS NOT NULL
  AND extra->'execution'->'rbd'->>'fsid'=other->'execution'->'rbd'->>'fsid'
  AND extra->'execution'->'rbd'->>'pool'=other->'execution'->'rbd'->>'pool'
  AND extra->'execution'->'rbd'->>'imagePrefix'=other->'execution'->'rbd'->>'imagePrefix' AS shared
 FROM sides
), roots AS (
 SELECT *,CASE WHEN shared AND extra->'execution'->>'instanceId'=other->'execution'->>'instanceId'
  AND COALESCE(extra->'execution'->>'dataSetId','')=COALESCE(other->'execution'->>'dataSetId','') THEN 0
  ELSE (execution->'asset'->'resources'->>'diskGiB')::bigint END AS system_gib,
  CASE WHEN shared AND COALESCE(execution->>'dataSetId','')=COALESCE(other->'execution'->>'dataSetId','') THEN 0
  ELSE COALESCE((SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_array_elements(execution->'asset'->'volumes') v WHERE v->>'persistentVolumeId' IS NULL),0)::bigint END AS anonymous_gib
 FROM allocations
), temporary AS (
 SELECT *,CASE WHEN capture_bytes>0 THEN (capture_bytes+1073741823)/1073741824
 ELSE (source->'execution'->'asset'->'resources'->>'diskGiB')::bigint
  +COALESCE((SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_array_elements(source->'execution'->'asset'->'volumes') v),0)::bigint END AS size_gib
 FROM moves WHERE NOT live AND source->'execution'->'template'->>'kind'='container'
)
SELECT environment_id,extra->>'nodeId' AS node_id,execution,
 CASE WHEN NOT committed AND extra->>'nodeId'<>other->>'nodeId' THEN (execution->'asset'->'resources'->>'cpu')::numeric(12,2) ELSE 0 END AS cpu,
 CASE WHEN NOT committed AND extra->>'nodeId'<>other->>'nodeId' THEN (execution->'asset'->'resources'->>'memoryMiB')::bigint ELSE 0 END AS memory_mib,
 system_gib+anonymous_gib AS disk_gib,0::bigint AS volume_gib,system_gib,
 COALESCE(execution->>'storagePoolId','')::text AS pool_id
FROM roots
UNION ALL
SELECT environment_id,extra->>'nodeId',execution,0,0,0,(v.value->>'sizeGiB')::bigint,0,
 COALESCE(v.value->>'storagePoolId',v.value->'storage'->'rbd'->>'secretId','')::text
FROM allocations a CROSS JOIN LATERAL jsonb_each(a.execution->'volumeSources') v
WHERE NOT COALESCE((
 (v.value->'storage'->'rbd'->>'fsid' IS NOT NULL
  AND v.value->'storage'->'rbd'->>'fsid'=other->'execution'->'volumeSources'->v.key->'storage'->'rbd'->>'fsid'
  AND v.value->'storage'->'rbd'->>'pool'=other->'execution'->'volumeSources'->v.key->'storage'->'rbd'->>'pool'
  AND v.value->'storage'->'rbd'->>'imagePrefix'=other->'execution'->'volumeSources'->v.key->'storage'->'rbd'->>'imagePrefix')
 OR (extra->>'nodeId'=other->>'nodeId' AND v.value->>'id'=other->'execution'->'volumeSources'->v.key->>'id'
  AND v.value->'storage'->>'path'=other->'execution'->'volumeSources'->v.key->'storage'->>'path')
),false)
UNION ALL
SELECT environment_id,source->>'nodeId',source->'execution',0,0,size_gib,0,0,
 COALESCE(source->'execution'->>'storagePoolId','')::text FROM temporary
WHERE phase NOT IN ('migration-cleanup','migration-services')
UNION ALL
SELECT environment_id,target->>'nodeId',target->'execution',0,0,size_gib,0,0,
 COALESCE(target->'execution'->>'storagePoolId','')::text FROM temporary
WHERE phase IN ('migration-stop','migration-capture','migration-copy');

CREATE VIEW node_asset_reservations AS
SELECT node_id,cpu,memory_mib,disk_gib FROM runtime_assets
UNION ALL SELECT node_id,cpu,memory_mib,disk_gib+volume_gib FROM migration_asset_reservations;
