package operation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

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
		if node.State == "ready" && slices.Contains(info.Capabilities, string(t.Kind)) &&
			(metadataImport || supports(info, t, t.Resources.Cpu)) &&
			(t.ArtifactNodeId == nil || *t.ArtifactNodeId == node.ID) {
			return node, nil
		}
	}
	return queries.ListNodesRow{}, errors.New("没有可准备该模板的节点")
}
