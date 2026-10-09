package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func TemplateCacheRoute(t api.Template) string {
	return fmt.Sprintf("/node/v1/templates/%s/versions/%d/cache", t.Id, t.Version)
}

func (w Worker) trimTemplateCache(ctx context.Context, op *queries.Operation, p *Payload) error {
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	refs, err := w.Queries.TemplateLocalReferences(ctx, p.Template.Id)
	if err != nil {
		return err
	}
	pools, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	if err = w.phase(ctx, op, p, "trim-template-cache"); err != nil {
		return err
	}
	endpoints := map[string]string{}
	for _, node := range nodes {
		if node.State == "ready" {
			endpoints[node.ID] = node.Endpoint
		}
	}
	for _, pool := range pools {
		if pool.Driver != "rbd" || pool.State != "ready" {
			continue
		}
		for _, id := range pool.NodeIds {
			if endpoints[id] == "" {
				continue
			}
			input := api.NodeTemplatePreparation{Template: *p.Template, StoragePoolId: &pool.ID}
			if p.Template.ArtifactNodeId != nil {
				endpoint := endpoints[*p.Template.ArtifactNodeId]
				input.ArtifactEndpoint = &endpoint
			}
			if err = w.prepareSharedTemplate(ctx, fmt.Sprintf("%s/%s/%d", pool.ID, p.Template.Id, p.Template.Version), endpoints[id], input); err != nil {
				return err
			}
			break
		}
	}
	for _, node := range nodes {
		if node.State != "ready" || slices.Contains(refs, node.ID) {
			continue
		}
		ids := []string{}
		for _, pool := range pools {
			if pool.Driver == "rbd" && pool.State == "ready" && slices.Contains(pool.NodeIds, node.ID) {
				ids = append(ids, pool.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		if err = w.Client.Do(ctx, http.MethodDelete, node.Endpoint, "/node/v1/template-cache", api.TemplateCacheRequest{Template: *p.Template, PoolIds: ids}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (w Worker) deleteTemplate(ctx context.Context, op *queries.Operation, p *Payload) error {
	nodes, err := w.Queries.ListNodes(ctx)
	if err != nil {
		return err
	}
	if err = w.phase(ctx, op, p, "remove-template"); err != nil {
		return err
	}
	pools, err := w.Queries.ListStoragePools(ctx)
	if err != nil {
		return err
	}
	owned := map[string][]string{}
	for _, pool := range pools {
		if pool.Driver == "rbd" {
			owned[pool.NodeIds[0]] = append(owned[pool.NodeIds[0]], pool.ID)
		}
	}
	var wg sync.WaitGroup
	failures := make([]error, len(nodes))
	for i, node := range nodes {
		wg.Go(func() {
			path := "/node/v1/templates/" + p.Template.Id
			if len(owned[node.ID]) > 0 {
				path += "?" + (url.Values{"storagePool": owned[node.ID]}).Encode()
			}
			failures[i] = w.Client.Do(ctx, http.MethodDelete, node.Endpoint, path, nil, nil)
		})
	}
	wg.Wait()
	return errors.Join(failures...)
}

func TemplateNode(nodes []queries.ListNodesRow, t api.Template) (queries.ListNodesRow, error) {
	metadataImport := t.Hardware == nil && t.Format != nil && (*t.Format == "ova" || *t.Format == "ovf")
	for _, node := range nodes {
		var info api.NodeInfo
		if err := json.Unmarshal(node.Info, &info); err != nil {
			return queries.ListNodesRow{}, err
		}
		if node.State == "ready" && !node.Retiring && slices.Contains(info.Capabilities, string(t.Kind)) &&
			(metadataImport || supports(info, t, t.Resources.Cpu)) &&
			(t.ArtifactNodeId == nil || *t.ArtifactNodeId == node.ID) {
			return node, nil
		}
	}
	return queries.ListNodesRow{}, errors.New("没有可准备该模板的节点")
}
