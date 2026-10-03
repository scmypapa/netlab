package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	var wg sync.WaitGroup
	failures := make([]error, len(nodes))
	for i, node := range nodes {
		wg.Go(func() {
			failures[i] = w.Client.Do(ctx, http.MethodDelete, node.Endpoint, fmt.Sprintf("/node/v1/templates/%s/versions/%d", p.Template.Id, p.Template.Version), nil, nil)
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
