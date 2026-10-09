-- name: NodeOperation :one
SELECT * FROM operations WHERE scope_kind='node' AND scope_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1;
-- name: DrainNode :exec
UPDATE nodes SET retiring=true WHERE id=$1;
-- name: ResumeNode :exec
UPDATE nodes SET retiring=false WHERE id=$1;
-- name: NodeAssets :many
SELECT a.*,e.name AS environment_name,e.revision FROM runtime_assets a JOIN environments e ON e.id=a.environment_id
WHERE a.node_id=$1 ORDER BY a.environment_id,a.asset_id;
-- name: NodeDependencies :many
SELECT name::text FROM (
 SELECT '外部 LAN：'||e.name AS name FROM external_network_leases x JOIN environments e ON e.id=x.environment_id WHERE x.node_id=sqlc.arg(node_id)::text
 UNION SELECT '备份仓库：'||name FROM backup_repositories WHERE node_id=sqlc.arg(node_id)
 UNION SELECT '抓包文件：'||e.name FROM capture_segments c JOIN environments e ON e.id=c.environment_id WHERE c.node_id=sqlc.arg(node_id)
 UNION SELECT '恢复点：'||p.name FROM recovery_points p CROSS JOIN LATERAL jsonb_array_elements(p.definition->'assets') a
 WHERE a->>'nodeId'=sqlc.arg(node_id)
 UNION SELECT '未挂载数据卷：'||v.name FROM persistent_volumes v WHERE v.node_id=sqlc.arg(node_id)
 AND NOT EXISTS(SELECT 1 FROM runtime_assets a WHERE a.current AND a.node_id=v.node_id
 AND EXISTS(SELECT 1 FROM jsonb_each(a.execution->'volumeSources') source WHERE source.value->>'id'=v.id))
 UNION SELECT '待完成操作：'||COALESCE(e.name,o.kind) FROM operations o LEFT JOIN environments e ON e.id=o.environment_id
 WHERE o.state IN ('queued','running') AND o.kind<>'retire-node' AND (
 (o.scope_kind='node' AND o.scope_id=sqlc.arg(node_id)) OR
 EXISTS(SELECT 1 FROM runtime_assets a WHERE a.environment_id=o.environment_id AND a.node_id=sqlc.arg(node_id)) OR
 e.network_node_id=sqlc.arg(node_id) OR
 jsonb_path_exists(o.payload,'$.**.artifactNodeId ? (@ == $node)',jsonb_build_object('node',sqlc.arg(node_id)::text)) OR
 EXISTS(SELECT 1 FROM storage_pools p WHERE p.id=o.scope_id AND o.scope_kind='storage-pool' AND sqlc.arg(node_id)=ANY(p.node_ids)))
) dependencies ORDER BY name;
-- name: NodeNetworkEnvironments :many
SELECT * FROM environments e WHERE status<>'destroyed' AND (network_node_id=$1 OR EXISTS(
 SELECT 1 FROM operations o WHERE o.environment_id=e.id AND o.kind='move-network'
 AND o.payload->>'networkSource'=$1::text AND o.phase NOT IN ('complete','cancelled'))) ORDER BY id;
-- name: NodeArtifactTemplates :many
SELECT DISTINCT definition::jsonb FROM (
 SELECT definition FROM templates
 UNION SELECT execution->'template' FROM runtime_assets
 UNION SELECT a->'execution'->'template' FROM recovery_points p CROSS JOIN LATERAL jsonb_array_elements(p.definition->'assets') a
 UNION SELECT a->'execution'->'template' FROM backups b CROSS JOIN LATERAL jsonb_array_elements(b.definition->'recovery'->'assets') a
 UNION SELECT t FROM operations o CROSS JOIN LATERAL jsonb_path_query(o.payload,'$.**.template') t WHERE o.phase NOT IN ('complete','rolled-back','cancelled')
) versions WHERE definition->>'artifactNodeId'=$1::text;
-- name: RelocateTemplateArtifacts :exec
WITH templates_updated AS (
 UPDATE templates SET definition=relocate_template_artifacts(definition,sqlc.arg(source)::text,sqlc.arg(target)::text) WHERE definition->>'artifactNodeId'=sqlc.arg(source)
), assets_updated AS (
 UPDATE runtime_assets SET execution=relocate_template_artifacts(execution,sqlc.arg(source),sqlc.arg(target)) WHERE execution->'template'->>'artifactNodeId'=sqlc.arg(source)
), recoveries_updated AS (
 UPDATE recovery_points SET definition=relocate_template_artifacts(definition,sqlc.arg(source),sqlc.arg(target)) WHERE jsonb_path_exists(definition,'$.**.artifactNodeId ? (@ == $node)',jsonb_build_object('node',sqlc.arg(source)::text))
), backups_updated AS (
 UPDATE backups SET definition=relocate_template_artifacts(definition,sqlc.arg(source),sqlc.arg(target)) WHERE jsonb_path_exists(definition,'$.**.artifactNodeId ? (@ == $node)',jsonb_build_object('node',sqlc.arg(source)::text))
)
UPDATE operations SET payload=relocate_template_artifacts(payload,sqlc.arg(source),sqlc.arg(target)) WHERE kind<>'retire-node' AND phase NOT IN ('complete','rolled-back','cancelled')
AND jsonb_path_exists(payload,'$.**.artifactNodeId ? (@ == $node)',jsonb_build_object('node',sqlc.arg(source)::text));
-- name: ClaimOperationByID :one
UPDATE operations o SET state='running',lease_owner=sqlc.arg(owner),lease_until=now()+interval '30 seconds',updated_at=now()
WHERE o.id=sqlc.arg(id) AND (o.state='queued' OR o.state='running' AND o.lease_until<now())
AND NOT EXISTS(SELECT 1 FROM operations active WHERE active.id<>o.id AND active.scope_kind=o.scope_kind AND active.scope_id=o.scope_id AND active.state='running')
RETURNING *;
-- name: CancelNodeRetirement :exec
UPDATE operations SET state='failed',phase='cancelled',error='已恢复节点调度',updated_at=now()
WHERE scope_kind='node' AND scope_id=$1 AND kind='retire-node' AND state IN ('queued','failed','partially_applied');
-- name: DeleteRetiredNode :exec
WITH historical_environments AS (UPDATE environments SET network_node_id=NULL,gateway_address=NULL WHERE network_node_id=$1 AND status='destroyed')
DELETE FROM nodes n WHERE n.id=$1 AND n.retiring;
