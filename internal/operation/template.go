package operation

import (
	"encoding/json"
	"errors"
	"slices"

	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

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
