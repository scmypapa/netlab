package environment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func Services(spec api.EnvironmentSpec) []api.ServiceExposure {
	if spec.Services == nil {
		return nil
	}
	return *spec.Services
}

func ServiceOnly(before, after api.EnvironmentSpec) bool {
	before.Services, after.Services = nil, nil
	return reflect.DeepEqual(before, after)
}

func ChangedServices(before, after api.EnvironmentSpec) []api.ServiceExposure {
	old := map[string]api.ServiceExposure{}
	for _, service := range Services(before) {
		old[service.Id] = service
	}
	changed := []api.ServiceExposure{}
	for _, service := range Services(after) {
		previous, exists := old[service.Id]
		delete(old, service.Id)
		if exists && reflect.DeepEqual(previous, service) {
			continue
		}
		changed = append(changed, service)
		if exists {
			changed = append(changed, previous)
		}
	}
	for _, service := range old {
		changed = append(changed, service)
	}
	return changed
}

func ChangedServiceAssets(before, after api.EnvironmentSpec) map[string]bool {
	assets := map[string]bool{}
	for _, service := range ChangedServices(before, after) {
		assets[service.AssetId] = true
	}
	return assets
}

func AuthorizeChange(identity access.Identity, row queries.Environment, before, after api.EnvironmentSpec) error {
	if err := AuthorizeHostBindings(identity, before, after); err != nil {
		return err
	}
	changed := ChangedServices(before, after)
	if (!ServiceOnly(before, after) || len(changed) == 0) && !identity.Allows("compose", row.ProjectID, row.ID, "", row.OwnerID) {
		return access.ErrForbidden
	}
	for _, service := range changed {
		// Removing an asset also removes its dependent mappings under the same compose permission.
		if !serviceTargetExists(after, service) {
			continue
		}
		if !identity.Allows("access", row.ProjectID, row.ID, service.AssetId, row.OwnerID) {
			return access.ErrForbidden
		}
	}
	return nil
}

func RemoveDependentServices(before, after api.EnvironmentSpec) api.EnvironmentSpec {
	old := map[string]api.ServiceExposure{}
	for _, service := range Services(before) {
		old[service.Id] = service
	}
	services := slices.DeleteFunc(slices.Clone(Services(after)), func(service api.ServiceExposure) bool {
		return reflect.DeepEqual(old[service.Id], service) && !serviceTargetExists(after, service)
	})
	if len(services) == 0 {
		after.Services = nil
	} else {
		after.Services = &services
	}
	return after
}

func serviceTargetExists(spec api.EnvironmentSpec, service api.ServiceExposure) bool {
	for _, asset := range spec.Assets {
		if asset.Id == service.AssetId {
			return slices.ContainsFunc(asset.Interfaces, func(nic api.Interface) bool { return nic.Id == service.InterfaceId })
		}
	}
	return false
}

func normalizeServices(spec api.EnvironmentSpec) (api.EnvironmentSpec, error) {
	services := slices.Clone(Services(spec))
	ids := map[string]bool{}
	for i := range services {
		service := &services[i]
		if service.Id == "" {
			service.Id = uuid.NewString()
		}
		if ids[service.Id] {
			return spec, Invalid("服务标识重复")
		}
		ids[service.Id] = true
		if !serviceTargetExists(spec, *service) {
			return spec, Invalid("服务引用的资产接口不存在")
		}
		if service.Protocol != "tcp" && service.Protocol != "udp" {
			return spec, Invalid("服务协议应为 TCP 或 UDP")
		}
		if service.TargetPort < 1 || service.TargetPort > 65535 || service.ListenPort != nil && (*service.ListenPort < 1 || *service.ListenPort > 65535) {
			return spec, Invalid("服务端口应在 1–65535 之间")
		}
	}
	if len(services) == 0 {
		spec.Services = nil
	} else {
		spec.Services = &services
	}
	return spec, nil
}

func (s Service) CreateService(ctx context.Context, identity access.Identity, id, assetID string, request api.CreateService) (api.Operation, error) {
	row, err := s.Authorized(ctx, identity, id, "access", assetID)
	if err != nil {
		return api.Operation{}, err
	}
	if previous, err := s.previousServiceRequest(ctx, identity, row, request.ClientRequestId); err != nil {
		return api.Operation{}, err
	} else if previous != nil {
		return *previous, nil
	}
	if row.AppliedSpec == nil {
		return api.Operation{}, Invalid("运行环境启动后可开放服务")
	}
	record, err := Record(row)
	if err != nil {
		return api.Operation{}, err
	}
	spec := record.Spec
	if record.AppliedSpec != nil {
		spec = *record.AppliedSpec
	}
	iface := ""
	for _, asset := range spec.Assets {
		if asset.Id != assetID {
			continue
		}
		for _, nic := range asset.Interfaces {
			if request.InterfaceId != nil && nic.Id == *request.InterfaceId || request.InterfaceId == nil && nic.Primary {
				iface = nic.Id
			}
		}
	}
	if iface == "" {
		return api.Operation{}, Invalid("请选择该资产的网络接口")
	}
	services := append(slices.Clone(Services(spec)), api.ServiceExposure{Id: uuid.NewString(), AssetId: assetID, InterfaceId: iface, Protocol: api.ServiceProtocol(request.Protocol), TargetPort: request.TargetPort, ListenPort: request.ListenPort})
	spec.Services = &services
	_, op, err := s.Change(ctx, identity, id, api.ChangeRequest{Apply: true, ExpectedRevision: request.ExpectedRevision, ClientRequestId: request.ClientRequestId, Spec: spec})
	if err != nil {
		return api.Operation{}, err
	}
	return *op, nil
}

func (s Service) DeleteService(ctx context.Context, identity access.Identity, id, serviceID string, revision int, requestID *string) (api.Operation, error) {
	row, err := s.Queries.GetEnvironment(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if previous, err := s.previousServiceRequest(ctx, identity, row, requestID); err != nil {
		return api.Operation{}, err
	} else if previous != nil {
		return *previous, nil
	}
	if row.AppliedSpec == nil {
		return api.Operation{}, Invalid("运行环境启动后可撤销服务")
	}
	record, err := Record(row)
	if err != nil {
		return api.Operation{}, err
	}
	spec := record.Spec
	if record.AppliedSpec != nil {
		spec = *record.AppliedSpec
	}
	services := slices.Clone(Services(spec))
	index := slices.IndexFunc(services, func(service api.ServiceExposure) bool { return service.Id == serviceID })
	if index < 0 {
		return api.Operation{}, pgx.ErrNoRows
	}
	if !identity.Allows("access", row.ProjectID, row.ID, services[index].AssetId, row.OwnerID) {
		return api.Operation{}, access.ErrForbidden
	}
	services = slices.Delete(services, index, index+1)
	spec.Services = &services
	_, op, err := s.Change(ctx, identity, id, api.ChangeRequest{Apply: true, ExpectedRevision: revision, ClientRequestId: requestID, Spec: spec})
	if err != nil {
		return api.Operation{}, err
	}
	return *op, nil
}

func (s Service) previousServiceRequest(ctx context.Context, identity access.Identity, row queries.Environment, requestID *string) (*api.Operation, error) {
	if requestID == nil {
		return nil, nil
	}
	op, err := s.Queries.GetOperationByRequest(ctx, queries.GetOperationByRequestParams{EnvironmentID: &row.ID, ClientRequestID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Spec       api.EnvironmentSpec  `json:"spec"`
		BeforeSpec *api.EnvironmentSpec `json:"beforeSpec"`
	}
	if err = json.Unmarshal(op.Payload, &payload); err != nil {
		return nil, err
	}
	if op.Kind != "change" || payload.BeforeSpec == nil || !ServiceOnly(*payload.BeforeSpec, payload.Spec) {
		return nil, ErrConflict
	}
	if err = AuthorizeChange(identity, row, *payload.BeforeSpec, payload.Spec); err != nil {
		return nil, err
	}
	result, err := Operation(op)
	return &result, err
}
