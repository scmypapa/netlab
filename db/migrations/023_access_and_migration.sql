DROP TRIGGER principal_access_changed ON principals;
CREATE TRIGGER principal_access_changed AFTER UPDATE OF disabled, password_hash, administrator ON principals
  FOR EACH ROW EXECUTE FUNCTION notify_access_change();

CREATE OR REPLACE VIEW migration_asset_reservations AS
SELECT o.scope_id AS environment_id,t->>'nodeId' AS node_id,t->'execution' AS execution,
 CASE WHEN EXISTS(SELECT 1 FROM runtime_assets a WHERE a.environment_id=o.environment_id AND a.asset_id=o.asset_id AND a.current AND a.node_id=t->>'nodeId') THEN 0 ELSE
 (t->'execution'->'asset'->'resources'->>'cpu')::integer END AS cpu,
 CASE WHEN EXISTS(SELECT 1 FROM runtime_assets a WHERE a.environment_id=o.environment_id AND a.asset_id=o.asset_id AND a.current AND a.node_id=t->>'nodeId') THEN 0 ELSE
 (t->'execution'->'asset'->'resources'->>'memoryMiB')::bigint END AS memory_mib,
 CASE WHEN t->'execution'->'rbd' IS NULL THEN
  (t->'execution'->'asset'->'resources'->>'diskGiB')::bigint + COALESCE((
   SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_array_elements(t->'execution'->'asset'->'volumes') v WHERE v->>'persistentVolumeId' IS NULL
  ),0) ELSE 0 END::bigint AS disk_gib,
 COALESCE((SELECT sum((v->>'sizeGiB')::bigint) FROM jsonb_each(t->'execution'->'volumeSources') AS volumes(name,v) WHERE v->'storage'->'rbd' IS NULL),0)::bigint AS volume_gib
FROM operations o CROSS JOIN LATERAL (SELECT o.payload->'migration'->'target' AS t) target
WHERE o.kind='migrate' AND t IS NOT NULL AND NOT COALESCE((o.payload->>'committed')::boolean,false) AND o.phase NOT IN ('complete','rolled-back');
CREATE FUNCTION relocate_template_artifacts(value jsonb,source text,target text) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
 IF jsonb_typeof(value)='object' THEN
  RETURN COALESCE((SELECT jsonb_object_agg(key,CASE WHEN key='artifactNodeId' AND val=to_jsonb(source) THEN to_jsonb(target)
   ELSE relocate_template_artifacts(val,source,target) END) FROM jsonb_each(value) AS fields(key,val)),'{}'::jsonb);
 ELSIF jsonb_typeof(value)='array' THEN
  RETURN COALESCE((SELECT jsonb_agg(relocate_template_artifacts(item,source,target) ORDER BY ordinal)
   FROM jsonb_array_elements(value) WITH ORDINALITY AS items(item,ordinal)),'[]'::jsonb);
 END IF;
 RETURN value;
END $$;
ALTER TABLE nodes ADD COLUMN retiring boolean NOT NULL DEFAULT false;
