-- name: ListObservedNodes :many
SELECT id,endpoint FROM nodes ORDER BY id;

-- name: ApplyNodeObservation :exec
WITH previous_node AS MATERIALIZED (
 SELECT id,state FROM nodes WHERE id=sqlc.arg(node_id)::text
), node_ready AS (
 UPDATE nodes SET state='ready',observed_at=sqlc.arg(observed_at)::timestamptz
 WHERE id=sqlc.arg(node_id) RETURNING id
), reported AS MATERIALIZED (
 SELECT r."environmentId"::text AS environment_id,r."assetId"::text AS asset_id,r."instanceId"::text AS instance_id,r.state::text AS state,r.error::text AS error,r."observedAt"::timestamptz AS observed_at
 FROM jsonb_to_recordset(sqlc.arg(results)::jsonb)
 AS r("environmentId" text,"assetId" text,"instanceId" text,state text,error text,"observedAt" timestamptz)
), observations AS (
 SELECT environment_id,asset_id,instance_id,state,error,observed_at FROM reported
 UNION ALL
 SELECT a.environment_id,a.asset_id,a.instance_id,'absent','节点上已不存在该实例',sqlc.arg(observed_at)::timestamptz
 FROM runtime_assets a WHERE sqlc.arg(snapshot)::boolean AND a.node_id=sqlc.arg(node_id) AND a.current
 AND NOT EXISTS(SELECT 1 FROM reported r WHERE r.instance_id=a.instance_id AND r.asset_id=a.asset_id AND r.environment_id=a.environment_id)
), changed AS (
 UPDATE runtime_assets a SET state=r.state,error=r.error,observed_at=r.observed_at
 FROM observations r WHERE a.node_id=sqlc.arg(node_id) AND a.current
 AND a.environment_id=r.environment_id AND a.asset_id=r.asset_id AND a.instance_id=r.instance_id
 AND a.observed_at<r.observed_at
 RETURNING a.environment_id
), affected AS (
 SELECT environment_id FROM changed
 UNION
 SELECT a.environment_id FROM runtime_assets a,previous_node n WHERE a.node_id=n.id AND a.current AND n.state<>'ready'
)
INSERT INTO events(environment_id,kind,payload)
SELECT environment_id,'runtime.changed',jsonb_build_object('nodeId',sqlc.arg(node_id)::text) FROM affected;

-- name: MarkObservedNodeOffline :exec
WITH changed AS (
 UPDATE nodes SET state='offline' WHERE id=$1 AND state<>'offline' RETURNING id
)
INSERT INTO events(environment_id,kind,payload)
SELECT DISTINCT a.environment_id,'runtime.changed',jsonb_build_object('nodeId',a.node_id)
FROM runtime_assets a JOIN changed n ON n.id=a.node_id WHERE a.current;

-- name: ListRuntimeAssetStates :many
SELECT a.environment_id,a.asset_id,a.instance_id,a.node_id,a.current,a.observed_at,
 CASE WHEN n.state='ready' THEN a.state ELSE 'unknown' END::text AS state,
 COALESCE(CASE WHEN n.state='ready' THEN a.error ELSE '节点连接已断开' END,'')::text AS error
FROM runtime_assets a JOIN nodes n ON n.id=a.node_id WHERE a.environment_id=$1 ORDER BY a.asset_id,a.instance_id;

-- name: CurrentEventCursor :one
SELECT COALESCE(max(cursor),0)::bigint FROM events WHERE environment_id=$1;
