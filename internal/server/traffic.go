package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/capture"
)

func (s *Server) runtimeInterfaces(ctx context.Context, id string) ([]api.CaptureInterface, error) {
	assets, err := s.Queries.ListRuntimeAssets(ctx, id)
	if err != nil {
		return nil, err
	}
	interfaces := []api.CaptureInterface{}
	for _, asset := range assets {
		if !asset.Current {
			continue
		}
		var execution api.AssetExecution
		if err := json.Unmarshal(asset.Execution, &execution); err != nil {
			return nil, err
		}
		for _, iface := range execution.Interfaces {
			interfaces = append(interfaces, api.CaptureInterface{AssetId: asset.AssetID, AssetName: execution.Asset.Name, InterfaceId: iface.Id, NetworkId: iface.NetworkId, PortName: iface.PortName, NodeId: asset.NodeID, Mac: iface.Mac, Address: iface.Address})
		}
	}
	return interfaces, nil
}

func (s *Server) environmentTraffic(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "observe", ""); err != nil {
		return err
	}
	interfaces, err := s.runtimeInterfaces(r.Context(), id)
	if err != nil {
		return err
	}
	ids := []string{}
	for _, iface := range interfaces {
		if !slices.Contains(ids, iface.NodeId) {
			ids = append(ids, iface.NodeId)
		}
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), ids)
	if err != nil {
		return err
	}
	result := api.TrafficObservation{Flows: []api.CaptureFlow{}, Errors: map[string]string{}, ObservedAt: time.Now().UTC(), SamplingRate: capture.SamplingRate, WindowSeconds: capture.ObservationWindow}
	var mutex sync.Mutex
	var workers sync.WaitGroup
	for _, node := range nodes {
		workers.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			var observation api.TrafficObservation
			err := s.Nodes.Do(ctx, http.MethodPost, node.Endpoint, "/node/v1/traffic", interfaces, &observation)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				result.Errors[node.Name] = err.Error()
				return
			}
			result.Flows = append(result.Flows, observation.Flows...)
			result.OmittedSamples += observation.OmittedSamples
		})
	}
	workers.Wait()
	capture.SortFlows(result.Flows)
	return writeJSON(w, http.StatusOK, result)
}
