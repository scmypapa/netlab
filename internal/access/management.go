package access

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

type InputError string

func (e InputError) Error() string { return string(e) }

var operatorPermissions = []api.Permission{api.PermissionRead, api.PermissionOperate, api.PermissionSession, api.PermissionFile, api.PermissionObserve, api.PermissionNetwork, api.PermissionAccess}
var Permissions = append(slices.Clone(operatorPermissions), api.PermissionCompose, api.PermissionManage)
var Roles = []api.RolePreset{
	{Key: api.Viewer, Name: "查看者", Permissions: []api.Permission{api.PermissionRead}},
	{Key: api.Operator, Name: "操作员", Permissions: operatorPermissions},
	{Key: api.Manager, Name: "管理员", Permissions: Permissions},
}

func OperationPermission(kind string) string {
	switch kind {
	case "vpn-create", "vpn-revoke":
		return "access"
	case "change":
		return "compose"
	case "destroy", "rebuild", "capture-template", "capture-recovery", "delete-recovery", "restore-recovery", "clone-recovery", "create-backup", "delete-backup":
		return "manage"
	default:
		return "operate"
	}
}

func (i Identity) Permissions(project, environment, asset string, owner *string) []api.Permission {
	result := []api.Permission{}
	for _, permission := range Permissions {
		if i.Allows(string(permission), project, environment, asset, owner) {
			result = append(result, permission)
		}
	}
	return result
}

func Profile(i Identity) api.Identity {
	grants := make([]api.ScopeGrant, 0, len(i.Grants))
	for _, g := range i.Grants {
		grants = append(grants, api.ScopeGrant{ScopeKind: api.ScopeGrantScopeKind(g.ScopeKind), ScopeId: g.ScopeID, Permissions: permissionValues(g.Permissions)})
	}
	return api.Identity{Id: i.Principal.ID, Name: i.Principal.Name, Administrator: i.Administrator(), Roles: &Roles, Grants: &grants}
}

func permissionValues(values []string) []api.Permission {
	result := make([]api.Permission, len(values))
	for index, value := range values {
		result[index] = api.Permission(value)
	}
	return result
}

func validPermissions(values []api.Permission) ([]string, error) {
	if len(values) == 0 {
		return nil, InputError("请选择权限")
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(Permissions, value) {
			return nil, InputError("未知权限")
		}
		result = append(result, string(value))
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", InputError("名称应为 1–100 个字符")
	}
	return name, nil
}
func passwordHash(password string) ([]byte, error) {
	if len(password) < 8 || len(password) > 72 {
		return nil, InputError("密码应为 8–72 字节")
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func principal(p queries.Principal) api.Principal {
	return api.Principal{Id: p.ID, Name: p.Name, Kind: api.PrincipalKind(p.Kind), Administrator: p.Administrator, Disabled: p.Disabled, CreatedAt: p.CreatedAt.Time}
}

func (s Service) CreateUser(ctx context.Context, identity Identity, input api.CreateUser) (api.Principal, error) {
	if !identity.Administrator() {
		return api.Principal{}, ErrForbidden
	}
	name, err := validName(input.Name)
	if err != nil {
		return api.Principal{}, err
	}
	hash, err := passwordHash(input.Password)
	if err != nil {
		return api.Principal{}, err
	}
	id := uuid.NewString()
	if err = s.Queries.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: id, Name: name, Kind: "user", PasswordHash: hash}); err != nil {
		return api.Principal{}, err
	}
	p, err := s.Queries.GetPrincipal(ctx, id)
	return principal(p), err
}

func (s Service) UpdateUser(ctx context.Context, identity Identity, id string, input api.UpdateUser) error {
	if !identity.Administrator() {
		return ErrForbidden
	}
	name, err := validName(input.Name)
	if err != nil {
		return err
	}
	var hash []byte
	if input.Password != nil {
		hash, err = passwordHash(*input.Password)
		if err != nil {
			return err
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	p, err := q.LockPrincipal(ctx, id)
	if err != nil {
		return err
	}
	if p.Kind != "user" || p.Administrator {
		return ErrForbidden
	}
	if err = q.UpdateUser(ctx, queries.UpdateUserParams{ID: id, Name: name, PasswordHash: hash, Disabled: input.Disabled}); err != nil {
		return err
	}
	if input.Disabled || input.Password != nil {
		if err = q.DeletePrincipalCredentials(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s Service) CreateToken(ctx context.Context, identity Identity, input api.CreateServiceToken) (api.IssuedServiceToken, error) {
	if !identity.Administrator() {
		return api.IssuedServiceToken{}, ErrForbidden
	}
	name, err := validName(input.Name)
	if err != nil {
		return api.IssuedServiceToken{}, err
	}
	if len(input.Grants) == 0 {
		return api.IssuedServiceToken{}, InputError("请选择授权范围")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return api.IssuedServiceToken{}, InputError("有效期应晚于当前时间")
	}
	token, hash, err := newCredential()
	if err != nil {
		return api.IssuedServiceToken{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.IssuedServiceToken{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	id := uuid.NewString()
	if err = q.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: id, Name: name, Kind: "token"}); err != nil {
		return api.IssuedServiceToken{}, err
	}
	for _, g := range input.Grants {
		permissions, validation := validPermissions(g.Permissions)
		if validation != nil {
			return api.IssuedServiceToken{}, validation
		}
		if err = validateScope(ctx, q, g); err != nil {
			return api.IssuedServiceToken{}, err
		}
		if err = q.PutGrant(ctx, queries.PutGrantParams{PrincipalID: id, ScopeKind: string(g.ScopeKind), ScopeID: g.ScopeId, Permissions: permissions}); err != nil {
			return api.IssuedServiceToken{}, err
		}
	}
	expires := pgtype.Timestamptz{}
	if input.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *input.ExpiresAt, Valid: true}
	}
	if err = q.CreateCredential(ctx, queries.CreateCredentialParams{Hash: hash, PrincipalID: id, ExpiresAt: expires}); err != nil {
		return api.IssuedServiceToken{}, err
	}
	p, err := q.GetPrincipal(ctx, id)
	if err != nil {
		return api.IssuedServiceToken{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.IssuedServiceToken{}, err
	}
	return api.IssuedServiceToken{Principal: principal(p), Token: token}, nil
}

func validateScope(ctx context.Context, q *queries.Queries, g api.ScopeGrant) error {
	switch g.ScopeKind {
	case api.ScopeGrantScopeKindProject:
		exists, err := q.ProjectExists(ctx, g.ScopeId)
		if err != nil {
			return err
		}
		if !exists {
			return pgx.ErrNoRows
		}
		return nil
	case api.ScopeGrantScopeKindEnvironment, api.ScopeGrantScopeKindAsset:
		id, asset, _ := strings.Cut(g.ScopeId, "/")
		e, err := q.GetEnvironment(ctx, id)
		if err != nil {
			return err
		}
		if g.ScopeKind == api.ScopeGrantScopeKindEnvironment {
			if asset != "" {
				return InputError("环境范围无效")
			}
			return nil
		}
		return checkAssets(e, []string{asset})
	default:
		return InputError("未知授权范围")
	}
}

func checkAssets(e queries.Environment, ids []string) error {
	var spec api.EnvironmentSpec
	raw := e.AppliedSpec
	if raw == nil {
		raw = e.Spec
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(spec.Assets, func(a api.Asset) bool { return a.Id == id }) {
			return InputError("资产不属于当前环境")
		}
	}
	return nil
}

func (s Service) RevokeToken(ctx context.Context, identity Identity, id string) error {
	if !identity.Administrator() {
		return ErrForbidden
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	p, err := q.LockPrincipal(ctx, id)
	if err != nil {
		return err
	}
	if p.Kind != "token" {
		return InputError("该账号不是服务 Token")
	}
	if err = q.DeletePrincipalCredentials(ctx, id); err != nil {
		return err
	}
	if err = q.DeletePrincipalGrants(ctx, id); err != nil {
		return err
	}
	if err = q.DisablePrincipal(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s Service) Sharing(ctx context.Context, identity Identity, id string) (api.EnvironmentSharing, error) {
	e, err := s.Queries.GetEnvironment(ctx, id)
	if err != nil {
		return api.EnvironmentSharing{}, err
	}
	if !identity.Allows("manage", e.ProjectID, id, "", e.OwnerID) {
		return api.EnvironmentSharing{}, ErrForbidden
	}
	rows, err := s.Queries.ListEnvironmentGrants(ctx, id)
	if err != nil {
		return api.EnvironmentSharing{}, err
	}
	result := api.EnvironmentSharing{Grants: []api.EnvironmentGrant{}, Inherited: []api.EnvironmentGrant{}, Subjects: []api.Principal{}, OwnerId: e.OwnerID}
	for _, g := range rows {
		result.Grants = append(result.Grants, environmentGrant(queries.Grant{PrincipalID: g.PrincipalID, ScopeKind: g.ScopeKind, ScopeID: g.ScopeID, Permissions: g.Permissions}))
	}
	projects, err := s.Queries.ListProjectGrants(ctx, e.ProjectID)
	if err != nil {
		return result, err
	}
	for _, g := range projects {
		result.Inherited = append(result.Inherited, environmentGrant(queries.Grant{PrincipalID: g.PrincipalID, ScopeKind: g.ScopeKind, ScopeID: g.ScopeID, Permissions: g.Permissions}))
	}
	subjects, err := s.Queries.ListSharingSubjects(ctx)
	if err != nil {
		return result, err
	}
	for _, p := range subjects {
		result.Subjects = append(result.Subjects, api.Principal{Id: p.ID, Name: p.Name, Kind: api.PrincipalKind(p.Kind), Administrator: p.Administrator, Disabled: p.Disabled, CreatedAt: p.CreatedAt.Time})
	}
	return result, nil
}

func environmentGrant(g queries.Grant) api.EnvironmentGrant {
	result := api.EnvironmentGrant{PrincipalId: g.PrincipalID, Permissions: permissionValues(g.Permissions)}
	if g.ScopeKind == "asset" {
		_, id, _ := strings.Cut(g.ScopeID, "/")
		ids := []string{id}
		result.AssetIds = &ids
	}
	return result
}

func (s Service) ReplaceSharing(ctx context.Context, identity Identity, id string, input []api.EnvironmentGrant) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	e, err := q.LockEnvironment(ctx, id)
	if err != nil {
		return err
	}
	if !identity.Allows("manage", e.ProjectID, id, "", e.OwnerID) {
		return ErrForbidden
	}
	before, err := q.ListEnvironmentGrants(ctx, id)
	if err != nil {
		return err
	}
	grants := map[string]queries.PutGrantParams{}
	existing := map[string][]string{}
	for _, g := range before {
		existing[g.PrincipalID+"/"+g.ScopeID] = g.Permissions
	}
	ids := make([]string, 0, len(input))
	for _, g := range input {
		ids = append(ids, g.PrincipalId)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	principals, err := q.LockPrincipals(ctx, ids)
	if err != nil {
		return err
	}
	if len(principals) != len(ids) {
		return pgx.ErrNoRows
	}
	for _, g := range input {
		permissions, validation := validPermissions(g.Permissions)
		if validation != nil {
			return validation
		}
		kind, scopes := "environment", []string{id}
		if g.AssetIds != nil && len(*g.AssetIds) > 0 {
			if err = checkAssets(e, *g.AssetIds); err != nil {
				return err
			}
			kind, scopes = "asset", []string{}
			for _, asset := range *g.AssetIds {
				scopes = append(scopes, id+"/"+asset)
			}
		}
		for _, scope := range scopes {
			key := g.PrincipalId + "/" + scope
			if _, exists := grants[key]; exists {
				return InputError("同一主体的授权范围重复")
			}
			asset := ""
			if kind == "asset" {
				_, asset, _ = strings.Cut(scope, "/")
			}
			for _, permission := range permissions {
				if !identity.Allows(permission, e.ProjectID, id, asset, e.OwnerID) && !slices.Contains(existing[key], permission) {
					return ErrForbidden
				}
			}
			grants[key] = queries.PutGrantParams{PrincipalID: g.PrincipalId, ScopeKind: kind, ScopeID: scope, Permissions: permissions}
		}
	}
	if err = q.DeleteEnvironmentGrants(ctx, id); err != nil {
		return err
	}
	for _, g := range grants {
		if err = q.PutGrant(ctx, g); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
