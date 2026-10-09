-- name: GetPrincipal :one
SELECT * FROM principals WHERE id=$1;
-- name: LockPrincipal :one
SELECT * FROM principals WHERE id=$1 FOR UPDATE;
-- name: LockPrincipals :many
SELECT * FROM principals WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE;
-- name: ListPrincipals :many
SELECT p.id,p.name,p.kind,p.administrator,p.disabled,p.created_at,
 (SELECT max(c.expires_at) FROM credentials c WHERE c.principal_id=p.id AND p.kind='token')::timestamptz AS expires_at FROM principals p
WHERE (sqlc.arg(kind)::text='' OR p.kind=sqlc.arg(kind))
AND (sqlc.arg(search)::text='' OR p.name ILIKE '%'||sqlc.arg(search)||'%')
AND (sqlc.arg(cursor)::text='' OR p.id>sqlc.arg(cursor))
ORDER BY p.id LIMIT sqlc.arg(page_limit);
-- name: ListSharingSubjects :many
SELECT id,name,kind,administrator,disabled,created_at FROM principals ORDER BY name,id;
-- name: UpdateUser :exec
UPDATE principals SET name=$2,password_hash=COALESCE($3,password_hash),disabled=$4,administrator=COALESCE(sqlc.narg(administrator)::boolean,administrator) WHERE id=$1;
-- name: ActiveAdministrators :one
SELECT count(*) FROM principals WHERE kind='user' AND administrator AND NOT disabled;
-- name: DeletePrincipalCredentials :exec
DELETE FROM credentials WHERE principal_id=$1;
-- name: DeletePrincipalGrants :exec
DELETE FROM grants WHERE principal_id=$1;
-- name: DisablePrincipal :exec
UPDATE principals SET disabled=true WHERE id=$1;
-- name: ListEnvironmentGrants :many
SELECT g.*,p.name,p.kind FROM grants g JOIN principals p ON p.id=g.principal_id
WHERE (g.scope_kind='environment' AND g.scope_id=$1) OR (g.scope_kind='asset' AND g.scope_id LIKE $1||'/%')
ORDER BY p.name,g.scope_kind,g.scope_id;
-- name: ListProjectGrants :many
SELECT g.*,p.name,p.kind FROM grants g JOIN principals p ON p.id=g.principal_id
WHERE g.scope_kind='project' AND g.scope_id=$1 ORDER BY p.name;
-- name: DeleteEnvironmentGrants :exec
DELETE FROM grants WHERE (scope_kind='environment' AND scope_id=$1) OR (scope_kind='asset' AND scope_id LIKE $1||'/%');
-- name: ProjectExists :one
SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1);
