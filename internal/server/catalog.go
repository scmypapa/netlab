package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/guest"
	"netlab.local/core/internal/operation"
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
		if !identity.Administrator() {
			item.Source = ""
		}
		result = append(result, item)
	}
	return writeJSON(w, http.StatusOK, result)
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var request struct {
		api.Template
		Registry *api.RegistryCredentials `json:"registry,omitempty"`
	}
	var files *multipart.Reader
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return httpError{http.StatusBadRequest, "请求格式无效"}
	}
	if mediaType == "multipart/form-data" {
		files, err = r.MultipartReader()
		if err != nil {
			return httpError{http.StatusBadRequest, err.Error()}
		}
		part, err := files.NextPart()
		if err != nil {
			return httpError{http.StatusBadRequest, "请先发送模板信息"}
		}
		if part.FormName() != "template" {
			return httpError{http.StatusBadRequest, "第一部分应为 template"}
		}
		decoder := json.NewDecoder(io.LimitReader(part, 1<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err != nil {
			return httpError{http.StatusBadRequest, err.Error()}
		}
	} else if err = decode(w, r, &request); err != nil {
		return err
	}
	input := request.Template
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
	if err := guest.ValidateCPU(input.Hardware, input.Resources); err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	if input.Id == "" {
		input.Id = uuid.NewString()
	}
	if input.Version == 0 {
		input.Version = 1
	}
	if input.Version < 1 || input.Id == "." || input.Id == ".." || strings.ContainsAny(input.Id, "/\\") {
		return httpError{http.StatusBadRequest, "模板标识或版本无效"}
	}
	state := api.TemplateStateImporting
	input.State, input.Error = &state, nil
	operationID := uuid.NewString()
	input.OperationId = &operationID
	input.ArtifactNodeId = nil
	input.StateFiles = nil
	p := operation.Payload{Template: &input}
	if request.Registry != nil {
		if input.Kind != api.Container || files != nil {
			return httpError{http.StatusBadRequest, "仓库认证只用于容器镜像地址"}
		}
		if s.Secrets == nil {
			return fmt.Errorf("controller secret key is not configured")
		}
		raw, err := json.Marshal(request.Registry)
		if err != nil {
			return err
		}
		p.TemplateCredentials = s.Secrets.Encrypt(raw, fmt.Sprintf("%s/%d", input.Id, input.Version))
	}
	committed := false
	uploadPath := ""
	var uploadNode queries.ListNodesRow
	defer func() {
		if uploadPath != "" && !committed {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.Nodes.Do(ctx, http.MethodDelete, uploadNode.Endpoint, uploadPath, nil, nil); err != nil {
				slog.Error("template upload cleanup failed", "template", input.Id, "node", uploadNode.ID, "error", err)
			}
		}
	}()
	if files != nil {
		items, err := s.Queries.GetTemplates(r.Context(), []string{input.Id})
		if err != nil {
			return err
		}
		if len(items) > 0 {
			return httpError{http.StatusConflict, "模板已存在"}
		}
		nodes, err := s.Queries.ListNodes(r.Context())
		if err != nil {
			return err
		}
		if uploadNode, err = operation.TemplateNode(nodes, input); err != nil {
			return httpError{http.StatusConflict, err.Error()}
		}
		uploadPath = fmt.Sprintf("/node/v1/templates/%s/versions/%d/imports/%s", input.Id, input.Version, uuid.NewString())
		if err = s.uploadTemplate(r.Context(), uploadNode.Endpoint, uploadPath, input.Source, files, r.Body, &input.Source); err != nil {
			return err
		}
		input.ArtifactNodeId = &uploadNode.ID
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(p)
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
	if _, err = q.CreateOperation(r.Context(), queries.CreateOperationParams{ID: operationID, ScopeKind: "template", ScopeID: input.Id, Kind: "prepare-template", Payload: payload, ExpectedRevision: int32(input.Version)}); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	committed = true
	w.Header().Set("Operation-Location", "/api/v1/operations/"+operationID)
	return writeJSON(w, http.StatusCreated, input)
}

func (s *Server) uploadTemplate(ctx context.Context, endpoint, path, source string, files *multipart.Reader, input io.Closer, output *string) error {
	reader, writer := io.Pipe()
	body := multipart.NewWriter(writer)
	finished := make(chan error, 1)
	go func() {
		var err error
		for {
			part, nextErr := files.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				err = nextErr
				break
			}
			var target io.Writer
			if target, err = body.CreatePart(part.Header); err != nil {
				break
			}
			if _, err = io.Copy(target, part); err != nil {
				break
			}
		}
		if err == nil {
			err = body.Close()
		}
		writer.CloseWithError(err)
		finished <- err
	}()
	err := s.Nodes.Stream(ctx, http.MethodPost, endpoint, path+"?source="+url.QueryEscape(source), body.FormDataContentType(), reader, output)
	reader.CloseWithError(err)
	if err != nil {
		input.Close()
	}
	return errors.Join(err, <-finished)
}

func nodeRecord(row queries.ListNodePageRow) (api.Node, error) {
	var info api.NodeInfo
	if err := json.Unmarshal(row.Info, &info); err != nil {
		return api.Node{}, err
	}
	result := api.Node{Id: row.ID, Name: row.Name, Endpoint: row.Endpoint, Capacity: info.Capacity, Capabilities: info.Capabilities, Slots: info.Slots, Reserved: api.Resources{Cpu: int(row.ReservedCpu), MemoryMiB: row.ReservedMemory, DiskGiB: row.ReservedDisk}, State: &row.State, ObservedAt: row.ObservedAt.Time}
	if row.Retiring && row.State == "ready" {
		state := "draining"
		result.State = &state
	}
	result.VmHardware = info.VmHardware
	result.StorageDevice = info.StorageDevice
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
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	devices, err := s.Queries.DeviceReservations(r.Context(), ids)
	if err != nil {
		return err
	}
	occupied := map[string]bool{}
	for _, device := range devices {
		occupied[device.NodeID+"/"+device.GroupID] = true
	}
	for _, row := range rows {
		item, err := nodeRecord(row)
		if err != nil {
			return err
		}
		if item.VmHardware != nil {
			for i := range item.VmHardware.PciGroups {
				group := &item.VmHardware.PciGroups[i]
				group.Available = group.Available && !occupied[item.Id+"/"+group.Id]
			}
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
	if err = (operation.Service{Pool: s.Pool, Queries: s.Queries}).ConfigureManagedStorage(r.Context(), nil); err != nil {
		return err
	}
	reserved, err := s.Queries.GetReservedResources(r.Context(), info.Id)
	if err != nil {
		return err
	}
	state := "ready"
	result := api.Node{Id: info.Id, Name: input.Name, Endpoint: input.Endpoint, Capacity: info.Capacity, Capabilities: info.Capabilities, Slots: info.Slots, Reserved: api.Resources{Cpu: int(reserved.Cpu), MemoryMiB: reserved.MemoryMib, DiskGiB: reserved.DiskGib}, State: &state, ObservedAt: time.Now().UTC()}
	result.VmHardware = info.VmHardware
	result.StorageDevice = info.StorageDevice
	return writeJSON(w, http.StatusCreated, result)
}

func (s *Server) nodeInterfaces(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	nodes, err := s.Queries.GetNodeEndpoints(r.Context(), []string{r.PathValue("id")})
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return httpError{http.StatusNotFound, "节点不存在"}
	}
	info, err := s.Nodes.Info(r.Context(), nodes[0].Endpoint)
	if err != nil {
		return err
	}
	if info.Id != nodes[0].ID {
		return fmt.Errorf("node interface inventory identity mismatch")
	}
	interfaces := []api.ExternalInterface{}
	if info.ExternalInterfaces != nil {
		interfaces = *info.ExternalInterfaces
	}
	return writeJSON(w, http.StatusOK, interfaces)
}
