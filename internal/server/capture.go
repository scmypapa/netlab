package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) listCaptures(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "observe", ""); err != nil {
		return err
	}
	refs, err := s.Queries.ListCaptureSegments(r.Context(), id)
	if err != nil {
		return err
	}
	nodes := map[string]queries.ListCaptureSegmentsRow{}
	for _, ref := range refs {
		nodes[ref.NodeID] = ref
	}
	result := api.CaptureList{Segments: []api.CaptureSegment{}, Errors: map[string]string{}}
	var mutex sync.Mutex
	var waiting sync.WaitGroup
	for _, node := range nodes {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			var segments []api.CaptureSegment
			err := s.Nodes.Do(ctx, http.MethodGet, node.Endpoint, "/node/v1/environments/"+url.PathEscape(id)+"/captures", nil, &segments)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				result.Errors[node.Name] = err.Error()
				return
			}
			for _, ref := range refs {
				if ref.NodeID != node.NodeID {
					continue
				}
				found := false
				for _, segment := range segments {
					if segment.Id != ref.CaptureID {
						continue
					}
					name := node.Name
					segment.NodeName = &name
					result.Segments = append(result.Segments, segment)
					found = true
					break
				}
				if !found {
					message, name := "节点未启动此抓包", node.Name
					result.Segments = append(result.Segments, api.CaptureSegment{Id: ref.CaptureID, NodeId: node.NodeID, NodeName: &name, EnvironmentId: id, AssetIds: []string{}, StartedAt: ref.CreatedAt.Time, Status: api.CaptureSegmentStatusFailed, Error: &message})
				}
			}
		}()
	}
	waiting.Wait()
	slices.SortFunc(result.Segments, func(a, b api.CaptureSegment) int { return b.StartedAt.Compare(a.StartedAt) })
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) startCapture(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id := r.PathValue("id")
	environment, err := s.Environments.Authorized(r.Context(), identity, id, "observe", "")
	if err != nil {
		return err
	}
	if environment.Status == "destroying" || environment.Status == "destroyed" {
		return httpError{http.StatusConflict, "环境正在销毁或已销毁"}
	}
	var settings api.CreateCapture
	if err := decode(w, r, &settings); err != nil {
		return err
	}
	if len(settings.AssetIds) == 0 || settings.DurationSeconds < 10 || settings.DurationSeconds > 1800 || settings.FileSizeMiB < 1 || settings.FileSizeMiB > 1024 {
		return httpError{http.StatusBadRequest, "请选择资产；时长为 10–1800 秒，文件额度为 1–1024 MiB"}
	}
	interfaces, err := s.runtimeInterfaces(r.Context(), id)
	if err != nil {
		return err
	}
	nodeIDs := []string{}
	found := map[string]bool{}
	for _, iface := range interfaces {
		if slices.Contains(settings.AssetIds, iface.AssetId) {
			found[iface.AssetId] = true
			if !slices.Contains(nodeIDs, iface.NodeId) {
				nodeIDs = append(nodeIDs, iface.NodeId)
			}
		}
	}
	for _, asset := range settings.AssetIds {
		if !found[asset] {
			return httpError{http.StatusBadRequest, "所选资产没有可用运行接口"}
		}
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), nodeIDs)
	if err != nil {
		return err
	}
	request := api.NodeCaptureRequest{Id: uuid.NewString(), EnvironmentId: id, Settings: settings, Interfaces: interfaces}
	if err = s.Queries.RegisterCaptureSegments(r.Context(), queries.RegisterCaptureSegmentsParams{CaptureID: request.Id, EnvironmentID: id, Column3: nodeIDs}); err != nil {
		return err
	}
	result := api.CaptureList{Segments: []api.CaptureSegment{}, Errors: map[string]string{}}
	var mutex sync.Mutex
	var waiting sync.WaitGroup
	for _, node := range nodes {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			var segment api.CaptureSegment
			err := s.Nodes.Do(ctx, http.MethodPost, node.Endpoint, "/node/v1/environments/"+url.PathEscape(id)+"/captures", request, &segment)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				result.Errors[node.ID] = err.Error()
			} else {
				result.Segments = append(result.Segments, segment)
			}
		}()
	}
	waiting.Wait()
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) captureTarget(r *http.Request, identity access.Identity) (string, string, error) {
	if _, err := s.Environments.Authorized(r.Context(), identity, r.PathValue("id"), "observe", ""); err != nil {
		return "", "", err
	}
	if _, err := uuid.Parse(r.PathValue("captureId")); err != nil {
		return "", "", httpError{http.StatusBadRequest, "无效抓包标识"}
	}
	endpoint, err := s.Queries.GetCaptureEndpoint(r.Context(), queries.GetCaptureEndpointParams{EnvironmentID: r.PathValue("id"), NodeID: r.PathValue("nodeId"), CaptureID: r.PathValue("captureId")})
	if err != nil {
		return "", "", err
	}
	return endpoint, "/node/v1/environments/" + url.PathEscape(r.PathValue("id")) + "/captures/" + r.PathValue("captureId"), nil
}
func (s *Server) captureSegment(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	endpoint, path, err := s.captureTarget(r, identity)
	if err != nil {
		return err
	}
	var output any
	if r.Method == http.MethodGet {
		output = &api.CaptureDetail{}
	} else if r.Method == http.MethodPost {
		output = &api.CaptureSegment{}
	}
	if err := s.Nodes.Do(r.Context(), r.Method, endpoint, path, nil, output); err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	if output == nil {
		if err := s.Queries.DeleteCaptureSegment(r.Context(), queries.DeleteCaptureSegmentParams{EnvironmentID: r.PathValue("id"), NodeID: r.PathValue("nodeId"), CaptureID: r.PathValue("captureId")}); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return writeJSON(w, http.StatusOK, output)
}
func (s *Server) captureFile(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	endpoint, path, err := s.captureTarget(r, identity)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	release, err := s.trackConnection(ctx, &accessConnection{principal: identity.Principal.ID, environment: r.PathValue("id"), credential: credential(r), permission: "observe", close: cancel})
	if err != nil {
		return err
	}
	defer release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path+"/file", nil)
	if err != nil {
		return err
	}
	response, err := s.Nodes.HTTP.Do(request)
	if err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			return err
		}
		return httpError{response.StatusCode, string(raw)}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if size := response.Header.Get("Content-Length"); size != "" {
		w.Header().Set("Content-Length", size)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=capture-%s.pcapng", r.PathValue("captureId")))
	_, err = io.Copy(w, response.Body)
	if err != nil {
		slog.ErrorContext(ctx, "capture download interrupted", "capture", r.PathValue("captureId"), "error", err)
	}
	return nil
}
