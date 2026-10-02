package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
)

func TestAccessAPIWithPostgreSQL(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"access_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool, nil, nil)
	if err = s.Access.Bootstrap(ctx, "access-test-password"); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	call := func(method, path, token string, body any, status int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(method, httpServer.URL+"/api/v1"+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s: status=%d expected=%d body=%s", method, path, response.StatusCode, status, result)
		}
		return result
	}
	login := func(name, password string) string {
		t.Helper()
		raw, _ := json.Marshal(api.Login{Name: name, Password: password})
		response, err := http.Post(httpServer.URL+"/api/v1/sessions/login", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("login status=%d", response.StatusCode)
		}
		for _, cookie := range response.Cookies() {
			if cookie.Name == "netlab_session" {
				return cookie.Value
			}
		}
		t.Fatal("login cookie missing")
		return ""
	}
	admin := login("admin", "access-test-password")
	var user api.Principal
	if err = json.Unmarshal(call("POST", "/principals", admin, api.CreateUser{Name: "operator", Password: "operator-password"}, 201), &user); err != nil {
		t.Fatal(err)
	}
	if user.Administrator || user.Kind != api.PrincipalKindUser {
		t.Fatalf("ordinary user: %+v", user)
	}
	adminIdentity, err := s.Access.Authenticate(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,owner_id,name,spec) VALUES
	 ('env-a','default',$1,'A','{"networks":[{"id":"shared","name":"Shared","cidr":"10.0.0.0/24"},{"id":"hidden","name":"Hidden","cidr":"10.0.1.0/24"}],"assets":[{"id":"one","name":"One","templateId":"test","resources":{"cpu":1,"memoryMiB":512,"diskGiB":1},"interfaces":[{"id":"one-nic","networkId":"shared"}]},{"id":"two","name":"Two","templateId":"test","resources":{"cpu":1,"memoryMiB":512,"diskGiB":1},"interfaces":[{"id":"two-nic","networkId":"hidden"}]}]}'),
	 ('env-b','default',$1,'B','{"networks":[],"assets":[]}')`, adminIdentity.Principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	userSession := login("operator", "operator-password")
	call("GET", "/environments/env-a", userSession, nil, 403)
	call("POST", "/principals", userSession, api.CreateUser{Name: "other", Password: "other-password"}, 403)
	assets := []string{"one"}
	grants := []api.EnvironmentGrant{{PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionRead}}, {PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionOperate, api.PermissionSession}, AssetIds: &assets}}
	call("PUT", "/environments/env-a/grants", admin, grants, 204)
	call("GET", "/environments/env-a", userSession, nil, 200)
	call("GET", "/environments/env-b", userSession, nil, 403)
	call("GET", "/environments/env-a/grants", userSession, nil, 403)
	identity, err := s.Access.Authenticate(ctx, userSession)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.Allows("session", "default", "env-a", "one", nil) || identity.Allows("session", "default", "env-a", "two", nil) || identity.Allows("file", "default", "env-a", "one", nil) {
		t.Fatal("asset scope or file/session separation failed")
	}
	call("GET", "/environments/env-a/assets/one/console?kind=invalid", userSession, nil, 400)
	call("GET", "/environments/env-a/assets/two/console?kind=invalid", userSession, nil, 403)
	_, err = pool.Exec(ctx, `
	 INSERT INTO nodes(id,name,endpoint,info) VALUES('test-node','Test node','http://test','{}');
	 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current)
	 VALUES('env-a','one','instance-one','test-node','{}',1,512,1,true),('env-a','two','instance-two','test-node','{}',1,512,1,true);
	 INSERT INTO operations(id,environment_id,scope_kind,scope_id,kind,asset_id,payload,expected_revision)
	 VALUES('operation-one','env-a','environment','env-a','restart','one','{}',0),('operation-two','env-a','environment','env-a','restart','two','{}',0),('operation-all','env-a','environment','env-a','start',NULL,'{}',0);
	 UPDATE environments SET operation_id='operation-all',view='{"positions":{"one":{"x":0,"y":0},"two":{"x":200,"y":0}}}',draft=jsonb_build_object('baseRevision',0,'spec',spec),error='Two failed' WHERE id='env-a'`)
	if err != nil {
		t.Fatal(err)
	}
	call("PUT", "/environments/env-a/grants", admin, []api.EnvironmentGrant{{PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionRead, api.PermissionOperate, api.PermissionSession}, AssetIds: &assets}}, 204)
	if _, err = pool.Exec(ctx, `UPDATE operations SET kind='stop',client_request_id=CASE WHEN asset_id='one' THEN 'one-request' ELSE 'two-request' END WHERE asset_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	oneRequest, twoRequest := "one-request", "two-request"
	var repeated api.Operation
	if err = json.Unmarshal(call("POST", "/environments/env-a/assets/one/actions", userSession, api.ActionRequest{Action: api.ActionRequestActionStop, ClientRequestId: &oneRequest}, 202), &repeated); err != nil || repeated.Id != "operation-one" {
		t.Fatalf("same target request did not reuse its task: %v %+v", err, repeated)
	}
	call("POST", "/environments/env-a/assets/one/actions", userSession, api.ActionRequest{Action: api.ActionRequestActionStop, ClientRequestId: &twoRequest}, 409)
	call("POST", "/environments/env-a/assets/one/actions", userSession, api.ActionRequest{Action: api.ActionRequestActionStart, ClientRequestId: &oneRequest}, 409)
	call("POST", "/environments/env-a/changes", admin, api.ChangeRequest{Apply: true, ClientRequestId: &oneRequest}, 409)
	var scoped api.Environment
	if err = json.Unmarshal(call("GET", "/environments/env-a", userSession, nil, 200), &scoped); err != nil {
		t.Fatal(err)
	}
	if len(scoped.Spec.Assets) != 1 || scoped.Spec.Assets[0].Id != "one" || len(scoped.Spec.Networks) != 1 || scoped.Spec.Networks[0].Id != "shared" || scoped.Draft != nil || scoped.Error != nil || scoped.OperationId != nil || len(*scoped.View.Positions) != 1 {
		t.Fatalf("asset-scoped environment leaked other data: %+v", scoped)
	}
	var state api.EnvironmentState
	if err = json.Unmarshal(call("GET", "/environments/env-a/state", userSession, nil, 200), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Assets) != 1 || state.Assets[0].AssetId != "one" || state.Operation != nil {
		t.Fatalf("asset-scoped state leaked other assets: %+v", state)
	}
	var summaries []api.EnvironmentSummary
	if err = json.Unmarshal(call("GET", "/environments", userSession, nil, 200), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Id != "env-a" || summaries[0].AssetCount != 1 || summaries[0].NetworkCount != 1 {
		t.Fatalf("asset-scoped environment discovery failed: %+v", summaries)
	}
	var operations []api.Operation
	if err = json.Unmarshal(call("GET", "/operations?environmentId=env-a", userSession, nil, 200), &operations); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Id != "operation-one" {
		t.Fatalf("asset-scoped operations leaked other assets: %+v", operations)
	}
	call("GET", "/operations/operation-two", userSession, nil, 403)
	call("GET", "/operations/operation-all", userSession, nil, 403)
	t.Run("retry eligibility matches the actual scoped endpoint", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE operations SET state='failed' WHERE id='operation-one'; UPDATE environments SET operation_id='operation-one' WHERE id='env-a'`); err != nil {
			t.Fatal(err)
		}
		var listed []api.Operation
		if err := json.Unmarshal(call("GET", "/operations?environmentId=env-a", userSession, nil, 200), &listed); err != nil || len(listed) != 1 || !listed[0].Retryable {
			t.Fatalf("current failed asset task: operations=%+v error=%v", listed, err)
		}
		var detail api.Operation
		if err := json.Unmarshal(call("GET", "/operations/operation-one", userSession, nil, 200), &detail); err != nil || !detail.Retryable {
			t.Fatalf("task detail: operation=%+v error=%v", detail, err)
		}
		var runtime api.EnvironmentState
		if err := json.Unmarshal(call("GET", "/environments/env-a/state", userSession, nil, 200), &runtime); err != nil || runtime.Operation == nil || !runtime.Operation.Retryable {
			t.Fatalf("current state: state=%+v error=%v", runtime, err)
		}
		call("POST", "/operations/operation-one/retry", userSession, nil, 202)
		if err := json.Unmarshal(call("GET", "/operations/operation-one", userSession, nil, 200), &detail); err != nil || detail.Retryable {
			t.Fatalf("queued task: operation=%+v error=%v", detail, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE operations SET state='failed',kind='rebuild' WHERE id='operation-one'`); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(call("GET", "/operations/operation-one", userSession, nil, 200), &detail); err != nil || detail.Retryable {
			t.Fatalf("rebuild without manage: operation=%+v error=%v", detail, err)
		}
		call("POST", "/operations/operation-one/retry", userSession, nil, 403)
		if _, err := pool.Exec(ctx, `UPDATE operations SET kind='restart' WHERE id='operation-one'; UPDATE environments SET operation_id='operation-all',status='draft' WHERE id='env-a'`); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(call("GET", "/operations/operation-one", userSession, nil, 200), &detail); err != nil || detail.Retryable {
			t.Fatalf("superseded task: operation=%+v error=%v", detail, err)
		}
		call("POST", "/operations/operation-one/retry", userSession, nil, 409)
	})
	checkScopedEvents := func(token string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO events(environment_id,kind,payload) VALUES('env-a','operation.failed','{"assetId":"two","error":"hidden failure"}')`)
		if err != nil {
			t.Fatal(err)
		}
		streamCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(streamCtx, "GET", httpServer.URL+"/api/v1/environments/env-a/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Last-Event-ID", "0")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("asset-scoped events status=%d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		kind, payload := "", ""
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				payload = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if kind != "runtime.changed" || payload != "{}" {
			t.Fatalf("asset-scoped stream leaked event data: kind=%s payload=%s error=%v", kind, payload, scanner.Err())
		}
	}
	checkScopedEvents(userSession)
	call("GET", "/environments/env-a/assets/one/console?kind=invalid", userSession, nil, 400)
	foreign := []string{"foreign"}
	call("PUT", "/environments/env-a/grants", admin, []api.EnvironmentGrant{{PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionSession}, AssetIds: &foreign}}, 400)
	identity, err = s.Access.Authenticate(ctx, userSession)
	if err != nil || !identity.Allows("session", "default", "env-a", "one", nil) {
		t.Fatal("failed replacement changed existing grants")
	}
	var peer api.Principal
	if err = json.Unmarshal(call("POST", "/principals", admin, api.CreateUser{Name: "peer", Password: "peer-password"}, 201), &peer); err != nil {
		t.Fatal(err)
	}
	managerGrants := []api.EnvironmentGrant{{PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionRead, api.PermissionManage}}, {PrincipalId: peer.Id, Permissions: []api.Permission{api.PermissionRead, api.PermissionFile}}}
	call("PUT", "/environments/env-a/grants", admin, managerGrants, 204)
	call("PUT", "/environments/env-a/grants", userSession, managerGrants, 204)
	managerGrants[0].Permissions = append(managerGrants[0].Permissions, api.PermissionFile)
	call("PUT", "/environments/env-a/grants", userSession, managerGrants, 403)
	managerGrants[0].Permissions = []api.Permission{api.PermissionRead, api.PermissionManage}
	managerGrants[1].Permissions = append(managerGrants[1].Permissions, api.PermissionSession)
	call("PUT", "/environments/env-a/grants", userSession, managerGrants, 403)
	identity, err = s.Access.Authenticate(ctx, userSession)
	if err != nil || identity.Allows("file", "default", "env-a", "one", nil) {
		t.Fatal("shared manager escalated its permissions")
	}
	call("PUT", "/environments/env-a/grants", admin, []api.EnvironmentGrant{{PrincipalId: user.Id, Permissions: []api.Permission{api.PermissionRead, api.PermissionFile}, AssetIds: nil}}, 204)
	identity, err = s.Access.Authenticate(ctx, userSession)
	if err != nil || identity.Allows("session", "default", "env-a", "one", nil) || !identity.Allows("file", "default", "env-a", "one", nil) {
		t.Fatal("replacement did not take effect immediately")
	}
	call("GET", "/environments/env-a/assets/one/console?kind=invalid", userSession, nil, 403)
	call("PUT", "/principals/"+user.Id, admin, api.UpdateUser{Name: user.Name, Disabled: true}, 204)
	call("GET", "/identity", userSession, nil, 401)
	call("PUT", "/principals/"+user.Id, admin, api.UpdateUser{Name: user.Name, Disabled: false}, 204)
	userSession = login("operator", "operator-password")
	password := "changed-password"
	call("PUT", "/principals/"+user.Id, admin, api.UpdateUser{Name: user.Name, Password: &password}, 204)
	call("GET", "/identity", userSession, nil, 401)
	call("GET", "/identity", login("operator", password), nil, 200)
	var issued api.IssuedServiceToken
	if err = json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "automation", Grants: []api.ScopeGrant{{ScopeKind: api.ScopeGrantScopeKindEnvironment, ScopeId: "env-a", Permissions: []api.Permission{api.PermissionRead, api.PermissionSession}}}}, 201), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Token == "" || issued.Principal.Administrator {
		t.Fatal("token issuance is incomplete")
	}
	call("GET", "/environments/env-a", issued.Token, nil, 200)
	call("GET", "/environments/env-b", issued.Token, nil, 403)
	call("GET", "/principals", issued.Token, nil, 403)
	if strings.Contains(string(call("GET", "/principals?kind=token", admin, nil, 200)), issued.Token) {
		t.Fatal("token secret was returned again")
	}
	expiring := issued
	expiry := time.Now().Add(time.Hour)
	if err = json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "expiring", ExpiresAt: &expiry, Grants: []api.ScopeGrant{{ScopeKind: api.ScopeGrantScopeKindEnvironment, ScopeId: "env-a", Permissions: []api.Permission{api.PermissionRead}}}}, 201), &expiring); err != nil {
		t.Fatal(err)
	}
	expiringIdentity, err := s.Access.Authenticate(ctx, expiring.Token)
	if err != nil || expiringIdentity.ExpiresAt == nil {
		t.Fatal("credential expiry missing from identity")
	}
	if _, err = pool.Exec(ctx, `UPDATE credentials SET expires_at=now()-interval '1 second' WHERE principal_id=$1`, expiring.Principal.Id); err != nil {
		t.Fatal(err)
	}
	call("GET", "/identity", expiring.Token, nil, 401)
	call("DELETE", "/service-tokens/"+expiring.Principal.Id, admin, nil, 204)
	var assetToken api.IssuedServiceToken
	if err = json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "asset automation", Grants: []api.ScopeGrant{{ScopeKind: api.ScopeGrantScopeKindAsset, ScopeId: "env-a/one", Permissions: []api.Permission{api.PermissionRead}}}}, 201), &assetToken); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("GET", "/environments/env-a", assetToken.Token, nil, 200), &scoped); err != nil || len(scoped.Spec.Assets) != 1 || scoped.Spec.Assets[0].Id != "one" {
		t.Fatal("asset token did not use the filtered environment model")
	}
	call("GET", "/environments/env-b", assetToken.Token, nil, 403)
	checkScopedEvents(assetToken.Token)
	call("DELETE", "/service-tokens/"+assetToken.Principal.Id, admin, nil, 204)
	if _, err = pool.Exec(ctx, `UPDATE principals SET administrator=true WHERE id=$1`, issued.Principal.Id); err != nil {
		t.Fatal(err)
	}
	identity, err = s.Access.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Administrator() || identity.Allows("manage", "default", "env-b", "", nil) {
		t.Fatal("token inherited administrator privilege")
	}
	_, err = pool.Exec(ctx, `INSERT INTO environments(id,project_id,owner_id,name,spec) VALUES('token-origin','default',$1,'Token origin','{"assets":[],"networks":[]}')`, issued.Principal.Id)
	if err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/service-tokens/"+issued.Principal.Id, admin, nil, 204)
	call("GET", "/identity", issued.Token, nil, 401)
	var remaining int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM credentials WHERE principal_id=$1`, issued.Principal.Id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("credential cleanup count=%d error=%v", remaining, err)
	}
	if slices.Contains(identity.Permissions("default", "env-a", "one", nil), api.PermissionFile) {
		t.Fatal("ungranted file permission")
	}
	testServiceAccessAPI(t, ctx, pool, s, admin, call)
}
