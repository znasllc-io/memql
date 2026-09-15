package automations

// loop_graph_test.go -- the static graph over automations, their writes and
// their triggers, and the refusal of a cycle no @loop covers (memql#5381,
// D-H and D-I of the loop protection plan). DB-free: the call graph is a
// literal work.Registry behind fakeSource, and the automations are compiled
// from source the way the tree loader compiles them. @loop is set on the
// compiled automation in Go, because the annotation's parser support lands in
// a parallel stream.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// fakeSource is a FunctionSource over a literal registry.
type fakeSource struct{ reg work.Registry }

func (f fakeSource) Registry() work.Registry { return f.reg }

// graphAutomation compiles one automation and stamps the origin a loader
// stamps, which the graph reports problems under.
func graphAutomation(t *testing.T, src string) *Automation {
	t.Helper()
	a, err := NewLoader(LoaderOptions{}).CompileSource(src, "test")
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, src)
	}
	a.Origin = "test:" + a.Name
	return a
}

func graphAutomations(t *testing.T, srcs ...string) []*Automation {
	t.Helper()
	out := make([]*Automation, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, graphAutomation(t, src))
	}
	return out
}

const thingConcept = "v1:t:thing"

// thingReg is the call graph most cases share.
func thingReg() work.Registry {
	return work.Registry{
		"advanceThing": {ConstructKind: work.ConstructMutation, Concept: thingConcept, Write: &work.WriteSpec{
			Kind: "update", Fields: map[string]work.FieldValue{"status": {Arg: "s"}},
		}},
		"finishThing": {ConstructKind: work.ConstructMutation, Concept: thingConcept, Write: &work.WriteSpec{
			Kind: "update", Fields: map[string]work.FieldValue{"status": {Literal: "done", HasLiteral: true}},
		}},
		"createThing": {ConstructKind: work.ConstructMutation, Concept: thingConcept, Write: &work.WriteSpec{
			Kind: "insert", NewRow: true, Fields: map[string]work.FieldValue{"status": {Literal: "new", HasLiteral: true}},
		}},
		"recordOther": {ConstructKind: work.ConstructMutation, Concept: "v1:t:other", Write: &work.WriteSpec{Kind: "insert", NewRow: true}},
		"findThing":   {ConstructKind: work.ConstructQuery},
		"decideThing": {ConstructKind: work.ConstructLogic, Calls: []string{"findThing", "finishThing"}},
		"sendThing":   {ConstructKind: work.ConstructBuiltin},
	}
}

func graphNode(t *testing.T, g *LoopGraph, name string) GraphAutomation {
	t.Helper()
	for _, a := range g.Automations {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no automation %q in the graph", name)
	return GraphAutomation{}
}

func graphEdge(g *LoopGraph, from, to string) *GraphEdge {
	for i := range g.Edges {
		if g.Edges[i].From == from && g.Edges[i].To == to {
			return &g.Edges[i]
		}
	}
	return nil
}

// corpusCode is the corpus runner's reading of a refusal's rule id.
var corpusCode = regexp.MustCompile(`\[([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\]\s*$`)

func TestLoopGraph_SelfEdge(t *testing.T) {
	as := graphAutomations(t, `@trigger(event="node.created", concept="v1:t:thing")
automation a {
  args {
    id any
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	e := graphEdge(g, "a", "a")
	if e == nil || !e.Decided || e.Concept != thingConcept || e.Topic != "graph.node.created.v1:t:thing" {
		t.Fatalf("self-edge = %+v, want a decided edge on graph.node.created.v1:t:thing", e)
	}
	if strings.Join(e.Via, ",") != "advanceThing" {
		t.Errorf("via = %v, want [advanceThing]", e.Via)
	}
	if len(g.Cycles) != 1 || strings.Join(g.Cycles[0].Path, " -> ") != "a -> a" || len(g.Cycles[0].PermittedBy) != 0 {
		t.Fatalf("cycles = %+v, want one uncovered a -> a", g.Cycles)
	}
	if len(g.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", g.Problems)
	}
	p := g.Problems[0]
	if p.Code != "loop_cycle" || p.Automation != "a" || p.Origin != "test:a" {
		t.Errorf("problem = %+v, want loop_cycle on a (test:a)", p)
	}
	if !strings.HasPrefix(p.Message, "automation cycle not covered by @loop: a -> a\n") {
		t.Errorf("message does not open with the cycle:\n%s", p.Message)
	}
	if m := corpusCode.FindStringSubmatch(p.Message); m == nil || m[1] != "loop_cycle" {
		t.Errorf("the corpus reads the code %v from:\n%s", m, p.Message)
	}
	if n := graphNode(t, g, "a"); n.Cycle != 0 || strings.Join(n.Writes, ",") != thingConcept {
		t.Errorf("node = %+v, want cycle 0 and writes [%s]", n, thingConcept)
	}
}

func TestLoopGraph_RefutedByKind(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(schedule="0 */5 * * * *")
automation a {
  step create {
    mutation createThing()
  }
}`,
		`@trigger(event="node.updated", concept="v1:t:thing")
automation b {
  args {
    id any
  }
  step record {
    mutation recordOther(id: id)
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "a", "b"); e != nil {
		t.Fatalf("an insert publishes no graph.node.updated, yet a -> b: %+v", e)
	}
	if len(g.Edges) != 0 || len(g.Problems) != 0 {
		t.Errorf("edges %+v, problems %+v; want none", g.Edges, g.Problems)
	}
}

func TestLoopGraph_RefutedByTheFilter(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(schedule="0 */5 * * * *")
automation a {
  args {
    id any
  }
  step finish {
    mutation finishThing(id: id)
  }
}`,
		`@trigger(event="node.created", concept="v1:t:thing")
@filter(row => row.status == "open")
automation b {
  args {
    id any
  }
  step record {
    mutation recordOther(id: id)
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "a", "b"); e != nil {
		t.Fatalf("finishThing writes status \"done\", which b's filter refuses, yet a -> b: %+v", e)
	}
}

// undecidedPair is a writer passing `s` to advanceThing and a reader whose
// filter reads the status that write sets.
func undecidedPair(t *testing.T, sArg string) []*Automation {
	return graphAutomations(t,
		`@trigger(event="tick.a")
automation a {
  args {
    id any
    x any
  }
  step advance {
    mutation advanceThing(id: id, s: `+sArg+`)
  }
}`,
		`@trigger(event="node.created", concept="v1:t:thing")
@filter(row => row.status == "open")
automation b {
  args {
    id any
  }
  step record {
    mutation recordOther(id: id)
  }
}`)
}

func TestLoopGraph_Undecided(t *testing.T) {
	g := BuildLoopGraph(undecidedPair(t, "x"), fakeSource{thingReg()}, 0)
	e := graphEdge(g, "a", "b")
	if e == nil {
		t.Fatal("an undecided filter must keep the edge")
	}
	if e.Decided {
		t.Errorf("edge %+v is decided; the call site passes an expression for s", e)
	}
	for _, want := range []string{"status", `its @filter row => row.status == "open" could not be decided`, "advanceThing sets status to args.s, known only at run time"} {
		if !strings.Contains(e.Reason, want) {
			t.Errorf("reason does not say %q:\n%s", want, e.Reason)
		}
	}
}

func TestLoopGraph_ResolvedAtTheCallSite(t *testing.T) {
	g := BuildLoopGraph(undecidedPair(t, `"open"`), fakeSource{thingReg()}, 0)
	e := graphEdge(g, "a", "b")
	if e == nil || !e.Decided {
		t.Fatalf("the call site passes s: \"open\", which b's filter holds for: edge = %+v", e)
	}
	if !strings.Contains(e.Reason, `its @filter row => row.status == "open" holds for this write`) {
		t.Errorf("reason = %s", e.Reason)
	}
	// And a literal the filter refuses removes it.
	if e := graphEdge(BuildLoopGraph(undecidedPair(t, `"held"`), fakeSource{thingReg()}, 0), "a", "b"); e != nil {
		t.Errorf("s: \"held\" refutes the filter, yet a -> b: %+v", e)
	}
}

func TestLoopGraph_FirstVersion(t *testing.T) {
	reader := `@trigger(event="node.created", concept="v1:t:thing")
@filter(row => args.firstVersion == true)
automation b {
  args {
    firstVersion bool
    id           any
  }
  step record {
    mutation recordOther(id: id)
  }
}`
	updater := `@trigger(event="tick.a")
automation a {
  args {
    id any
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
}`
	g := BuildLoopGraph(graphAutomations(t, updater, reader), fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "a", "b"); e != nil {
		t.Fatalf("an update is never a first version, yet a -> b: %+v", e)
	}
	// The control: an insert of a new row is one.
	creator := `@trigger(event="tick.c")
automation c {
  step create {
    mutation createThing()
  }
}`
	g = BuildLoopGraph(graphAutomations(t, creator, reader), fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "c", "b"); e == nil || !e.Decided {
		t.Fatalf("an insert of a new row is its first version: edge = %+v", e)
	}
}

// A filter's `args` binds only the fields the JUDGED automation's args block
// declares (bindEventArgs), and nothing with no block: an undeclared field is
// absent at run time whatever the write sets. So `args.status != "done"`
// fires on a write stamping "done", and that edge must stay; and a
// first-version filter on an automation that does not declare firstVersion
// never fires, so no write reaches it.
func TestLoopGraph_UndeclaredArgsReadAbsent(t *testing.T) {
	finisher := `@trigger(event="tick.a")
automation a {
  step finish {
    mutation finishThing()
  }
}`
	reader := func(args string) string {
		return `@trigger(event="node.created", concept="v1:t:thing")
@filter(row => args.status != "done")
automation b {
` + args + `  step record {
    mutation recordOther()
  }
}`
	}
	for name, args := range map[string]string{
		"no args block":                "",
		"an args block without status": "  args {\n    id any\n  }\n",
	} {
		g := BuildLoopGraph(graphAutomations(t, finisher, reader(args)), fakeSource{thingReg()}, 0)
		if e := graphEdge(g, "a", "b"); e == nil || !e.Decided {
			t.Errorf("%s: args.status is absent at run time, so != \"done\" holds and b fires: edge = %+v", name, e)
		}
	}
	// The control: declared, args.status is the "done" the write stamps.
	g := BuildLoopGraph(graphAutomations(t, finisher, reader("  args {\n    status any\n  }\n")), fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "a", "b"); e != nil {
		t.Errorf("b declares status, which reads \"done\", yet a -> b: %+v", e)
	}

	creator := `@trigger(event="tick.c")
automation c {
  step create {
    mutation createThing()
  }
}`
	firstOnly := `@trigger(event="node.created", concept="v1:t:thing")
@filter(row => args.firstVersion == true)
automation f {
  args {
    id any
  }
  step record {
    mutation recordOther(id: id)
  }
}`
	g = BuildLoopGraph(graphAutomations(t, creator, firstOnly), fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "c", "f"); e != nil {
		t.Errorf("f does not declare firstVersion, so its filter reads absent and never holds, yet c -> f: %+v", e)
	}
}

// libraryGraph is the library's archive pair as the tree ships it, compiled
// from the embedded tree, over its real mutations (loop_graph_source.go
// reads them from the loaded function registry), plus any extra automations.
func libraryGraph(t *testing.T, extra ...string) *LoopGraph {
	t.Helper()
	var as []*Automation
	for _, name := range []string{"archiveFileOnArtifactArchive", "indexFileOnCreate"} {
		as = append(as, treeAutomation(t, "library/automations.memql", name))
	}
	as = append(as, graphAutomations(t, extra...)...)
	src := NewFunctionSource(functionsFromTree(t, map[string][]string{
		"library/mutations.memql": {"archiveLibraryFile", "createArtifact", "archiveArtifact"},
	}))
	return BuildLoopGraph(as, src, 0)
}

func TestLoopGraph_TheLibraryPair(t *testing.T) {
	g := libraryGraph(t)
	e := graphEdge(g, "archiveFileOnArtifactArchive", "indexFileOnCreate")
	if e == nil {
		t.Fatal("archiveLibraryFile's update publishes graph.node.created on the file, which indexFileOnCreate triggers on")
	}
	if e.Decided || !strings.Contains(e.Reason, "status") {
		t.Errorf("the edge is undecided on the status the update inherits: %+v", e)
	}
	if back := graphEdge(g, "indexFileOnCreate", "archiveFileOnArtifactArchive"); back != nil {
		t.Errorf("createArtifact is an insert, which publishes no graph.node.updated, yet the edge back exists: %+v", back)
	}
	if len(g.Cycles) != 0 || len(g.Problems) != 0 {
		t.Errorf("the pair is not a cycle: cycles %+v, problems %+v", g.Cycles, g.Problems)
	}
}

func TestLoopGraph_TheMirrorFixture(t *testing.T) {
	g := libraryGraph(t, `@trigger(event="node.updated", concept="v1:library:file")
@filter(row => row.archived == true)
automation archiveArtifactOnFileArchive {
  args {
    artifactId any
  }
  step archiveIndex {
    mutation archiveArtifact(artifactId: artifactId)
  }
}`)
	if len(g.Problems) != 1 || g.Problems[0].Code != "loop_cycle" {
		t.Fatalf("problems = %+v, want the one loop_cycle", g.Problems)
	}
	msg := g.Problems[0].Message
	for _, edge := range []string{"archiveFileOnArtifactArchive -> archiveArtifactOnFileArchive: ", "archiveArtifactOnFileArchive -> archiveFileOnArtifactArchive: "} {
		if !strings.Contains(msg, "\n  "+edge) {
			t.Errorf("the refusal does not name the edge %q:\n%s", edge, msg)
		}
	}
}

// loopSelfCycle is an automation whose own update may fire it again: the
// status it writes is known only at run time.
const loopSelfCycle = `@trigger(event="node.created", concept="v1:t:thing")
@filter(row => row.status != "done")
automation a {
  args {
    id any
    x  any
  }
  step advance {
    mutation advanceThing(id: id, s: x)
  }
}`

func withLoop(a *Automation) *Automation {
	a.Loop = &LoopConfig{MaxDepth: 3, Until: `row => row.status == "done"`}
	return a
}

func TestLoopGraph_Permitted(t *testing.T) {
	g := BuildLoopGraph([]*Automation{withLoop(graphAutomation(t, loopSelfCycle))}, fakeSource{thingReg()}, 0)
	if len(g.Problems) != 0 {
		t.Fatalf("a @loop covers the cycle, yet: %+v", g.Problems)
	}
	if len(g.Cycles) != 1 || strings.Join(g.Cycles[0].PermittedBy, ",") != "a" {
		t.Fatalf("cycles = %+v, want one permitted by a", g.Cycles)
	}
	if n := graphNode(t, g, "a"); n.Cycle != 0 || n.Loop == nil || n.Loop.MaxDepth != 3 {
		t.Errorf("node = %+v, want cycle 0 carrying the @loop", n)
	}
	// The control: the same automation without the @loop is refused.
	g = BuildLoopGraph([]*Automation{graphAutomation(t, loopSelfCycle)}, fakeSource{thingReg()}, 0)
	if len(g.Problems) != 1 || g.Problems[0].Code != "loop_cycle" {
		t.Fatalf("without @loop the cycle is refused: %+v", g.Problems)
	}
}

// twoCycles is a <-> b and b <-> c, on three concepts.
func twoCycles(t *testing.T) ([]*Automation, work.Registry) {
	reg := work.Registry{}
	for _, c := range []string{"one", "two", "three"} {
		reg["update"+strings.ToUpper(c[:1])+c[1:]] = work.Target{ConstructKind: work.ConstructMutation, Concept: "v1:t:" + c,
			Write: &work.WriteSpec{Kind: "update", Fields: map[string]work.FieldValue{"status": {Arg: "s"}}}}
	}
	as := graphAutomations(t,
		`@trigger(event="node.created", concept="v1:t:one")
@filter(row => row.status != "done")
automation a {
  args {
    id any
    x  any
  }
  step toTwo {
    mutation updateTwo(id: id, s: x)
  }
}`,
		`@trigger(event="node.created", concept="v1:t:two")
automation b {
  args {
    id any
    x  any
  }
  step toOne {
    mutation updateOne(id: id, s: x)
  }
  step toThree {
    mutation updateThree(id: id, s: x)
  }
}`,
		`@trigger(event="node.created", concept="v1:t:three")
@filter(row => row.status != "done")
automation c {
  args {
    id any
    x  any
  }
  step toTwo {
    mutation updateTwo(id: id, s: x)
  }
}`)
	return as, reg
}

func TestLoopGraph_TwoLoopFreeCyclesOneLoop(t *testing.T) {
	as, reg := twoCycles(t)
	withLoop(as[0]) // a
	g := BuildLoopGraph(as, fakeSource{reg}, 0)
	if len(g.Problems) != 1 {
		t.Fatalf("b <-> c passes through no @loop, so the cycle is refused: %+v", g.Problems)
	}
	p := g.Problems[0]
	if p.Automation != "b" || !strings.HasPrefix(p.Message, "automation cycle not covered by @loop: b -> c -> b\n") {
		t.Errorf("the refusal names the shortest uncovered cycle through b:\n%+v", p)
	}
	if len(g.Cycles) != 1 || strings.Join(g.Cycles[0].Members, ",") != "a,b,c" || len(g.Cycles[0].PermittedBy) != 0 {
		t.Errorf("cycles = %+v, want the one SCC {a, b, c}, not permitted", g.Cycles)
	}
	// The control: a @loop on c too, and every cycle passes through one.
	as, reg = twoCycles(t)
	withLoop(as[0])
	withLoop(as[2])
	g = BuildLoopGraph(as, fakeSource{reg}, 0)
	if len(g.Problems) != 0 || len(g.Cycles) != 1 || strings.Join(g.Cycles[0].PermittedBy, ",") != "a,c" {
		t.Fatalf("with @loop on a and c the SCC is permitted: cycles %+v, problems %+v", g.Cycles, g.Problems)
	}
}

func TestLoopGraph_Strata(t *testing.T) {
	reg := work.Registry{}
	for _, c := range []string{"one", "two", "three"} {
		reg["create"+strings.ToUpper(c[:1])+c[1:]] = work.Target{ConstructKind: work.ConstructMutation, Concept: "v1:t:" + c,
			Write: &work.WriteSpec{Kind: "insert", NewRow: true}}
	}
	source := `@trigger(event="tick.a")
automation a {
  step s {
    mutation createOne()
  }
}`
	reader := func(name, on, writes string) string {
		return `@trigger(event="node.created", concept="v1:t:` + on + `")
automation ` + name + ` {
  step s {
    mutation ` + writes + `()
  }
}`
	}
	g := BuildLoopGraph(graphAutomations(t, source, reader("b", "one", "createTwo"), reader("c", "two", "createThree")), fakeSource{reg}, 0)
	for name, want := range map[string]int{"a": 0, "b": 1, "c": 2} {
		if got := graphNode(t, g, name).Stratum; got != want {
			t.Errorf("chain: %s is at stratum %d, want %d", name, got, want)
		}
	}
	g = BuildLoopGraph(graphAutomations(t, source, reader("b", "one", "createTwo"), reader("c", "two", "createOne")), fakeSource{reg}, 0)
	if sb, sc := graphNode(t, g, "b").Stratum, graphNode(t, g, "c").Stratum; sb != sc || sb != 1 || graphNode(t, g, "a").Stratum != 0 {
		t.Errorf("an SCC {b, c} shares one stratum after a: a=%d b=%d c=%d", graphNode(t, g, "a").Stratum, sb, sc)
	}
}

func TestLoopGraph_TopicEdges(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(event="node.created", concept="v1:t:source")
automation p {
  step s {
    publishEvent(topic: "x.y", payload: {a: 1})
  }
}`,
		`@trigger(event="node.created", concept="v1:t:source2")
automation r {
  args {
    kind any
  }
  step s {
    publishEvent(topic: "x." + kind, payload: {a: 1})
  }
}`,
		`@trigger(event="x.y")
automation q {
  step s {
    mutation recordOther()
  }
}`,
		`@trigger(event="system.startup")
automation s {
  step s {
    mutation recordOther()
  }
}`,
		`@trigger(event="x.z")
@filter(row => row.a == 2)
automation v {
  step s {
    mutation recordOther()
  }
}`,
		`@trigger(event="node.created", concept="v1:t:thing")
automation u {
  step s {
    mutation recordOther()
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	if e := graphEdge(g, "p", "q"); e == nil || !e.Decided || e.Topic != "x.y" {
		t.Errorf("a publish of x.y reaches the automation on x.y: %+v", e)
	}
	for _, to := range []string{"q", "s"} {
		e := graphEdge(g, "r", to)
		if e == nil || e.Decided || e.Topic != "" || !strings.Contains(e.Reason, "known only at run time") {
			t.Errorf("an expression topic reaches every raw-topic automation, undecided: r -> %s = %+v", to, e)
		}
	}
	if e := graphEdge(g, "r", "v"); e != nil {
		t.Errorf("v's filter refuses the payload r publishes whatever the topic, yet r -> v: %+v", e)
	}
	for _, to := range []string{"u", "p", "r"} {
		if e := graphEdge(g, "r", to); e != nil {
			t.Errorf("an expression topic is not a graph write, yet r -> %s: %+v", to, e)
		}
	}
	if pubs := graphNode(t, g, "r").Publishes; strings.Join(pubs, ",") != topicKnownAtRunTime {
		t.Errorf("r publishes %v, want [%s]", pubs, topicKnownAtRunTime)
	}
}

func TestLoopGraph_SubAutomations(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(event="tick.a")
automation a {
  args {
    id any
  }
  step s {
    automation subAdvance(id: id)
  }
}`,
		`automation subAdvance {
  args {
    id any
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
  step recurse {
    automation subBack(id: id)
  }
}`,
		`automation subBack {
  args {
    id any
  }
  step again {
    automation subAdvance(id: id)
  }
}`,
		`@trigger(event="node.updated", concept="v1:t:thing")
automation b {
  step s {
    mutation recordOther()
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	if w := graphNode(t, g, "a").Writes; strings.Join(w, ",") != thingConcept {
		t.Errorf("a writes %v, want the sub-automation's %s", w, thingConcept)
	}
	e := graphEdge(g, "a", "b")
	if e == nil || strings.Join(e.Via, ",") != "subAdvance,advanceThing" {
		t.Fatalf("a reaches b through its sub-automation's write: %+v", e)
	}
	if !strings.Contains(e.Reason, "through subAdvance -> advanceThing (update)") {
		t.Errorf("reason = %s", e.Reason)
	}
	if e := graphEdge(g, "subBack", "b"); e == nil {
		t.Error("the write a mutually-recursive sub-automation reaches is still its write")
	}
}

func TestLoopGraph_Coverage(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(event="tick.a")
automation a {
  args {
    id any
  }
  step find {
    query findThing(id: id)
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
  step decide {
    logic decideThing(id: id)
  }
  step send {
    builtin sendThing(id: id)
  }
  step act {
    action doThing(id: id)
  }
  step lost {
    mutation noSuchThing(id: id)
  }
  step sub {
    automation subX(id: id)
  }
  step missing {
    automation noSuchAutomation(id: id)
  }
}`,
		`automation subX {
  args {
    id any
  }
  step find {
    query findThing(id: id)
  }
}`)
	g := BuildLoopGraph(as, fakeSource{thingReg()}, 0)
	want := GraphCoverage{Automations: 2, Resolved: 5, Unresolved: 2, Opaque: 2}
	if g.Coverage != want {
		t.Errorf("coverage = %+v, want %+v", g.Coverage, want)
	}
	if o := graphNode(t, g, "a").Opaque; strings.Join(o, ",") != "action doThing,builtin sendThing" {
		t.Errorf("opaque = %v", o)
	}
	// With no function registry every construct call is unresolved: the
	// check did not run, and the coverage says so rather than reading clean.
	g = BuildLoopGraph(as, nil, 0)
	want = GraphCoverage{Automations: 2, Resolved: 1, Unresolved: 7, Opaque: 1}
	if g.Coverage != want {
		t.Errorf("coverage with no registry = %+v, want %+v", g.Coverage, want)
	}
}

// unfilteredRouteRequest is the shape forge's routeRequest had before its
// first-version filter (memql#5381): it fires on every write to a request and
// advances the request it fired on, so it re-fires on its own advance. The
// shipped automation carries the filter now, so the tests that need a shipped
// cycle hold this copy of the old one.
const unfilteredRouteRequest = `@trigger(event="node.created", concept="v1:forge:request")
automation routeRequest {
  args {
    id any
  }
  step advance {
    mutation advanceRequest(requestId: id, status: "queued")
  }
  step persistRouted {
    mutation recordRequestEvent(requestId: id, kind: "routed", fromStatus: "submitted", note: "routed by submitter role")
  }
}`

// The refusal as the plan writes it, over routeRequest as it was before its
// first-version filter.
func TestLoopGraph_RefusalText(t *testing.T) {
	reg := work.Registry{
		"requestRouteStatus": {ConstructKind: work.ConstructLogic},
		"advanceRequest": {ConstructKind: work.ConstructMutation, Concept: "v1:forge:request", Write: &work.WriteSpec{
			Kind: "update", Fields: map[string]work.FieldValue{"status": {Arg: "status"}, "approvedByUserId": {Arg: "approvedByUserId"}},
		}},
		"recordRequestEvent": {ConstructKind: work.ConstructMutation, Concept: "v1:forge:requestEvent", Write: &work.WriteSpec{Kind: "insert"}},
	}
	as := graphAutomations(t, unfilteredRouteRequest)
	g := BuildLoopGraph(as, fakeSource{reg}, 0)
	want := "automation cycle not covered by @loop: routeRequest -> routeRequest\n" +
		"  routeRequest -> routeRequest: routeRequest writes v1:forge:request through advanceRequest (update), which publishes graph.node.created.v1:forge:request, and routeRequest triggers on it with no @filter\n" +
		"  Permit it with @loop(maxDepth=<n>, until=row => <done>) on one automation of the cycle, with its negation in that automation's @filter; or give an automation a @filter this write cannot satisfy -- a trigger for first versions only declares `firstVersion bool` in its args block and filters `@filter(row => args.firstVersion == true)` [loop_cycle]"
	if len(g.Problems) != 1 || g.Problems[0].Message != want {
		t.Fatalf("problems = %+v\nwant the message\n%s", g.Problems, want)
	}

	// An undecided edge ends with what could not be decided, and why.
	as = graphAutomations(t, `@trigger(event="node.created", concept="v1:forge:request")
@filter(row => row.status == "submitted")
automation routeSubmitted {
  args {
    id     any
    status any
  }
  step advance {
    mutation advanceRequest(requestId: id, status: status)
  }
}`)
	g = BuildLoopGraph(as, fakeSource{reg}, 0)
	if len(g.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", g.Problems)
	}
	line := strings.Split(g.Problems[0].Message, "\n")[1]
	if suffix := `; its @filter row => row.status == "submitted" could not be decided: advanceRequest sets status to args.status, known only at run time`; !strings.HasSuffix(line, suffix) {
		t.Errorf("the edge line\n%s\ndoes not end\n%s", line, suffix)
	}
}

// An automation built in Go -- a sandbox bundle, a LogicRunner body -- can
// carry the two write shapes the compiler does not emit for a v1 automation:
// a query step whose expression is a construct call, which the query executor
// runs on the engine, and an inline mutation step, which inserts. Both are
// writes, read the same way prepared or not.
func TestLoopGraph_StepsBuiltInGo(t *testing.T) {
	build := func(prepare bool) []*Automation {
		as := []*Automation{
			{Name: "q", Origin: "test:q", Trigger: &TriggerConfig{Event: "tick.q"}, Steps: []*Step{
				{ID: "s", Type: StepTypeQuery, Query: &QueryStepConfig{Query: `mutation advanceThing(id: "t-1", s: "open")`}},
			}},
			{Name: "m", Origin: "test:m", Trigger: &TriggerConfig{Event: "tick.m"}, Steps: []*Step{
				{ID: "ins", Type: StepTypeMutation, Mutation: &MutationStepConfig{Concept: "v1:t:other", Payload: map[string]any{
					"status": "new",
					"tags":   []any{"a", "b"},
					"meta":   map[string]any{"k": "v"},
					"who":    map[string]any{exprLeafKey: "actor.userId"},
				}}},
			}},
			{Name: "readThing", Origin: "test:readThing", Trigger: &TriggerConfig{Event: "graph.node.created.v1:t:thing", Filter: `row => row.status == "open"`}},
			{Name: "readNew", Origin: "test:readNew", Trigger: &TriggerConfig{Event: "graph.node.created.v1:t:other", Filter: `row => row.status == "new" && row.note == nil`}},
			{Name: "readOld", Origin: "test:readOld", Trigger: &TriggerConfig{Event: "graph.node.created.v1:t:other", Filter: `row => row.status == "old"`}},
			{Name: "readWho", Origin: "test:readWho", Trigger: &TriggerConfig{Event: "graph.node.created.v1:t:other", Filter: `row => row.who == "x"`}},
		}
		if prepare {
			for _, a := range as {
				if err := PrepareExpressions(a); err != nil {
					t.Fatalf("prepare %s: %v", a.Name, err)
				}
			}
		}
		return as
	}
	for _, prepared := range []bool{false, true} {
		g := BuildLoopGraph(build(prepared), fakeSource{thingReg()}, 0)
		if e := graphEdge(g, "q", "readThing"); e == nil || !e.Decided || strings.Join(e.Via, ",") != "advanceThing" {
			t.Errorf("prepared=%v: a query step's construct call writes through advanceThing with s \"open\": %+v", prepared, e)
		}
		// A new row: status is the literal, note is absent.
		if e := graphEdge(g, "m", "readNew"); e == nil || !e.Decided || strings.Join(e.Via, ",") != "step ins" {
			t.Errorf("prepared=%v: an inline insert of a new row writes status \"new\" and no note: %+v", prepared, e)
		}
		if e := graphEdge(g, "m", "readOld"); e != nil {
			t.Errorf("prepared=%v: status \"new\" refutes row.status == \"old\", yet: %+v", prepared, e)
		}
		if e := graphEdge(g, "m", "readWho"); e == nil || e.Decided {
			t.Errorf("prepared=%v: who is an expression, so the edge stays undecided: %+v", prepared, e)
		}
		if w := graphNode(t, g, "m").Writes; strings.Join(w, ",") != "v1:t:other" {
			t.Errorf("prepared=%v: m writes %v", prepared, w)
		}
	}
}

// problemThrough judges one automation: refused for a cycle through it no
// @loop covers, and for nothing else -- not a cycle it carries the @loop of,
// not a cycle another member's @loop covers, not a cycle it sits beside.
func TestLoopGraph_ProblemThrough(t *testing.T) {
	loopy := graphAutomation(t, loopSelfCycle) // a, a self-cycle
	g := BuildLoopGraph([]*Automation{loopy}, fakeSource{thingReg()}, 0)
	if p, ok := g.problemThrough("test:a"); !ok || p.Code != "loop_cycle" || !strings.HasPrefix(p.Message, "automation cycle not covered by @loop: a -> a\n") {
		t.Fatalf("a self-cycle with no @loop: %+v, %v", p, ok)
	}
	g = BuildLoopGraph([]*Automation{withLoop(graphAutomation(t, loopSelfCycle))}, fakeSource{thingReg()}, 0)
	if p, ok := g.problemThrough("test:a"); ok {
		t.Errorf("a self-cycle the automation's own @loop covers was refused: %+v", p)
	}

	// b <-> c with a @loop on c: b's only cycle passes c.
	as, reg := twoCycles(t)
	withLoop(as[2])
	g = BuildLoopGraph(as[1:], fakeSource{reg}, 0)
	if p, ok := g.problemThrough("test:b"); ok {
		t.Errorf("b's cycle passes c's @loop, yet: %+v", p)
	}

	// A bystander: an automation in no cycle beside a refused one.
	bystander := graphAutomation(t, `@trigger(event="tick.z")
automation z {
  step s {
    mutation recordOther()
  }
}`)
	g = BuildLoopGraph([]*Automation{graphAutomation(t, loopSelfCycle), bystander}, fakeSource{thingReg()}, 0)
	if p, ok := g.problemThrough("test:z"); ok {
		t.Errorf("z is in no cycle, yet: %+v", p)
	}
	if _, ok := g.problemThrough("test:nobody"); ok {
		t.Error("an origin the graph does not hold was refused")
	}
}

// A @loop bound outside [1, depthCap] is refused by the graph as by the load.
func TestLoopGraph_LoopBoundOutsideTheCap(t *testing.T) {
	a := withLoop(graphAutomation(t, loopSelfCycle))
	a.Loop.MaxDepth = 17
	g := BuildLoopGraph([]*Automation{a}, fakeSource{thingReg()}, 16)
	var codes []string
	for _, p := range g.Problems {
		codes = append(codes, p.Code)
	}
	if strings.Join(codes, ",") != "loop_max_depth_range" {
		t.Fatalf("problems = %+v, want the one loop_max_depth_range", g.Problems)
	}
	if !strings.HasSuffix(g.Problems[0].Message, "[loop_max_depth_range]") {
		t.Errorf("message = %s", g.Problems[0].Message)
	}
	// No cap, no range check: the load's own prepare step owns it.
	if g := BuildLoopGraph([]*Automation{a}, fakeSource{thingReg()}, 0); len(g.Problems) != 0 {
		t.Errorf("depthCap 0 checks no range: %+v", g.Problems)
	}
}

// The graph is printed in a refusal and drawn in the OS: the same input in any
// order builds the same graph.
func TestLoopGraph_Deterministic(t *testing.T) {
	as, reg := twoCycles(t)
	extra := graphAutomations(t, strings.Replace(loopSelfCycle, "automation a {", "automation d {", 1), `@trigger(event="tick.z")
automation z {
  step s {
    publishEvent(topic: "x.y", payload: {})
  }
}`)
	all := append(as, extra...)
	for k, v := range thingReg() {
		reg[k] = v
	}
	render := func(in []*Automation) string {
		b, err := json.Marshal(BuildLoopGraph(in, fakeSource{reg}, 0))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := render(all)
	reversed := make([]*Automation, len(all))
	for i, a := range all {
		reversed[len(all)-1-i] = a
	}
	for i := 0; i < 5; i++ {
		if got := render(all); got != first {
			t.Fatalf("run %d built a different graph", i)
		}
	}
	if got := render(reversed); got != first {
		t.Fatalf("the input order changed the graph:\n%s\nvs\n%s", first, got)
	}
}
