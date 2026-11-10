package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/capture"
	"sync"
	"time"
)

func (s *Server) recentPackets(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	environment, err := s.Environments.Authorized(r.Context(), identity, r.PathValue("id"), "observe", "")
	if err != nil {
		return err
	}
	if environment.Status == "destroyed" || environment.Status == "destroying" {
		return httpError{http.StatusGone, "环境已销毁或正在销毁"}
	}
	query, err := capture.ParseRecentQuery(r.URL.Query())
	if err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	nodes, err := s.Queries.ListEnvironmentPacketNodes(r.Context(), environment.ID)
	if err != nil {
		return err
	}
	packetID := r.PathValue("packetId")
	if packetID != "" && packetID != "export" {
		node, _, _, parseErr := capture.ParsePacketIdentity(packetID)
		if parseErr != nil {
			return httpError{http.StatusBadRequest, parseErr.Error()}
		}
		for _, target := range nodes {
			if target.NodeID == node {
				_, err = s.proxyPacket(w, r, target.Endpoint, "/node/v1/environments/"+url.PathEscape(environment.ID)+"/packets/"+url.PathEscape(packetID))
				return err
			}
		}
		return httpError{http.StatusGone, "报文已不在留存范围内"}
	}
	if packetID == "export" {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		release, trackErr := s.trackConnection(ctx, &accessConnection{principal: identity.Principal.ID, environment: environment.ID, credential: credential(r), permission: "observe", close: cancel})
		if trackErr != nil {
			return trackErr
		}
		defer release()
		r = r.WithContext(ctx)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=packets.pcapng")
		if len(nodes) == 0 {
			return capture.EmptyPacketCapture(w)
		}
		var wrote int64
		for _, node := range nodes {
			var copied int64
			copied, err = s.proxyPacket(w, r, node.Endpoint, "/node/v1/environments/"+url.PathEscape(environment.ID)+"/packets/export")
			wrote += copied
			if err != nil {
				if wrote > 0 {
					panic(http.ErrAbortHandler)
				}
				return err
			}
		}
		return nil
	}
	pages := []api.RecentPacketPage{}
	var mutex sync.Mutex
	var waiting sync.WaitGroup
	for _, node := range nodes {
		waiting.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			page := api.RecentPacketPage{Packets: []api.CapturePacket{}, Errors: map[string]string{}}
			err := s.Nodes.Do(ctx, http.MethodGet, node.Endpoint, "/node/v1/environments/"+url.PathEscape(environment.ID)+"/packets?"+r.URL.RawQuery, nil, &page)
			if err != nil {
				page.Errors[node.Name] = err.Error()
				json.Unmarshal(node.Observation, &page.Retention)
				page.Retention.Status = api.PacketRetentionStatusUnavailable
				message := err.Error()
				page.Retention.Error = &message
			}
			mutex.Lock()
			pages = append(pages, page)
			mutex.Unlock()
		})
	}
	waiting.Wait()
	result := capture.MergePacketPages(pages, query.Limit)
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, result)
}
func (s *Server) proxyPacket(w http.ResponseWriter, r *http.Request, endpoint, path string) (int64, error) {
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint+path, nil)
	if err != nil {
		return 0, err
	}
	response, err := s.Nodes.HTTP.Do(request)
	if err != nil {
		return 0, httpError{http.StatusBadGateway, err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 16384))
		if readErr != nil {
			return 0, readErr
		}
		return 0, httpError{response.StatusCode, string(data)}
	}
	if response.Header.Get("Content-Type") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-store")
	n, err := io.Copy(w, response.Body)
	return n, err
}
func (s *Server) packetRetention(ctx context.Context, id string) (api.PacketRetention, error) {
	rows, err := s.Queries.ListEnvironmentPacketNodes(ctx, id)
	if err != nil {
		return api.PacketRetention{}, err
	}
	pages := []api.RecentPacketPage{}
	for _, row := range rows {
		var info api.PacketRetention
		if err = json.Unmarshal(row.Observation, &info); err != nil {
			return info, err
		}
		if row.State != "ready" || time.Since(row.UpdatedAt.Time) > 30*time.Second {
			info.Status = api.PacketRetentionStatusUnavailable
			message := "节点报文观测未更新"
			info.Error = &message
		}
		pages = append(pages, api.RecentPacketPage{Retention: info})
	}
	return capture.MergePacketPages(pages, 1).Retention, nil
}
