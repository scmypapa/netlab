//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"netlab.local/core/api"
)

func (e *Engine) CephStatus(ctx context.Context, id string) (api.CephStatus, error) {
	result := api.CephStatus{Messages: []string{}, Disks: []api.CephDisk{}, Daemons: []api.CephDaemon{}}
	out, err := e.managedCeph(ctx, id, "status", "--format", "json")
	if err != nil {
		return result, err
	}
	var status struct {
		Health struct {
			Status string
			Checks map[string]struct{ Summary struct{ Message string } }
		}
		Osdmap struct {
			Total int `json:"num_osds"`
			Up    int `json:"num_up_osds"`
		}
	}
	if err = json.Unmarshal(out, &status); err != nil {
		return result, err
	}
	result.Health, result.OsdsTotal, result.OsdsUp = status.Health.Status, status.Osdmap.Total, status.Osdmap.Up
	for _, check := range status.Health.Checks {
		result.Messages = append(result.Messages, check.Summary.Message)
	}
	slices.Sort(result.Messages)
	out, err = e.managedCeph(ctx, id, "osd", "pool", "get", "netlab", "size", "--format", "json")
	if err != nil {
		return result, err
	}
	var pool struct{ Size int }
	if err = json.Unmarshal(out, &pool); err != nil {
		return result, err
	}
	result.Replicas = pool.Size
	out, err = e.managedCeph(ctx, id, "osd", "df", "tree", "--format", "json")
	if err != nil {
		return result, err
	}
	var tree struct {
		Nodes []struct {
			Name     string
			Type     string
			Status   string
			Size     int64 `json:"kb"`
			Used     int64 `json:"kb_used"`
			Children []int
			ID       int
		}
	}
	if err = json.Unmarshal(out, &tree); err != nil {
		return result, err
	}
	hosts := map[int]string{}
	for _, item := range tree.Nodes {
		if item.Type == "host" {
			for _, child := range item.Children {
				hosts[child] = item.Name
			}
		}
	}
	for _, item := range tree.Nodes {
		if item.Type == "osd" {
			result.Disks = append(result.Disks, api.CephDisk{Name: item.Name, Host: hosts[item.ID], State: item.Status, CapacityBytes: item.Size * 1024, UsedBytes: item.Used * 1024})
		}
	}
	out, err = e.managedCeph(ctx, id, "orch", "ps", "--format", "json")
	if err != nil {
		return result, err
	}
	var daemons []struct {
		Name  string `json:"daemon_name"`
		Host  string `json:"hostname"`
		Role  string `json:"daemon_type"`
		State string `json:"status_desc"`
	}
	if err = json.Unmarshal(out, &daemons); err != nil {
		return result, err
	}
	for _, d := range daemons {
		result.Daemons = append(result.Daemons, api.CephDaemon{Name: d.Name, Host: d.Host, Role: d.Role, State: d.State})
	}
	slices.SortFunc(result.Daemons, func(a, b api.CephDaemon) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}
