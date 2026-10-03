package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
)

func (s *Server) listServices(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	row, visible, err := s.Environments.Readable(r.Context(), identity, r.PathValue("id"))
	if err != nil {
		return err
	}
	var spec api.EnvironmentSpec
	if row.AppliedSpec != nil {
		if err = json.Unmarshal(row.AppliedSpec, &spec); err != nil {
			return err
		}
	}
	declared := map[string]api.ServiceExposure{}
	for _, service := range environment.Services(spec) {
		declared[service.Id] = service
	}
	ports, err := s.Queries.GetAppliedServicePorts(r.Context(), row.ID)
	if err != nil {
		return err
	}
	result := []api.ServiceEndpoint{}
	for _, port := range ports {
		service, exists := declared[port.ServiceID]
		if !exists || visible != nil && !visible[service.AssetId] {
			continue
		}
		var info api.NodeInfo
		if err := json.Unmarshal(port.NodeInfo, &info); err != nil {
			return err
		}
		if info.AccessAddress == nil || *info.AccessAddress == "" {
			return fmt.Errorf("节点未声明业务访问地址")
		}
		result = append(result, api.ServiceEndpoint{Id: service.Id, AssetId: service.AssetId, InterfaceId: service.InterfaceId, Protocol: service.Protocol, TargetPort: service.TargetPort, ListenPort: service.ListenPort, Address: *info.AccessAddress, Port: int(*port.Port), UpdatedAt: port.UpdatedAt.Time})
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	var request api.CreateService
	if err := decode(w, r, &request); err != nil {
		return err
	}
	result, err := s.Environments.CreateService(r.Context(), identity, r.PathValue("id"), r.PathValue("assetId"), request)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	revision, err := strconv.Atoi(r.URL.Query().Get("expectedRevision"))
	if err != nil || revision < 0 {
		return httpError{http.StatusBadRequest, "请提供 expectedRevision"}
	}
	var requestID *string
	if value := r.URL.Query().Get("clientRequestId"); value != "" {
		requestID = &value
	}
	result, err := s.Environments.DeleteService(r.Context(), identity, r.PathValue("id"), r.PathValue("serviceId"), revision, requestID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, result)
}
