package access

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

// Restrict uses the authenticated integration account; no upstream user is persisted.
func (i Identity) Restrict(scope *api.RequestScope) (Identity, error) {
	if scope == nil {
		return i, nil
	}
	if i.Principal.Kind != "token" || !i.Administrator() {
		return Identity{}, ErrForbidden
	}
	name, err := validName(scope.Name)
	if err != nil {
		return Identity{}, err
	}
	grants := make([]queries.Grant, 0, len(scope.Grants))
	for _, grant := range scope.Grants {
		if grant.ScopeKind != api.ScopeGrantScopeKindProject && grant.ScopeKind != api.ScopeGrantScopeKindEnvironment && grant.ScopeKind != api.ScopeGrantScopeKindAsset {
			return Identity{}, InputError("未知授权范围")
		}
		if strings.TrimSpace(grant.ScopeId) == "" {
			return Identity{}, InputError("授权范围不能为空")
		}
		permissions, err := validPermissions(grant.Permissions)
		if err != nil {
			return Identity{}, err
		}
		grants = append(grants, queries.Grant{PrincipalID: i.Principal.ID, ScopeKind: string(grant.ScopeKind), ScopeID: grant.ScopeId, Permissions: permissions})
	}
	i.Principal.Name = name
	i.Principal.Administrator = scope.Administrator
	i.Grants = grants
	i.RequestScope = scope
	return i, nil
}

func ParseRequestScope(header string) (*api.RequestScope, error) {
	if header == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return nil, InputError("请求授权格式无效")
	}
	var scope api.RequestScope
	if err = json.Unmarshal(raw, &scope); err != nil {
		return nil, InputError("请求授权格式无效")
	}
	return &scope, nil
}

func (i Identity) GrantsJSON() []byte {
	grants := make([]api.ScopeGrant, 0, len(i.Grants))
	for _, grant := range i.Grants {
		grants = append(grants, api.ScopeGrant{ScopeKind: api.ScopeGrantScopeKind(grant.ScopeKind), ScopeId: grant.ScopeID, Permissions: permissionValues(grant.Permissions)})
	}
	raw, _ := json.Marshal(grants)
	return raw
}
