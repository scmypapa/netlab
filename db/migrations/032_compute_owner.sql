DROP INDEX operations_compute_pending;

CREATE OR REPLACE VIEW node_asset_reservations AS
WITH planned AS MATERIALIZED (
 SELECT DISTINCT t->'execution'->>'instanceId' AS instance_id
 FROM operations o JOIN environments e ON e.operation_id=o.id
 CROSS JOIN LATERAL jsonb_array_elements(
  COALESCE(o.payload->'targets','[]'::jsonb) || COALESCE(o.payload->'updates','[]'::jsonb) ||
  COALESCE(o.payload->'unchanged','[]'::jsonb) ||
  CASE WHEN COALESCE((o.payload->>'committed')::boolean,false) THEN '[]'::jsonb ELSE COALESCE(o.payload->'before','[]'::jsonb) END
 ) t
 WHERE (o.state IN ('queued','running') OR o.state='failed' AND o.phase IN ('rollback','migration-rollback'))
 AND o.phase NOT IN ('queued','plan')
 AND t->>'state' IN ('running','suspended')
)
SELECT a.node_id,
 CASE WHEN a.state IN ('stopped','prepared') AND p.instance_id IS NULL THEN 0 ELSE a.cpu END::numeric(12,2) AS cpu,
 CASE WHEN a.state IN ('stopped','prepared') AND p.instance_id IS NULL THEN 0 ELSE a.memory_mib END::bigint AS memory_mib,
 a.disk_gib
FROM runtime_assets a LEFT JOIN planned p ON p.instance_id=a.instance_id
UNION ALL SELECT node_id,cpu,memory_mib,disk_gib+volume_gib FROM migration_asset_reservations;
