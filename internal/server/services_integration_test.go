package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/operation"
)

func testServiceAccessAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Server, admin string, call func(string, string, string, any, int) []byte) {
	t.Helper()
	id := uuid.NewString()
	template := api.Template{Id: id, Name: "Service fixture", Kind: api.Container, Os: "Linux", Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}}
	raw, _ := json.Marshal(template)
	if err := s.Queries.CreateTemplate(ctx, queries.CreateTemplateParams{ID: id, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	spec := api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "10.3.0.0/24"}}, Assets: []api.Asset{
		{Id: "web", Name: "Web", TemplateId: id, Interfaces: []api.Interface{{Id: "web-nic", NetworkId: "lan", Primary: true}}},
		{Id: "other", Name: "Other", TemplateId: id, Interfaces: []api.Interface{{Id: "other-nic", NetworkId: "lan", Primary: true}}},
	}}
	var env api.Environment
	if err := json.Unmarshal(call("POST", "/environments", admin, api.CreateEnvironment{Name: "Service fixture", Spec: &spec}, 201), &env); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE environments SET applied_spec=spec,status='running' WHERE id=$1", env.Id); err != nil {
		t.Fatal(err)
	}
	var issued api.IssuedServiceToken
	if err := json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "asset service access", Grants: []api.ScopeGrant{{ScopeKind: "asset", ScopeId: env.Id + "/web", Permissions: []api.Permission{"read", "access"}}}}, 201), &issued); err != nil {
		t.Fatal(err)
	}
	var task api.Operation
	request := api.CreateService{Protocol: "tcp", TargetPort: 80, ExpectedRevision: env.Revision}
	path := "/environments/" + env.Id
	if err := json.Unmarshal(call("POST", path+"/assets/web/services", issued.Token, request, 202), &task); err != nil || task.Total != 1 {
		t.Fatalf("service task: total=%d error=%v", task.Total, err)
	}
	call("POST", path+"/assets/other/services", issued.Token, request, 403)
	call("GET", "/operations/"+task.Id, issued.Token, nil, 200)
	var tasks []api.Operation
	if err := json.Unmarshal(call("GET", "/operations?environmentId="+env.Id, issued.Token, nil, 200), &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("asset service tasks: count=%d error=%v", len(tasks), err)
	}
	var row queries.Operation
	row, err := s.Queries.GetOperation(ctx, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	var payload operation.Payload
	if err = json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	identity, err := s.Access.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	row.State = "failed"
	if !operation.Retryable(identity, row, queries.Environment{ID: env.Id, ProjectID: "default", OperationID: &row.ID}) {
		t.Fatal("asset service operator could not retry its own mapping")
	}
	if _, err = pool.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", task.Id); err != nil {
		t.Fatal(err)
	}
	call("POST", "/operations/"+task.Id+"/retry", issued.Token, nil, 202)
	call("GET", "/operations/"+task.Id, issued.Token, nil, 200)
	services := *payload.Spec.Services
	reserved, err := s.Queries.ReserveServicePort(ctx, queries.ReserveServicePortParams{NodeID: "test-node", Protocol: "tcp", EnvironmentID: env.Id, ServiceID: services[0].Id, OperationID: task.Id})
	if err != nil || reserved.Port != nil || reserved.State != "reserved" {
		t.Fatalf("automatic reservation: %+v error=%v", reserved, err)
	}
	var endpoints []api.ServiceEndpoint
	if err = json.Unmarshal(call("GET", path+"/services", issued.Token, nil, 200), &endpoints); err != nil || len(endpoints) != 0 {
		t.Fatal("reserved mapping was exposed as applied")
	}
	port := int32(26000)
	manual := queries.ReserveServicePortParams{NodeID: "test-node", Protocol: "tcp", EnvironmentID: env.Id, ServiceID: "manual", OperationID: task.Id, RequestedPort: port}
	if _, err = s.Queries.ReserveServicePort(ctx, manual); err != nil {
		t.Fatal(err)
	}
	manual.ServiceID = "conflicting"
	if _, err = s.Queries.ReserveServicePort(ctx, manual); err == nil {
		t.Fatal("manual TCP port was registered twice")
	}
	manual.Protocol = "udp"
	if _, err = s.Queries.ReserveServicePort(ctx, manual); err != nil {
		t.Fatal("independent UDP port rejected", err)
	}
	bindings, _ := json.Marshal([]api.NodeServiceBinding{{Id: services[0].Id, Protocol: "tcp", ListenPort: 26001}})
	if err = s.Queries.ApplyServicePorts(ctx, queries.ApplyServicePortsParams{EnvironmentID: env.Id, Bindings: bindings}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(payload.Spec)
	if _, err = pool.Exec(ctx, "UPDATE environments SET applied_spec=$2 WHERE id=$1", env.Id, raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("GET", path+"/services", issued.Token, nil, 200), &endpoints); err != nil || len(endpoints) != 1 || endpoints[0].Port != 26001 || endpoints[0].Address != "192.0.2.10" {
		t.Fatalf("applied automatic port: %+v error=%v", endpoints, err)
	}
	services = append(services, api.ServiceExposure{Id: "hidden", AssetId: "other", InterfaceId: "other-nic", Protocol: "tcp", TargetPort: 80})
	manual.Protocol, manual.ServiceID, manual.RequestedPort = "tcp", "hidden", 26002
	if _, err = s.Queries.ReserveServicePort(ctx, manual); err != nil {
		t.Fatal(err)
	}
	bindings, _ = json.Marshal([]api.NodeServiceBinding{{Id: "hidden", Protocol: "tcp", ListenPort: 26002}})
	if err = s.Queries.ApplyServicePorts(ctx, queries.ApplyServicePortsParams{EnvironmentID: env.Id, Bindings: bindings}); err != nil {
		t.Fatal(err)
	}
	payload.Spec.Services = &services
	raw, _ = json.Marshal(payload.Spec)
	if _, err = pool.Exec(ctx, "UPDATE environments SET applied_spec=$2 WHERE id=$1", env.Id, raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(call("GET", path+"/services", issued.Token, nil, 200), &endpoints); err != nil || len(endpoints) != 1 || endpoints[0].AssetId != "web" {
		t.Fatal("asset service list leaked another asset mapping")
	}
	changed := payload.Spec
	changed.Assets[1].Name = "changed by service token"
	call("POST", path+"/changes", issued.Token, api.ChangeRequest{Spec: changed, ExpectedRevision: env.Revision}, 403)
}
