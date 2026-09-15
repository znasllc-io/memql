package automations

// loop_graph_report.go -- how the static loop graph says what it found
// (memql#5381): each edge's reason, the one representative cycle a refusal
// prints, and the refusal itself, which ends with its rule id so the corpus
// runner reads the code off it (D24).

import (
	"fmt"
	"strings"
)

// cycleFix is the refusal's closing advice: the two ways out of a cycle.
const cycleFix = "Permit it with @loop(maxDepth=<n>, until=row => <done>) on one automation of the cycle, with its negation in that automation's @filter; " +
	"or give an automation a @filter this write cannot satisfy -- a trigger for first versions only is @filter(row => args.firstVersion == true)"

// kindWord names a write's kind in an edge's reason.
func kindWord(kind string) string {
	if kind == "" {
		return "a write of unknown kind"
	}
	return kind
}

// edgeReason is the sentence an edge prints: what from writes or publishes,
// through what, and whether to's @filter holds for it, could not be decided,
// or is absent.
func edgeReason(from, to *Automation, p production, t tri, why string) string {
	var b strings.Builder
	through := ""
	if len(p.via) > 0 {
		through = " through " + strings.Join(p.via, " -> ")
	}
	subject := "this write"
	switch {
	case p.kind == kindPublish && p.topic == "":
		fmt.Fprintf(&b, "%s publishes an event whose topic is known only at run time%s, and %s triggers on the raw topic %s", from.Name, through, to.Name, to.Trigger.Event)
		subject = "this event"
	case p.kind == kindPublish:
		fmt.Fprintf(&b, "%s publishes %s%s, and %s triggers on it", from.Name, p.topic, through, to.Name)
		subject = "this event"
	default:
		fmt.Fprintf(&b, "%s writes %s%s (%s), which publishes %s, and %s triggers on it", from.Name, p.concept, through, kindWord(p.kind), p.topic, to.Name)
	}
	filter := ""
	if to.Trigger != nil {
		filter = strings.TrimSpace(to.Trigger.Filter)
	}
	switch {
	case filter == "":
		b.WriteString(" with no @filter")
	case t == triTrue:
		fmt.Fprintf(&b, "; its @filter %s holds for %s", filter, subject)
	default:
		fmt.Fprintf(&b, "; its @filter %s could not be decided: %s", filter, why)
	}
	return b.String()
}

// cyclePath is the representative cycle of a component: the shortest cycle
// through its first member that is on one, found breadth-first over the
// component minus the members skip excludes -- its @loop automations, for a
// refused component, where the cycle printed must be one no @loop covers.
// The path closes on its first node: [a, b, a], or [a, a] for a self-edge.
// Nil when no cycle avoids the excluded members.
func cyclePath(members []int, succ [][]int, skip func(int) bool) []int {
	allowed := map[int]bool{}
	for _, m := range members {
		if !skip(m) {
			allowed[m] = true
		}
	}
	for _, start := range members {
		if !allowed[start] {
			continue
		}
		if path := shortestCycle(start, succ, allowed); path != nil {
			return path
		}
	}
	return nil
}

// shortestCycle is the shortest cycle through start over the allowed nodes,
// breadth-first in successor order, closing on start; nil when there is none.
func shortestCycle(start int, succ [][]int, allowed map[int]bool) []int {
	parent := map[int]int{}
	var queue []int
	for _, w := range succ[start] {
		if !allowed[w] {
			continue
		}
		if w == start {
			return []int{start, start}
		}
		if _, seen := parent[w]; !seen {
			parent[w] = start
			queue = append(queue, w)
		}
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, w := range succ[v] {
			if !allowed[w] {
				continue
			}
			if w == start {
				var back []int
				for x := v; x != start; x = parent[x] {
					back = append(back, x)
				}
				path := []int{start}
				for i := len(back) - 1; i >= 0; i-- {
					path = append(path, back[i])
				}
				return append(path, start)
			}
			if _, seen := parent[w]; !seen && w != start {
				parent[w] = v
				queue = append(queue, w)
			}
		}
	}
	return nil
}

// renderCycleProblem is the loop_cycle refusal for one cycle path: the cycle,
// one line per edge with its reason, and the fix.
//
//	automation cycle not covered by @loop: a -> b -> a
//	  a -> b: <reason>
//	  b -> a: <reason>
//	  Permit it with @loop(...) ... [loop_cycle]
func renderCycleProblem(g *LoopGraph, path []int) string {
	idx := g.idx
	names := make([]string, len(path))
	for i, v := range path {
		names[i] = idx.nodes[v].Name
	}
	var b strings.Builder
	b.WriteString("automation cycle not covered by @loop: ")
	b.WriteString(strings.Join(names, " -> "))
	for k := 0; k+1 < len(path); k++ {
		if e, ok := idx.edge[[2]int{path[k], path[k+1]}]; ok {
			fmt.Fprintf(&b, "\n  %s -> %s: %s", g.Edges[e].From, g.Edges[e].To, g.Edges[e].Reason)
		}
	}
	fmt.Fprintf(&b, "\n  %s [%s]", cycleFix, codeLoopCycle)
	return b.String()
}

// problemThrough is the loop_cycle refusal for a cycle through the
// automation at origin that no @loop covers: the shortest such cycle through
// it, over its component minus the @loop automations. ok is false when the
// automation carries @loop (a cycle through it is covered), lies on no cycle,
// or lies only on cycles another @loop covers.
//
// It is how an authored automation is judged at activation: the tree may
// already hold a cycle (reported at boot), and the candidate is refused for a
// cycle it closes, never for one it merely sits beside.
func (g *LoopGraph) problemThrough(origin string) (LoopProblem, bool) {
	idx := g.idx
	if idx == nil {
		return LoopProblem{}, false
	}
	at := -1
	for i, a := range idx.nodes {
		if a.Origin == origin {
			at = i
			break
		}
	}
	if at < 0 || idx.nodes[at].Loop != nil {
		return LoopProblem{}, false
	}
	allowed := map[int]bool{}
	for _, m := range idx.comps[idx.comp[at]] {
		if idx.nodes[m].Loop == nil {
			allowed[m] = true
		}
	}
	path := shortestCycle(at, idx.succ, allowed)
	if path == nil {
		return LoopProblem{}, false
	}
	a := idx.nodes[at]
	return LoopProblem{Automation: a.Name, Origin: a.Origin, Code: codeLoopCycle, Message: renderCycleProblem(g, path)}, true
}
