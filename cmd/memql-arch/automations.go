package main

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
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
	for _, a := range g.Automations {
		attrs := map[string]string{"stratum": strconv.Itoa(a.Stratum), "trigger": a.Trigger, "schedule": a.Schedule, "filter": a.Filter, "writes": strings.Join(a.Writes, ","), "origin": a.Origin}
		if a.Loop != nil {
			attrs["loop"] = fmt.Sprintf("maxDepth=%d until=%s", a.Loop.MaxDepth, a.Loop.Until)
		}
		if a.Mode != nil {
			attrs["mode"] = a.Mode.Kind
			if a.Mode.Max > 0 {
				attrs["mode"] += fmt.Sprintf(" max=%d", a.Mode.Max)
			}
		}
		for key, value := range attrs {
			if value == "" {
				delete(attrs, key)
			}
		}
		var source *model.SourceRef
		if strings.HasPrefix(a.Origin, "unified:") {
			file := strings.TrimSuffix(strings.TrimPrefix(a.Origin, "unified:"), ":"+a.Name)
			source = &model.SourceRef{File: "dsl/" + file}
		}
		m.Nodes = append(m.Nodes, model.Node{ID: model.AutomationID(a.Name), Kind: model.KindAutomation, Name: a.Name, Parent: cluster, Source: source, Attrs: attrs})
		m.Edges = append(m.Edges, model.Edge{From: cluster, To: model.AutomationID(a.Name), Kind: model.EdgeContains})
	}
	for _, e := range g.Edges {
		m.Edges = append(m.Edges, model.Edge{From: model.AutomationID(e.From), To: model.AutomationID(e.To), Kind: model.EdgeTriggers, Attrs: map[string]string{"concept": e.Concept, "topic": e.Topic, "decided": strconv.FormatBool(e.Decided), "via": strings.Join(e.Via, ","), "reason": e.Reason}})
	}
	return nil
}
