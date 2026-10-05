CREATE VIEW migration_asset_reservations AS
SELECT o.scope_id AS environment_id,t->>'nodeId' AS node_id,t->'execution' AS execution,
 (t->'execution'->'asset'->'resources'->>'cpu')::integer AS cpu,
 (t->'execution'->'asset'->'resources'->>'memoryMiB')::bigint AS memory_mib,
 CASE WHEN t->'execution'->'rbd' IS NULL THEN
  (t->'execution'->'asset'->'resources'->>'diskGiB')::bigint + COALESCE((
   SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_array_elements(t->'execution'->'asset'->'volumes') v WHERE v->>'persistentVolumeId' IS NULL
  ),0) ELSE 0 END::bigint AS disk_gib,
 COALESCE((SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_each(t->'execution'->'volumeSources') AS volumes(name,v) WHERE v->'storage'->'rbd' IS NULL),0)::bigint AS volume_gib
FROM operations o CROSS JOIN LATERAL (SELECT o.payload->'migration'->'target' AS t) target
WHERE o.kind='migrate' AND t IS NOT NULL AND NOT COALESCE((o.payload->>'committed')::boolean,false) AND o.phase NOT IN ('complete','rolled-back');

CREATE OR REPLACE VIEW node_asset_reservations AS
SELECT node_id,cpu,memory_mib,disk_gib FROM runtime_assets
UNION ALL SELECT node_id,cpu,memory_mib,disk_gib FROM migration_asset_reservations;
