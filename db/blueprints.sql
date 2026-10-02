-- name: CreateBlueprint :exec
INSERT INTO blueprints(id,project_id,owner_id,name) VALUES($1,$2,$3,$4);
-- name: NextBlueprintVersion :one
UPDATE blueprints SET version=version+1,updated_at=now() WHERE id=$1 RETURNING *;
-- name: CreateBlueprintVersion :one
INSERT INTO blueprint_versions(id,blueprint_id,version,spec,view,asset_count,network_count,source_environment_id,source_revision)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *;
-- name: GetBlueprint :one
SELECT b.*,v.id AS latest_version_id,v.asset_count,v.network_count FROM blueprints b
JOIN blueprint_versions v ON v.blueprint_id=b.id AND v.version=b.version WHERE b.id=$1;
-- name: ListBlueprints :many
SELECT b.*,v.id AS latest_version_id,v.asset_count,v.network_count FROM blueprints b
JOIN blueprint_versions v ON v.blueprint_id=b.id AND v.version=b.version
WHERE (sqlc.arg(is_admin)::boolean OR b.owner_id=sqlc.arg(principal_id) OR EXISTS
 (SELECT 1 FROM grants g WHERE g.principal_id=sqlc.arg(principal_id) AND g.scope_kind='project' AND g.scope_id=b.project_id AND 'read'=ANY(g.permissions)))
AND (sqlc.arg(cursor)::text='' OR b.id<sqlc.arg(cursor)) ORDER BY b.id DESC LIMIT sqlc.arg(page_limit);
-- name: GetBlueprintVersion :one
SELECT v.*,b.project_id,b.owner_id FROM blueprint_versions v JOIN blueprints b ON b.id=v.blueprint_id WHERE v.id=$1;
-- name: ListBlueprintVersions :many
SELECT v.id,v.blueprint_id,v.version,v.asset_count,v.network_count,v.created_at FROM blueprint_versions v
WHERE v.blueprint_id=$1 AND (sqlc.arg(cursor)::text='' OR v.version<(SELECT p.version FROM blueprint_versions p WHERE p.id=sqlc.arg(cursor) AND p.blueprint_id=$1))
ORDER BY v.version DESC LIMIT sqlc.arg(page_limit);
-- name: SetEnvironmentBlueprint :exec
UPDATE environments SET blueprint_version_id=$2 WHERE id=$1;
