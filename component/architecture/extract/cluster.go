package extract

import (
	"github.com/visionarys-io/memql/component/architecture/model"
)

// ExtractCluster prepends a synthetic cluster root and parents every
// Service to it. This gives the model a true C4-L1 root so the
// drill-down navigator has a defined "zoom all the way out" position
// regardless of how many services live in the workspace.
//
// The cluster's identity (name) is supplied by the caller -- typically
// the user via --cluster on the CLI, defaulting to the workspace
// folder name.
func ExtractCluster(m *model.Model, clusterName string) {
	clusterID := model.ClusterID(clusterName)
	cluster := model.Node{
		ID:   clusterID,
		Kind: model.KindCluster,
		Name: clusterName,
	}

	// Insert at index 0 so JSON output reads top-down. We also rewrite
	// each Service node's Parent so the index can resolve it without
	// having to walk EdgeContains.
	nodes := make([]model.Node, 0, len(m.Nodes)+1)
	nodes = append(nodes, cluster)
	for _, n := range m.Nodes {
		if n.Kind == model.KindService && n.Parent == "" {
			n.Parent = clusterID
			m.Edges = append(m.Edges, model.Edge{
				From: clusterID,
				To:   n.ID,
				Kind: model.EdgeContains,
			})
		}
		nodes = append(nodes, n)
	}
	m.Nodes = nodes
}
