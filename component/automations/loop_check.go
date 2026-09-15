package automations

// loop_check.go -- where the static loop graph runs (memql#5381; D-J of the
// loop protection plan): the automation loader, after its walk and before its
// strict gate, over every automation the walk loaded, so a cycle is a load
// problem in the same list as a compile error (phase "loops"). memqllint and
// the corpus runner load through this loader too, and the authored scheduler
// runs the same graph at activation (authored_scheduler.go).
//
// A loader with no function registry cannot see what an automation writes,
// so the check does not run -- and the load says so, and StaticGraph refuses,
// rather than reporting a graph with no edges as clean.

import (
	"fmt"
	"strings"
)

// checkLoops builds the static loop graph over loaded -- reading the
// functions the loader holds, against the concepts it compiles with -- and
// turns each of its problems into a load problem on the file of the
// automation it names.
//
// It passes no depth cap: the load's own prepare step refuses a @loop bound
// outside the cap before an automation reaches the graph.
func (l *Loader) checkLoops(loaded []*Automation) (*LoopGraph, []automationLoadProblem) {
	g := BuildLoopGraph(loaded, newFunctionSource(l.functions, l.registry), 0)
	var out []automationLoadProblem
	for _, p := range g.Problems {
		out = append(out, automationLoadProblem{
			Path:  automationFile(p.Origin, p.Automation),
			Name:  p.Automation,
			Phase: "loops",
			Err:   p.Message,
		})
	}
	return g, out
}

// logLoopCheck logs what the check found: its coverage, every problem, and
// a @loop on an automation in no cycle, which is not a problem -- it still
// bounds the automation at run time -- but is worth a look. A nil graph is a
// check that did not run, and says so.
//
// ONCE PER LOADER. The node's one loader also serves LoadByName at run time
// (run_automation, a work template's dispatch), which re-walks the tree every
// call; the tree does not change after boot, and neither does what the check
// finds in it, so the boot load logs it and the rest stay quiet.
func (l *Loader) logLoopCheck(g *LoopGraph, problems []automationLoadProblem) {
	if l.logger == nil {
		return
	}
	l.loopLogOnce.Do(func() { l.logLoopCheckNow(g, problems) })
}

func (l *Loader) logLoopCheckNow(g *LoopGraph, problems []automationLoadProblem) {
	if g == nil {
		l.logger.Warn("static loop analysis not run: the loader has no function registry", "component", ComponentName)
		return
	}
	l.logger.Info("static loop analysis",
		"component", ComponentName,
		"automations", g.Coverage.Automations,
		"resolvedCalls", g.Coverage.Resolved,
		"unresolvedCalls", g.Coverage.Unresolved,
		"opaqueCalls", g.Coverage.Opaque,
		"edges", len(g.Edges),
		"cycles", len(g.Cycles),
		"problems", len(problems))
	for _, p := range problems {
		l.logger.Warn("static loop analysis: problem",
			"component", ComponentName, "path", p.Path, "automation", p.Name, "problem", p.Err)
	}
	for _, a := range g.Automations {
		if a.Loop != nil && a.Cycle < 0 {
			l.logger.Warn("static loop analysis: @loop on an automation in no cycle; it still bounds the automation at run time",
				"component", ComponentName, "automation", a.Name, "origin", a.Origin)
		}
	}
}

// StaticGraph loads every automation of the tree and builds the static loop
// graph over them: what the OS and the architecture model draw. It refuses
// when the loader has no function registry, because a graph built without
// one has no edges and would read as a tree with no cycles.
func (l *Loader) StaticGraph() (*LoopGraph, error) {
	if l.functions == nil {
		return nil, fmt.Errorf("static loop graph: the loader has no function registry (LoaderOptions.Functions), so it cannot see what an automation writes")
	}
	loaded, err := l.LoadAll()
	if err != nil {
		return nil, err
	}
	g, _ := l.checkLoops(loaded)
	return g, nil
}

// refuseCandidateCycle is the check at authored activation (D-J): the graph
// over the shipped automations plus the candidate, refusing the candidate
// when it lies on a cycle no @loop covers. A cycle among the shipped
// automations alone is the load's to report, not this candidate's to be
// refused for (problemThrough).
//
// The scheduler reaches the engine's function registry through its loader:
// the app builds both from one loader, which carries the registry. A
// scheduler whose loader has none cannot run the check, and logs that it did
// not rather than activating as if it had.
//
// What the candidate's own bundle adds to the function registry is not in
// the engine's, so a write it makes through a construct of its own bundle is
// an unresolved call here, counted but not followed.
func (s *AuthoredScheduler) refuseCandidateCycle(automation *Automation, origin string) error {
	if s.loader == nil || s.loader.functions == nil {
		if s.logger != nil {
			s.logger.Warn("static loop analysis not run for an authored automation: the scheduler's loader has no function registry",
				"component", ComponentName, "origin", origin)
		}
		return nil
	}
	shipped := s.shippedAutomations()
	candidate := *automation
	candidate.Origin = origin
	candidate.Enabled = nil // Authored activation wires even @disabled sources.
	all := append(append(make([]*Automation, 0, len(shipped)+1), shipped...), &candidate)
	s.mu.Lock()
	for _, entry := range s.entries {
		active := *entry.automation
		active.Origin = fmt.Sprintf("authored:%s:%s", entry.owner, active.Name)
		if active.Origin == origin {
			continue // The candidate replaces this version.
		}
		active.Enabled = nil
		all = append(all, &active)
	}
	s.mu.Unlock()
	g := BuildLoopGraph(all, newFunctionSource(s.loader.functions, s.loader.registry), 0)
	// A before-write body changes other automations' event payloads without
	// being an event node in their cycle. Its activation must therefore judge
	// the full resulting graph, not only cycles passing through its own node.
	if candidate.BeforeWrite != nil && len(g.Problems) > 0 {
		return fmt.Errorf("authored scheduler: %s: before-write hook may enable an uncovered cycle: %s", origin, g.Problems[0].Message)
	}
	if p, refused := g.problemThrough(origin); refused {
		return fmt.Errorf("authored scheduler: %s: %s", origin, p.Message)
	}
	return nil
}

// shippedAutomations is the tree's automations, loaded once. A load that
// fails is not cached, so the next activation tries again; until one
// succeeds a candidate is judged on its own.
func (s *AuthoredScheduler) shippedAutomations() []*Automation {
	s.shippedMu.Lock()
	defer s.shippedMu.Unlock()
	if s.shippedLoaded {
		return s.shipped
	}
	loaded, err := s.loader.LoadAll()
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("static loop analysis: the shipped automations did not load, so an authored automation is checked on its own",
				"component", ComponentName, "error", err)
		}
		return nil
	}
	s.shipped, s.shippedLoaded = loaded, true
	return loaded
}

// automationFile is the file an automation was loaded from: its origin with
// the loader's `unified:` prefix and `:<name>` suffix removed. Any other
// origin (an authored one, a test's) is returned as it is.
func automationFile(origin, name string) string {
	rest, ok := strings.CutPrefix(origin, "unified:")
	if !ok {
		return origin
	}
	if file, cut := strings.CutSuffix(rest, ":"+name); cut {
		return file
	}
	return rest
}
