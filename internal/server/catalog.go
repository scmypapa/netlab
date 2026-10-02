package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	ids := []string{}
	if raw := r.URL.Query().Get("ids"); raw != "" {
		ids = strings.Split(raw, ",")
	}
	rows, err := s.Queries.ListTemplatePage(r.Context(), queries.ListTemplatePageParams{Cursor: cursor, PageLimit: limit, Search: strings.TrimSpace(r.URL.Query().Get("search")), Kind: r.URL.Query().Get("kind"), Ids: ids})
	if err != nil {
		return err
	}
	result := make([]api.Template, 0, len(rows))
	for _, row := range rows {
		var item api.Template
		if err = json.Unmarshal(row.Definition, &item); err != nil {
			return err
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.Template
	if err := decode(w, r, &input); err != nil {
		return err
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Source) == "" {
		return httpError{http.StatusBadRequest, "请填写模板名称与镜像地址"}
	}
	if input.Kind != api.Container && input.Kind != api.Vm {
		return httpError{http.StatusBadRequest, "模板类型应为 container 或 vm"}
	}
	if input.Resources.Cpu < 1 || input.Resources.MemoryMiB < 64 || input.Resources.DiskGiB < 1 {
		return httpError{http.StatusBadRequest, "模板规格应至少为 1 核、64 MiB 内存、1 GiB 磁盘"}
	}
	if input.Kind == api.Vm && input.Hardware == nil && (input.Format == nil || (*input.Format != "ova" && *input.Format != "ovf")) {
		return httpError{http.StatusBadRequest, "虚拟机模板需要虚拟硬件配置"}
	}
	if input.Id == "" {
		input.Id = uuid.NewString()
	}
	if input.Version == 0 {
		input.Version = 1
	}
	state := api.Importing
	input.State, input.Error = &state, nil
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Template api.Template `json:"template"`
	}{input})
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	q := s.Queries.WithTx(tx)
	if err = q.CreateTemplate(r.Context(), queries.CreateTemplateParams{ID: input.Id, Definition: raw}); err != nil {
		return err
	}
	if _, err = q.CreateOperation(r.Context(), queries.CreateOperationParams{ID: uuid.NewString(), ScopeKind: "template", ScopeID: input.Id, Kind: "prepare-template", Payload: payload, ExpectedRevision: int32(input.Version)}); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, input)
}

func nodeRecord(row queries.ListNodePageRow) (api.Node, error) {
	var info api.NodeInfo
	if err := json.Unmarshal(row.Info, &info); err != nil {
		return api.Node{}, err
	}
	result := api.Node{Id: row.ID, Name: row.Name, Endpoint: row.Endpoint, Capacity: info.Capacity, Capabilities: info.Capabilities, Slots: info.Slots, Reserved: api.Resources{Cpu: int(row.ReservedCpu), MemoryMiB: row.ReservedMemory, DiskGiB: row.ReservedDisk}, State: &row.State, ObservedAt: row.ObservedAt.Time}
	if len(row.CapacityOverride) > 0 {
		if err := json.Unmarshal(row.CapacityOverride, &result.Override); err != nil {
			return result, err
		}
		result.Capacity = *result.Override
	}
	return result, nil
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	cursor, limit, err := pagination(r)
	if err != nil {
		return err
	}
	rows, err := s.Queries.ListNodePage(r.Context(), queries.ListNodePageParams{Cursor: cursor, PageLimit: limit, Search: strings.TrimSpace(r.URL.Query().Get("search"))})
	if err != nil {
		return err
	}
	result := make([]api.Node, 0, len(rows))
	for _, row := range rows {
		item, err := nodeRecord(row)
		if err != nil {
			return err
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) registerNode(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.NodeRegistration
	if err := decode(w, r, &input); err != nil {
		return err
	}
	endpoint, err := url.Parse(input.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return httpError{http.StatusBadRequest, "节点服务地址应为 HTTPS 地址"}
	}
	input.Endpoint = strings.TrimRight(input.Endpoint, "/")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info, err := s.Nodes.Info(ctx, input.Endpoint)
	if err != nil {
		return httpError{http.StatusBadGateway, err.Error()}
	}
	if strings.TrimSpace(input.Name) == "" {
		input.Name = info.Name
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err = s.Queries.PutNode(r.Context(), queries.PutNodeParams{ID: info.Id, Name: input.Name, Endpoint: input.Endpoint, Info: raw}); err != nil {
		return err
	}
	reserved, err := s.Queries.GetReservedResources(r.Context(), info.Id)
	if err != nil {
		return err
	}
	state := "ready"
	result := api.Node{Id: info.Id, Name: input.Name, Endpoint: input.Endpoint, Capacity: info.Capacity, Capabilities: info.Capabilities, Slots: info.Slots, Reserved: api.Resources{Cpu: int(reserved.Cpu), MemoryMiB: reserved.MemoryMib, DiskGiB: reserved.DiskGib}, State: &state, ObservedAt: time.Now().UTC()}
	return writeJSON(w, http.StatusCreated, result)
}
