CREATE VIEW node_asset_reservations AS
SELECT node_id,cpu,memory_mib,disk_gib FROM runtime_assets
UNION ALL
SELECT payload->'migration'->'target'->>'nodeId',
 (payload->'migration'->'target'->'execution'->'asset'->'resources'->>'cpu')::integer,
 (payload->'migration'->'target'->'execution'->'asset'->'resources'->>'memoryMiB')::bigint,
 0::bigint
FROM operations WHERE kind='migrate' AND payload->'migration'->'target' IS NOT NULL
 AND NOT COALESCE((payload->>'committed')::boolean,false) AND phase NOT IN ('complete','rolled-back');

-- An unresolved ownership handoff must finish before another environment operation.
CREATE FUNCTION require_completed_migration() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.scope_kind='environment' AND EXISTS (
  SELECT 1 FROM operations WHERE scope_kind='environment' AND scope_id=NEW.scope_id
   AND kind='migrate' AND phase NOT IN ('complete','rolled-back')
   AND (state IN ('queued','running') OR payload->'migration'->'target' IS NOT NULL)
 ) THEN
  RAISE EXCEPTION '请先完成当前设备迁移' USING ERRCODE='23505';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operations_migration_handoff BEFORE INSERT ON operations
FOR EACH ROW EXECUTE FUNCTION require_completed_migration();

CREATE INDEX operations_migration_active ON operations(scope_id)
WHERE kind='migrate' AND phase NOT IN ('complete','rolled-back');
