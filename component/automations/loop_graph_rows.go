package automations

// loop_graph_rows.go -- the static loop graph as one row per automation
// (memql#5384; D-L of the loop protection plan): what the automationGraph
// builtin serves as v1:platform:automationNode rows, and what the OS's
// Cluster > Automations section draws.
//
// TWO HALVES. LoopGraphRows is a pure function of a graph, tested with no
// scheduler and no engine. Scheduler.AutomationGraphRows builds the graph over
// the automations THIS NODE'S SCHEDULER REGISTERED -- not StaticGraph, which
// re-walks the whole tree through LoadAll on every call -- and keeps the build
// until that set changes.
//
// WHAT THE ROWS ARE NOT. They carry no payload of any run and no argument
// value: names, trigger topics, filter source, concept ids and the graph's
// own reasons. The page that draws them is a map of the cluster's wiring,
// and nothing on it is a record of what any automation did.

import (
	"errors"
	"sort"
	"sync"

	"github.com/znasllc-io/memql/component/language/ast"
)

// LoopGraphRows is g as one row per automation, in the graph's own order
// (name, then origin). Every key a row has no value for is ABSENT rather than
// empty -- a scheduled automation has no trigger, and "" would read as a
// trigger nobody can describe -- except the three lists, which are always
// present, because a client reading `.length` must not guard for absence on
// the automations that write nothing, which are most of them.
//
// A row:
//
//	name, stratum, template                 always
//	trigger, triggerKind, triggerConcept    an event-triggered automation
//	schedule                                a scheduled one
//	triggerFilter                           its @filter's source
//	writes, publishes, opaque               concept ids, topics, and the calls
//	                                        the graph cannot see into
//	edgesOut                                [{to, concept, topic, via, decided, reason}]
//	loop, mode                              {maxDepth, until}, {kind, max}
//	cycle                                   {members, permittedBy, path, permitted}
//	problem                                 the refusal the load would make
//	origin                                  the file it was loaded from
func LoopGraphRows(g *LoopGraph) []map[string]any {
	if g == nil {
		return []map[string]any{}
	}
	edgesFrom := map[string][]GraphEdge{}
	for _, e := range g.Edges {
		edgesFrom[e.From] = append(edgesFrom[e.From], e)
	}
	problems := map[string]string{}
	for _, p := range g.Problems {
		// One problem per automation reads cleanly; a second is the same
		// cycle named from another place, and the first says what to do.
		if _, seen := problems[p.Automation]; !seen {
			problems[p.Automation] = p.Message
		}
	}

	out := make([]map[string]any, 0, len(g.Automations))
	for _, a := range g.Automations {
		row := map[string]any{
			"name":      a.Name,
			"stratum":   a.Stratum,
			"template":  a.Template,
			"writes":    stringList(a.Writes),
			"publishes": stringList(a.Publishes),
			"opaque":    stringList(a.Opaque),
			"edgesOut":  edgeRows(edgesFrom[a.Name]),
		}
		if a.Trigger != "" {
			row["trigger"] = a.Trigger
			// The pair a sentence is built from ("an update to request"),
			// put back through the declared inverse of the fold that made
			// the topic, so no client parses a topic to describe one.
			if kind, concept, ok := ast.SplitTriggerTopic(a.Trigger); ok {
				row["triggerKind"] = kind
				if concept != "" {
					row["triggerConcept"] = concept
				}
			}
		}
		if a.BeforeWrite != nil {
			row["triggerKind"] = "before." + a.BeforeWrite.On
			row["triggerConcept"] = a.BeforeWrite.Concept
		}
		if a.Schedule != "" {
			row["schedule"] = a.Schedule
		}
		if a.Filter != "" {
			row["triggerFilter"] = a.Filter
		}
		if a.Loop != nil {
			row["loop"] = map[string]any{"maxDepth": a.Loop.MaxDepth, "until": a.Loop.Until}
		}
		if a.Mode != nil {
			mode := map[string]any{"kind": a.Mode.Kind}
			// 0 is "the default" for queued and parallel (ModeConfig), which
			// is a different statement from a max of 0, so it is left out.
			if a.Mode.Max > 0 {
				mode["max"] = a.Mode.Max
			}
			row["mode"] = mode
		}
		if a.Cycle >= 0 && a.Cycle < len(g.Cycles) {
			row["cycle"] = cycleRow(g.Cycles[a.Cycle])
		}
		if p, ok := problems[a.Name]; ok {
			row["problem"] = p
		}
		if file := automationFile(a.Origin, a.Name); file != "" {
			row["origin"] = file
		}
		out = append(out, row)
	}
	return out
}

// edgeRows is one automation's outgoing edges, by target, then concept, then
// topic, so a re-read draws the same curves in the same order.
func edgeRows(edges []GraphEdge) []map[string]any {
	sorted := append([]GraphEdge(nil), edges...)
	sort.SliceStable(sorted, func(i, j int) bool {
		p, q := sorted[i], sorted[j]
		if p.To != q.To {
			return p.To < q.To
		}
		if p.Concept != q.Concept {
			return p.Concept < q.Concept
		}
		return p.Topic < q.Topic
	})
	out := make([]map[string]any, 0, len(sorted))
	for _, e := range sorted {
		out = append(out, map[string]any{
			"to":      e.To,
			"concept": e.Concept,
			"topic":   e.Topic,
			"via":     stringList(e.Via),
			"decided": e.Decided,
			"reason":  e.Reason,
		})
	}
	return out
}

// cycleRow is one strongly connected component that holds a cycle. `permitted`
// is said outright rather than left to be inferred from an empty permittedBy:
// operator break-glass can load a refused cycle, and the page must distinguish
// it from an explicitly bounded cycle.
func cycleRow(c GraphCycle) map[string]any {
	members := append([]string(nil), c.Members...)
	sort.Strings(members)
	permittedBy := append([]string(nil), c.PermittedBy...)
	sort.Strings(permittedBy)
	return map[string]any{
		"members":     stringList(members),
		"permittedBy": stringList(permittedBy),
		"path":        stringList(c.Path),
		"permitted":   len(c.PermittedBy) > 0,
	}
}

// stringList is in, never nil, so it marshals as [] rather than null.
func stringList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ---------------------------------------------------------------------------
// The scheduler's rows
// ---------------------------------------------------------------------------

// The two reasons there is no graph to serve. Each is an ERROR rather than an
// empty list, because an empty list is an answer -- "this node loaded no
// automations" -- and neither of these is that.
var (
	errGraphNotRegistered = errors.New("automation graph: this node's scheduler has not registered its automations yet; read again in a moment")
	errGraphNoFunctions   = errors.New("automation graph: this node's automation loader has no function registry (LoaderOptions.Functions), so the graph cannot see what an automation writes, and a graph with no edges would read as a cluster with no cycles")
)

// graphRowsKey is one registered automation: its name and the definition the
// scheduler holds under it.
type graphRowsKey struct {
	name string
	auto *Automation
}

// graphRowsCache is the rows built from one registered set. The zero value is
// ready to use.
type graphRowsCache struct {
	mu    sync.Mutex
	built bool
	key   []graphRowsKey
	rows  []map[string]any
}

// AutomationGraphRows is the static loop graph over the automations this
// scheduler registered, one row per automation (LoopGraphRows), satisfying
// memql.AutomationGraphSource.
//
// BUILT ONCE PER REGISTERED SET. The set is keyed by (name, definition) --
// the pointer, not only the name, so an automation re-registered under the
// same name is never served its predecessor's edges -- and today it is filled
// once, at the scheduler's start, so the graph is walked once per boot however
// often the page is read.
//
// The rows are SHARED between callers and must not be mutated; the engine
// marshals each one and lets go of it.
func (s *Scheduler) AutomationGraphRows() ([]map[string]any, error) {
	select {
	case <-s.readyCh:
	default:
		return nil, errGraphNotRegistered
	}
	if s.loader == nil || s.loader.functions == nil {
		return nil, errGraphNoFunctions
	}

	s.mu.RLock()
	set := make([]*Automation, 0, len(s.automations))
	key := make([]graphRowsKey, 0, len(s.automations))
	for name, a := range s.automations {
		if a == nil {
			continue
		}
		set = append(set, a)
		key = append(key, graphRowsKey{name: name, auto: a})
	}
	s.mu.RUnlock()
	sort.Slice(key, func(i, j int) bool { return key[i].name < key[j].name })

	s.graphRows.mu.Lock()
	defer s.graphRows.mu.Unlock()
	if s.graphRows.built && sameGraphRowsKey(s.graphRows.key, key) {
		return s.graphRows.rows, nil
	}
	g := BuildLoopGraph(set, newFunctionSource(s.loader.functions, s.loader.registry), 0)
	s.graphRows.rows = LoopGraphRows(g)
	s.graphRows.key = key
	s.graphRows.built = true
	return s.graphRows.rows, nil
}

func sameGraphRowsKey(a, b []graphRowsKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
