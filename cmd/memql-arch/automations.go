package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func addAutomations(m *model.Model) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		return err
	}
	eng, err := automations.NewOfflineEngine(logger, concept.DefaultRegistry())
	if err != nil {
		return err
	}
	l := automations.NewLoader(automations.LoaderOptions{Logger: logger, Registry: concept.DefaultRegistry(), Functions: eng.Functions()})
	g, err := l.StaticGraph()
	if err != nil {
		return err
	}
	return appendAutomationGraph(m, g)
}

// appendAutomationGraph records g in the model through
// automations.FlattenLoopGraph -- the one projection the automationGraph
// builtin's rows and cmd/platformgraph share (memql#5727) -- so the model, the
// platform graph and the OS's Cluster > Automations section describe each
// automation the same way.
func appendAutomationGraph(m *model.Model, g *automations.LoopGraph) error {
	var cluster model.ID
	count := 0
	for _, n := range m.Nodes {
		if n.Kind == model.KindCluster {
			cluster = n.ID
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("automation graph requires one cluster, found %d", count)
	}
	autos, triggers := automations.FlattenLoopGraph(g)
	for _, a := range autos {
		attrs := make(map[string]string, len(a.Attrs)+1)
		for k, v := range a.Attrs {
			attrs[k] = v
		}
		var source *model.SourceRef
		if strings.HasSuffix(a.File, ".memql") {
			attrs["origin"] = a.File
			source = &model.SourceRef{File: "dsl/" + a.File}
		}
		id := model.AutomationID(a.Name)
		m.Nodes = append(m.Nodes, model.Node{ID: id, Kind: model.KindAutomation, Name: a.Name, Parent: cluster, Source: source, Attrs: attrs})
		m.Edges = append(m.Edges, model.Edge{From: cluster, To: id, Kind: model.EdgeContains})
	}
	for _, e := range triggers {
		m.Edges = append(m.Edges, model.Edge{From: model.AutomationID(e.From), To: model.AutomationID(e.To), Kind: model.EdgeTriggers, Attrs: e.Attrs})
	}
	return nil
}
