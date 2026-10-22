-- name: ListSecurityTargets :many
SELECT a.environment_id,a.node_id,a.execution FROM runtime_assets a
JOIN environments e ON e.id=a.environment_id
WHERE a.current AND a.state='running' AND e.status NOT IN ('destroying','destroyed');

-- name: GetSecurityCursor :one
SELECT cursor FROM security_sensors WHERE node_id=$1;

-- name: SaveSecurityObservation :execrows
INSERT INTO security_sensors(node_id,cursor,observation,checked_at) VALUES(sqlc.arg(node_id),sqlc.arg(cursor),sqlc.arg(observation),now())
ON CONFLICT(node_id) DO UPDATE SET cursor=EXCLUDED.cursor,observation=EXCLUDED.observation,checked_at=EXCLUDED.checked_at
WHERE security_sensors.cursor=sqlc.arg(expected_cursor);

-- name: ImportSecurityAlerts :exec
WITH input AS (
 SELECT item AS detail,item->>'environmentId' AS environment_id,item->>'nodeId' AS node_id,item->>'networkId' AS network_id,item->>'interfaceId' AS interface_id,
 (item->>'generatorId')::bigint AS generator_id,(item->>'signatureId')::bigint AS signature_id,
 item->>'source' AS source,item->>'destination' AS destination,(item->>'sourcePort')::integer AS source_port,(item->>'destinationPort')::integer AS destination_port,item->>'protocol' AS protocol,
 (item->>'severity')::integer AS severity,(item->>'count')::bigint AS count,(item->>'firstSeen')::timestamptz AS first_seen,(item->>'lastSeen')::timestamptz AS last_seen,
 date_trunc('minute',(item->>'firstSeen')::timestamptz) AS bucket
 FROM jsonb_array_elements(sqlc.arg(alerts)::jsonb) item
), grouped AS (
 SELECT environment_id,node_id,bucket,network_id,interface_id,generator_id,signature_id,source,destination,source_port,destination_port,protocol,
 min(severity) AS severity,sum(count)::bigint AS count,min(first_seen) AS first_seen,max(last_seen) AS last_seen,(jsonb_agg(detail)->0)::jsonb AS detail
 FROM input GROUP BY environment_id,node_id,bucket,network_id,interface_id,generator_id,signature_id,source,destination,source_port,destination_port,protocol
)
INSERT INTO security_alerts(environment_id,node_id,bucket,network_id,interface_id,generator_id,signature_id,source,destination,source_port,destination_port,protocol,severity,count,first_seen,last_seen,detail)
SELECT g.environment_id,g.node_id,g.bucket,g.network_id,g.interface_id,g.generator_id,g.signature_id,g.source,g.destination,g.source_port,g.destination_port,g.protocol,g.severity,g.count,g.first_seen,g.last_seen,g.detail
FROM grouped g JOIN environments e ON e.id=g.environment_id WHERE e.status<>'destroyed'
AND EXISTS(SELECT 1 FROM runtime_assets a CROSS JOIN LATERAL jsonb_array_elements(a.execution->'interfaces') i
 WHERE a.environment_id=g.environment_id AND a.node_id=g.node_id AND i->>'id'=g.interface_id)
ON CONFLICT(environment_id,node_id,bucket,network_id,interface_id,generator_id,signature_id,source,destination,source_port,destination_port,protocol)
DO UPDATE SET count=security_alerts.count+EXCLUDED.count,first_seen=LEAST(security_alerts.first_seen,EXCLUDED.first_seen),last_seen=GREATEST(security_alerts.last_seen,EXCLUDED.last_seen),severity=LEAST(security_alerts.severity,EXCLUDED.severity);

-- name: ListSecurityAlerts :many
SELECT id,count,first_seen,last_seen,severity,detail FROM security_alerts
WHERE environment_id=sqlc.arg(environment_id) AND last_seen>=sqlc.arg(start_at) AND first_seen<=sqlc.arg(end_at)
AND (sqlc.arg(asset_id)::text='' OR detail->>'sourceAssetId'=sqlc.arg(asset_id) OR detail->>'destinationAssetId'=sqlc.arg(asset_id))
AND (sqlc.arg(protocol)::text='' OR protocol=sqlc.arg(protocol))
AND (sqlc.arg(search)::text='' OR detail->>'signature' ILIKE '%'||sqlc.arg(search)||'%' OR detail->>'category' ILIKE '%'||sqlc.arg(search)||'%' OR source ILIKE '%'||sqlc.arg(search)||'%' OR destination ILIKE '%'||sqlc.arg(search)||'%')
AND (sqlc.arg(level)::text='' OR CASE WHEN severity=1 THEN 'high' WHEN severity=2 THEN 'medium' ELSE 'low' END=sqlc.arg(level))
AND (sqlc.arg(before_id)::bigint=0 OR id<sqlc.arg(before_id)) ORDER BY id DESC LIMIT 101;

-- name: CountSecurityAlerts :one
SELECT COALESCE(sum(count) FILTER(WHERE severity=1),0)::bigint AS high,
COALESCE(sum(count) FILTER(WHERE severity=2),0)::bigint AS medium,COALESCE(sum(count) FILTER(WHERE severity>=3),0)::bigint AS low
FROM security_alerts WHERE environment_id=sqlc.arg(environment_id) AND last_seen>=sqlc.arg(start_at) AND first_seen<=sqlc.arg(end_at)
AND (sqlc.arg(asset_id)::text='' OR detail->>'sourceAssetId'=sqlc.arg(asset_id) OR detail->>'destinationAssetId'=sqlc.arg(asset_id))
AND (sqlc.arg(protocol)::text='' OR protocol=sqlc.arg(protocol))
AND (sqlc.arg(search)::text='' OR detail->>'signature' ILIKE '%'||sqlc.arg(search)||'%' OR detail->>'category' ILIKE '%'||sqlc.arg(search)||'%' OR source ILIKE '%'||sqlc.arg(search)||'%' OR destination ILIKE '%'||sqlc.arg(search)||'%');

-- name: ListEnvironmentSecuritySensors :many
SELECT DISTINCT n.id,n.name,n.state,s.observation,s.checked_at FROM nodes n
JOIN runtime_assets a ON a.node_id=n.id AND a.current
LEFT JOIN security_sensors s ON s.node_id=n.id WHERE a.environment_id=$1 ORDER BY n.name,n.id;

-- name: PruneSecurityAlerts :exec
WITH pruned_events AS (DELETE FROM events WHERE kind='security.changed' AND created_at<now()-interval '30 days')
DELETE FROM security_alerts WHERE last_seen<now()-interval '30 days'
OR environment_id IN(SELECT id FROM environments WHERE status='destroyed');
