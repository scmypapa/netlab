package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"time"
)

type Identity struct {
	Principal queries.Principal
	Grants    []queries.Grant
}
type Service struct{ Queries *queries.Queries }

var ErrUnauthorized = errors.New("请先登录")
var ErrForbidden = errors.New("无权执行此操作")

func (s Service) Bootstrap(ctx context.Context, password string) error {
	count, err := s.Queries.CountPrincipals(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if password == "" {
		return errors.New("首次启动请设置 NETLAB_ADMIN_PASSWORD")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.Queries.CreatePrincipal(ctx, queries.CreatePrincipalParams{ID: uuid.NewString(), Name: "admin", Kind: "user", PasswordHash: hash, Administrator: true})
}
func (s Service) Login(ctx context.Context, login api.Login) (api.Identity, string, error) {
	p, err := s.Queries.GetPrincipalByName(ctx, login.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Identity{}, "", ErrUnauthorized
	}
	if err != nil {
		return api.Identity{}, "", err
	}
	if p.Kind != "user" || bcrypt.CompareHashAndPassword(p.PasswordHash, []byte(login.Password)) != nil {
		return api.Identity{}, "", ErrUnauthorized
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return api.Identity{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	err = s.Queries.CreateCredential(ctx, queries.CreateCredentialParams{Hash: hash[:], PrincipalID: p.ID, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(12 * time.Hour), Valid: true}})
	return api.Identity{Id: p.ID, Name: p.Name, Administrator: p.Administrator}, token, err
}
func (s Service) Authenticate(ctx context.Context, token string) (Identity, error) {
	hash := sha256.Sum256([]byte(token))
	p, err := s.Queries.GetCredential(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthorized
	}
	if err != nil {
		return Identity{}, err
	}
	grants, err := s.Queries.GetGrants(ctx, p.ID)
	return Identity{Principal: p, Grants: grants}, err
}
func (s Service) Logout(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	return s.Queries.DeleteCredential(ctx, hash[:])
}
func (i Identity) Administrator() bool {
	return i.Principal.Kind == "user" && i.Principal.Administrator
}
func (i Identity) Allows(permission, project, environment, asset string, owner *string) bool {
	if i.Administrator() || (i.Principal.Kind == "user" && owner != nil && *owner == i.Principal.ID) {
		return true
	}
	for _, g := range i.Grants {
		match := (g.ScopeKind == "project" && g.ScopeID == project) || (g.ScopeKind == "environment" && g.ScopeID == environment) || (asset != "" && g.ScopeKind == "asset" && g.ScopeID == environment+"/"+asset)
		if match {
			for _, p := range g.Permissions {
				if p == permission {
					return true
				}
			}
		}
	}
	return false
}
