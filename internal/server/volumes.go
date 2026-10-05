package server

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/operation"
)

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	id := r.PathValue("id")
	rows, err := s.Queries.ListPersistentVolumes(r.Context(), id)
	if err != nil {
		return err
	}
	items := make([]api.PersistentVolume, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	references, err := s.Queries.PersistentVolumeReferences(r.Context(), ids)
	if err != nil {
		return err
	}
	refs := map[string][]string{}
	for _, reference := range references {
		refs[reference.VolumeID] = append(refs[reference.VolumeID], reference.Name)
	}
	for _, row := range rows {
		items = append(items, api.PersistentVolume{Id: row.ID, NodeId: row.NodeID, StoragePoolId: row.StoragePoolID, Name: row.Name, Kind: api.TemplateKind(row.Kind), SizeGiB: row.SizeGib, State: api.PersistentVolumeState(row.State), OperationId: row.OperationID, Error: row.OperationError, References: append([]string{}, refs[row.ID]...)})
	}
	if id != "" {
		if len(items) == 0 {
			return pgx.ErrNoRows
		}
		return writeJSON(w, http.StatusOK, items[0])
	}
	return writeJSON(w, http.StatusOK, items)
}

func (s *Server) volumeAction(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	if err := requireAdministrator(identity); err != nil {
		return err
	}
	var input api.CreateVolume
	id, kind := r.PathValue("id"), "create-volume"
	if id == "" {
		if err := decode(w, r, &input); err != nil {
			return err
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" || (input.Kind != api.Vm && input.Kind != api.Container) {
			return environment.Invalid("请填写名称并选择数据卷类型")
		}
		id = uuid.NewString()
	} else {
		row, err := s.Queries.GetPersistentVolume(r.Context(), id)
		if err != nil {
			return err
		}
		input = api.CreateVolume{Name: row.Name, Kind: api.TemplateKind(row.Kind), StoragePoolId: row.StoragePoolID, SizeGiB: row.SizeGib}
		kind = "delete-volume"
		if r.Method == http.MethodPut {
			var size api.ResizeVolumeJSONBody
			if err = decode(w, r, &size); err != nil {
				return err
			}
			input.SizeGiB, kind = size.SizeGiB, "resize-volume"
		}
	}
	if input.SizeGiB < 1 {
		return environment.Invalid("数据卷容量应大于零")
	}
	worker := operation.Worker{Pool: s.Pool, Queries: s.Queries, Client: s.Nodes}
	result, err := worker.SubmitVolume(r.Context(), id, kind, input)
	if err != nil {
		return err
	}
	w.Header().Set("Operation-Location", "/api/v1/operations/"+result.Id)
	w.Header().Set("Location", "/api/v1/volumes/"+id)
	return writeJSON(w, http.StatusAccepted, result)
}
