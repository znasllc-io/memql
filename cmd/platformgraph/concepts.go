package main

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

var (
	loadOnce sync.Once
	loadErr  error
)

// loadConcepts loads the embedded DSL tree's concepts into the default
// registry, once: the concepts pass and the automations pass both read it.
// Only the EMBEDDED tree -- MEMQL_DSL_PATH mounts nothing here, since that is
// dsl.MountRuntimeDomainsFromEnv's job and this command never calls it -- so
// the graph describes the engine, not whatever product the caller's shell
// points at.
func loadConcepts() error {
	loadOnce.Do(func() {
		_, loadErr = memql.LoadUnifiedConcepts(slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	return loadErr
}

// conceptNamespace is the namespace segment of a canonical concept id,
// v1:<namespace>:<name>.
func conceptNamespace(canonical string) (string, string, bool) {
	parts := strings.Split(canonical, ":")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// conceptsPass records every concept under its namespace, and one relates
// edge per @relationship whose target the registry resolves.
func conceptsPass(b *builder) error {
	if err := loadConcepts(); err != nil {
		return err
	}
	reg := concept.DefaultRegistry()
	list := reg.List()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	if len(list) == 0 {
		return fmt.Errorf("the concept registry is empty")
	}
	for _, c := range list {
		ns, short, ok := conceptNamespace(c.Name)
		if !ok {
			return fmt.Errorf("concept id %q is not v1:<namespace>:<name>", c.Name)
		}
		b.node(model.NamespaceID(ns), model.PlatformNamespace, ns, nil)
		b.node(model.ConceptID(c.Name), model.PlatformConcept, short, map[string]string{
			"namespace": ns,
			"type":      c.NodeType,
		})
		b.edge(model.NamespaceID(ns), model.ConceptID(c.Name), model.PlatformContains, nil)
	}
	unresolved := 0
	for _, c := range list {
		for _, r := range c.Relationships {
			target, err := reg.Get(r.TargetConcept)
			if err != nil || target == nil {
				unresolved++
				continue
			}
			b.edge(model.ConceptID(c.Name), model.ConceptID(target.Name), model.PlatformRelates, map[string]string{
				"type":      r.Type,
				"field":     r.Field,
				"direction": r.Direction,
				"as":        r.As,
			})
		}
	}
	if unresolved > 0 {
		return fmt.Errorf("%d @relationship target(s) resolve to no registered concept", unresolved)
	}
	return nil
}

// automationsPass records the static automation graph through
// automations.FlattenLoopGraph, the projection cmd/memql-arch and the
// automationGraph builtin's rows share, plus a writes edge from each
// automation to each concept it writes.
func automationsPass(b *builder) error {
	if err := loadConcepts(); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := automations.NewOfflineEngine(logger, concept.DefaultRegistry())
	if err != nil {
		return err
	}
	l := automations.NewLoader(automations.LoaderOptions{Logger: logger, Registry: concept.DefaultRegistry(), Functions: eng.Functions()})
	g, err := l.StaticGraph()
	if err != nil {
		return err
	}
	autos, triggers := automations.FlattenLoopGraph(g)
	if len(autos) == 0 {
		return fmt.Errorf("the static automation graph is empty")
	}
	for _, a := range autos {
		attrs := make(map[string]string, len(a.Attrs)+1)
		for k, v := range a.Attrs {
			attrs[k] = v
		}
		attrs["origin"] = a.File
		b.node(model.AutomationID(a.Name), model.PlatformAutomation, a.Name, attrs)
	}
	for _, a := range autos {
		for _, w := range a.Writes {
			if b.has(model.ConceptID(w)) {
				b.edge(model.AutomationID(a.Name), model.ConceptID(w), model.PlatformWrites, nil)
			}
		}
	}
	for _, e := range triggers {
		b.edge(model.AutomationID(e.From), model.AutomationID(e.To), model.PlatformTriggers, e.Attrs)
	}
	return nil
}
