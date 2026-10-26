DROP VIEW node_asset_reservations;
DROP VIEW migration_asset_reservations;

ALTER TABLE runtime_assets ALTER COLUMN cpu TYPE numeric(12,2);
ALTER TABLE nodes DROP COLUMN capacity_override;
ALTER TABLE nodes ADD COLUMN cpu_allocation_ratio double precision NOT NULL DEFAULT 4
 CHECK (cpu_allocation_ratio >= 1 AND cpu_allocation_ratio < 'Infinity'::double precision);

CREATE VIEW migration_asset_reservations AS
SELECT o.scope_id AS environment_id,t->>'nodeId' AS node_id,t->'execution' AS execution,
 CASE WHEN EXISTS(SELECT 1 FROM runtime_assets a WHERE a.environment_id=o.environment_id AND a.asset_id=o.asset_id AND a.current AND a.node_id=t->>'nodeId') THEN 0 ELSE
 (t->'execution'->'asset'->'resources'->>'cpu')::numeric(12,2) END AS cpu,
 CASE WHEN EXISTS(SELECT 1 FROM runtime_assets a WHERE a.environment_id=o.environment_id AND a.asset_id=o.asset_id AND a.current AND a.node_id=t->>'nodeId') THEN 0 ELSE
 (t->'execution'->'asset'->'resources'->>'memoryMiB')::bigint END AS memory_mib,
 CASE WHEN t->'execution'->'rbd' IS NULL THEN
  (t->'execution'->'asset'->'resources'->>'diskGiB')::bigint + COALESCE((
   SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_array_elements(t->'execution'->'asset'->'volumes') v WHERE v->>'persistentVolumeId' IS NULL
  ),0) ELSE 0 END::bigint AS disk_gib,
 COALESCE((SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_each(t->'execution'->'volumeSources') AS volumes(name,v) WHERE v->'storage'->'rbd' IS NULL),0)::bigint AS volume_gib
FROM operations o CROSS JOIN LATERAL (SELECT o.payload->'migration'->'target' AS t) target
WHERE o.kind='migrate' AND t IS NOT NULL AND NOT COALESCE((o.payload->>'committed')::boolean,false) AND o.phase NOT IN ('complete','rolled-back');

CREATE VIEW node_asset_reservations AS
SELECT node_id,cpu,memory_mib,disk_gib FROM runtime_assets
UNION ALL SELECT node_id,cpu,memory_mib,disk_gib FROM migration_asset_reservations;
