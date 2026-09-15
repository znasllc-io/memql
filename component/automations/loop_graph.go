package automations

// loop_graph.go -- the static graph over automations, their writes and their
// triggers (memql#5381; D-H and D-I of the loop protection plan).
//
// THE RULE. A -> B iff one of A's writes produces a topic B's trigger pattern
// matches (events.Match), and B's @filter is not refuted by what the write is
// known to set (loop_graph_filter.go). Every edge carries its reason and the
// call path that reaches the write, because a cycle is refused by printing
// it, and a refusal that says "somewhere in here" is not one an author can
// act on.
//
// WHAT A WRITE PUBLISHES. Every successful write publishes
// graph.node.created.<concept> -- MemQL is append-only, so an update is a new
// row too -- and an update ALSO publishes graph.node.updated.<concept>
// (component/memql/executor_mutation.go). Nothing publishes
// graph.node.deleted. An event step publishes its topic; one whose topic is an
// expression is taken to reach every automation on a non-graph topic, with
// the edge undecided.
//
// WHAT A's WRITES ARE. The mutations its steps call, directly and through
// logic, transitively (work.UnionWrites); an inline mutation step; and,
// through a sub-automation it calls, whatever that one writes and publishes.
// Builtins and actions are OPAQUE: no edges, listed on the node so the gap is
// visible. The engine's own journal writes (v1:work:*) are not modelled --
// journalSkipsAutomation is their guard.
//
// THE REFUSAL. A cycle is permitted when every cycle through its strongly
// connected component passes an automation carrying @loop, which bounds it
// at run time (D-H); any other cycle is refused as loop_cycle, printing one
// representative path with each edge's reason (loop_graph_report.go).

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
)

// LoopGraph is the static graph over a set of automations.
type LoopGraph struct {
	Automations []GraphAutomation
	Edges       []GraphEdge
	Cycles      []GraphCycle
	Problems    []LoopProblem
	Coverage    GraphCoverage

	// idx is the graph by automation index, which the authored scheduler's
	// candidate check walks (problemThrough). Unexported, so it is not part
	// of the graph's printed shape.
	idx *graphIndex
}

// GraphAutomation is one automation of the graph.
type GraphAutomation struct {
	Name, Origin, Trigger, Schedule, Filter string
	Template                                bool
	Stratum                                 int
	Writes, Publishes, Opaque               []string
	BeforeWrite                             *BeforeWriteConfig
	Loop                                    *LoopConfig
	Mode                                    *ModeConfig
	Cycle                                   int // index into Cycles, -1 when none
}

// GraphEdge is one automation's write or publish firing another.
type GraphEdge struct {
	From, To, Concept, Topic string
	Via                      []string
	Decided                  bool
	Reason                   string
}

// GraphCycle is one strongly connected component that holds a cycle.
type GraphCycle struct {
	Members, PermittedBy, Path []string
}

// LoopProblem is one refusal the graph makes.
type LoopProblem struct {
	Automation, Origin, Code, Message string
}

// GraphCoverage counts what the graph could see: the construct and
// sub-automation calls it resolved, those it could not, and the opaque ones
// (builtins, actions, webhooks) it lists without following.
type GraphCoverage struct {
	Automations, Resolved, Unresolved, Opaque int
}

// The refusal codes the graph makes.
const (
	codeLoopCycle         = "loop_cycle"
	codeLoopMaxDepthRange = "loop_max_depth_range"
)

// topicKnownAtRunTime stands in a node's Publishes for an event step whose
// topic is an expression.
const topicKnownAtRunTime = "(expression)"

// graphIndex is the graph by automation index.
type graphIndex struct {
	nodes []*Automation
	succ  [][]int
	edge  map[[2]int]int // (from, to) -> index into Edges
	comp  []int          // strongly connected component of each node
	comps [][]int
}

// BuildLoopGraph builds the static graph over automations, with the call
// graph src reads (a nil src is an empty one, in which every construct call
// is unresolved and the coverage says so). depthCap, when positive, is the
// chain depth cap a @loop's maxDepth must not exceed; the load's own prepare
// step owns that check, and a caller holding the cap passes it so an
// automation built another way is held to it too.
//
// The graph is deterministic: automations sort by (name, origin), and every
// list the graph holds is sorted, so the same input in any order builds the
// same graph -- it is printed in a refusal and drawn in the OS.
func BuildLoopGraph(automations []*Automation, src FunctionSource, depthCap int) *LoopGraph {
	var reg work.Registry
	if src != nil {
		reg = src.Registry()
	}
	b := &graphBuilder{reg: reg, byName: map[string]int{}}
	for _, a := range automations {
		if a != nil && a.IsEnabled() {
			b.nodes = append(b.nodes, a)
		}
	}
	sort.SliceStable(b.nodes, func(i, j int) bool {
		if b.nodes[i].Name != b.nodes[j].Name {
			return b.nodes[i].Name < b.nodes[j].Name
		}
		return b.nodes[i].Origin < b.nodes[j].Origin
	})
	for i, a := range b.nodes {
		if prior, dup := b.byName[a.Name]; !dup || (strings.HasPrefix(b.nodes[prior].Origin, "authored:") && !strings.HasPrefix(a.Origin, "authored:")) {
			b.byName[a.Name] = i
		}
	}
	n := len(b.nodes)
	b.facts = make([]stepFacts, n)
	for i, a := range b.nodes {
		b.facts[i] = collectStepFacts(a)
	}

	g := &LoopGraph{Automations: make([]GraphAutomation, n)}
	g.Coverage.Automations = n
	prods := make([][]production, n)
	for i, a := range b.nodes {
		prods[i] = b.productions(i)
		g.Automations[i] = b.node(i, a, &g.Coverage)
	}

	idx := &graphIndex{nodes: b.nodes, succ: make([][]int, n), edge: map[[2]int]int{}}
	for i := range b.nodes {
		for j, to := range b.nodes {
			if !to.IsEventTriggered() {
				continue
			}
			e, ok := b.edge(b.nodes[i], to, prods[i])
			if !ok {
				continue
			}
			idx.edge[[2]int{i, j}] = len(g.Edges)
			idx.succ[i] = append(idx.succ[i], j)
			g.Edges = append(g.Edges, e)
		}
	}
	g.idx = idx

	idx.comps = tarjan(n, idx.succ)
	idx.comp = make([]int, n)
	for c, members := range idx.comps {
		for _, m := range members {
			idx.comp[m] = c
		}
	}
	for i, s := range strata(idx.comps, idx.comp, idx.succ) {
		g.Automations[i].Stratum = s
	}
	b.cycles(g, idx)

	if depthCap > 0 {
		for _, a := range b.nodes {
			if a.Loop != nil && (a.Loop.MaxDepth < 1 || a.Loop.MaxDepth > depthCap) {
				g.Problems = append(g.Problems, LoopProblem{
					Automation: a.Name, Origin: a.Origin, Code: codeLoopMaxDepthRange,
					Message: fmt.Sprintf("automation %s: @loop(maxDepth=%d) is outside 1 to %d, the chain depth cap (MEMQL_AUTOMATION_MAX_CHAIN_DEPTH) -- write a maxDepth from 1 to %d [%s]",
						a.Name, a.Loop.MaxDepth, depthCap, depthCap, codeLoopMaxDepthRange),
				})
			}
		}
	}
	sort.SliceStable(g.Problems, func(i, j int) bool {
		p, q := g.Problems[i], g.Problems[j]
		if p.Automation != q.Automation {
			return p.Automation < q.Automation
		}
		if p.Origin != q.Origin {
			return p.Origin < q.Origin
		}
		if p.Code != q.Code {
			return p.Code < q.Code
		}
		return p.Message < q.Message
	})
	return g
}

// graphBuilder carries one build.
type graphBuilder struct {
	reg    work.Registry
	nodes  []*Automation
	byName map[string]int // a name's first automation, for sub-automation calls
	facts  []stepFacts
}

// graphCall is one construct call a step makes, with its arguments: a literal
// is plain JSON, an expression an *ExprLeaf (or its compiled `{"$expr": ...}`
// form before the automation is prepared).
type graphCall struct {
	name string
	args map[string]any
}

// graphPublish is one publish statement.
type graphPublish struct {
	topic   string // "" when the topic is an expression
	payload map[string]any
}

// stepFacts is what an automation's own steps do.
type stepFacts struct {
	calls     []graphCall
	subs      []string
	publishes []graphPublish
	opaque    []string // actions
}

// collectStepFacts walks an automation's statements -- the bodies of its
// `for` loops, its parallel branches, and the blocks an if or a switch
// compiles to -- in the order they are written. A construct call is always a
// statement of its own (a function step); an expression or a return is pure.
func collectStepFacts(a *Automation) stepFacts {
	var f stepFacts
	var walk func(steps []*Step)
	visit := func(s *Step) {
		if s == nil {
			return
		}
		switch {
		case s.Function != nil:
			f.calls = append(f.calls, graphCall{name: s.Function.Name, args: s.Function.Args})
		case s.Automation != nil:
			f.subs = append(f.subs, s.Automation.Name)
		case s.Event != nil:
			f.publishes = append(f.publishes, eventPublishOf(s))
		case s.Action != nil:
			f.opaque = append(f.opaque, "action "+s.Action.Ref)
		case s.ForEach != nil:
			walk(s.ForEach.Do)
		case s.Parallel != nil:
			walk(s.Parallel.Branches)
		case s.Block != nil:
			walk(s.Block.Steps)
		}
	}
	walk = func(steps []*Step) {
		for _, s := range steps {
			visit(s)
		}
	}
	walk(a.Steps)
	return f
}

// eventPublishOf reads an event step: its topic when it is a literal, and
// its payload.
func eventPublishOf(s *Step) graphPublish {
	p := graphPublish{payload: s.Event.Payload}
	switch {
	case s.Exprs != nil && s.Exprs.Topic != nil:
		if lit, ok := ast.Unparen(s.Exprs.Topic).(*ast.LiteralExpr); ok {
			if topic, isString := lit.Value.(string); isString {
				p.topic = topic
			}
		}
	case s.Event.leaves["topic"] == nil:
		p.topic = s.Event.Topic
	}
	return p
}

// production is one topic an automation's work publishes, with what the row
// its event carries is known to hold.
type production struct {
	kind    string // insert | update | "" (a write whose kind is unknown) | publish
	concept string // the written concept; "" for a publish
	topic   string // the topic; "" for a publish whose topic is an expression
	via     []string
	row     knownRow
}

const kindPublish = "publish"

// productions is everything automation i publishes: its own steps' work and,
// through each sub-automation it calls (transitively, each entered once),
// that one's -- with the sub-automations prepended to the path.
func (b *graphBuilder) productions(i int) []production {
	if b.nodes[i].BeforeWrite != nil {
		return nil
	}
	out := b.ownProductions(i, nil)
	seen := map[int]bool{i: true}
	var walk func(j int, path []string)
	walk = func(j int, path []string) {
		for _, name := range b.facts[j].subs {
			k, ok := b.byName[name]
			if !ok || seen[k] {
				continue
			}
			seen[k] = true
			via := append(append(make([]string, 0, len(path)+1), path...), name)
			out = append(out, b.ownProductions(k, via)...)
			walk(k, via)
		}
	}
	walk(i, nil)
	for i := range out {
		b.invalidateBeforeWriteFields(&out[i])
	}
	sort.SliceStable(out, func(x, y int) bool {
		p, q := out[x], out[y]
		if p.topic != q.topic {
			return p.topic < q.topic
		}
		if p.concept != q.concept {
			return p.concept < q.concept
		}
		if p.kind != q.kind {
			return p.kind < q.kind
		}
		return strings.Join(p.via, "\x00") < strings.Join(q.via, "\x00")
	})
	return out
}

// ownProductions is what automation i's own steps publish, prefix being the
// sub-automation path it is reached through.
func (b *graphBuilder) ownProductions(i int, prefix []string) []production {
	f := b.facts[i]
	var out []production
	with := func(path []string) []string {
		return append(append(make([]string, 0, len(prefix)+len(path)), prefix...), path...)
	}
	for _, c := range f.calls {
		for _, w := range work.UnionWrites([]string{c.name}, b.reg) {
			out = append(out, writeProductions(w, c.args, with(w.Path))...)
		}
	}
	for _, p := range f.publishes {
		out = append(out, publishProduction(p, prefix))
	}
	return out
}

// payloadFields reads a step's payload map the way a template's is read.
func payloadFields(payload map[string]any) map[string]work.FieldValue {
	if len(payload) == 0 {
		return nil
	}
	out := make(map[string]work.FieldValue, len(payload))
	for k, v := range payload {
		if lit, ok := literalArg(v); ok {
			out[k] = work.FieldValue{Literal: lit, HasLiteral: true}
		} else {
			out[k] = work.FieldValue{}
		}
	}
	return out
}

// writeProductions is the topics one write publishes -- created, and updated
// too for an update or a write of unknown kind -- each with its row.
func writeProductions(w work.Write, args map[string]any, via []string) []production {
	topics := []string{events.BuildTopicWithConcept(events.TopicGraphNodeCreated, w.Concept)}
	if w.Spec.Kind != string(ast.MutationKindInsert) {
		topics = append(topics, events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, w.Concept))
	}
	out := make([]production, 0, len(topics))
	for _, topic := range topics {
		out = append(out, production{
			kind:    w.Spec.Kind,
			concept: w.Concept,
			topic:   topic,
			via:     via,
			row:     writeRow(w, args, strings.HasPrefix(topic, events.TopicGraphNodeUpdated+".")),
		})
	}
	return out
}

// writeRow is what a write is known to put in its row (D-I):
//
//   - a field the template sets to a literal is known;
//   - one set to `args.x` is known when the mutation is called directly and
//     the call passes a literal for x -- one level, from a step to a
//     mutation, never through a logic's parameters -- and not written at all
//     when the call leaves x out;
//   - the concept is the write's;
//   - an update is not a first version, and the fields it does not set keep
//     what is stored, so they are unknown;
//   - an insert of a new row is its first version, and the fields it does
//     not set are absent;
//   - an insert that names an id may land on a row that exists: its first
//     version and the fields it does not set are unknown.
//
// A graph.node.updated event carries no firstVersion of its own, so on that
// topic the answer is left unknown.
func writeRow(w work.Write, args map[string]any, updatedTopic bool) knownRow {
	spec, mut := w.Spec, w.Mutation
	direct := len(w.Path) == 1
	kindKnown := spec.Kind == string(ast.MutationKindInsert) || spec.Kind == string(ast.MutationKindUpdate)
	newRow := spec.Kind == string(ast.MutationKindInsert) && spec.NewRow
	row := knownRow{
		Concept:     w.Concept,
		Fields:      map[string]any{},
		Absent:      map[string]bool{},
		Unknown:     map[string]bool{},
		OthersKnown: newRow,
	}
	keeps := "the row keeps the value already stored"
	why := map[string]string{}
	for _, f := range sortedKeys(spec.Fields) {
		fv := spec.Fields[f]
		switch {
		case fv.HasLiteral:
			row.Fields[f] = fv.Literal
		case fv.Arg != "" && direct:
			v, passed, literal := callSiteArg(args, fv.Arg)
			switch {
			case !passed && newRow:
				row.Absent[f] = true
			case !passed:
				row.Unknown[f] = true
				why[f] = fmt.Sprintf("%s sets %s to args.%s, which the call leaves out, so %s", mut, f, fv.Arg, keeps)
			case literal:
				row.Fields[f] = v
			default:
				row.Unknown[f] = true
				why[f] = fmt.Sprintf("%s sets %s to args.%s, known only at run time", mut, f, fv.Arg)
			}
		case fv.Arg != "":
			row.Unknown[f] = true
			why[f] = fmt.Sprintf("%s sets %s to args.%s, known only at run time", mut, f, fv.Arg)
		default:
			row.Unknown[f] = true
			why[f] = fmt.Sprintf("%s sets %s to a value computed at run time", mut, f)
		}
	}
	switch {
	case updatedTopic || !kindKnown:
		row.FirstVersion = triUnknown
	case spec.Kind == string(ast.MutationKindUpdate):
		row.FirstVersion = triFalse
	case newRow:
		row.FirstVersion = triTrue
	default:
		row.FirstVersion = triUnknown
	}
	row.explain = func(field string) string {
		if s, ok := why[field]; ok {
			return s
		}
		switch {
		case !kindKnown:
			return fmt.Sprintf("%s's write is not known at load", mut)
		case field == "firstVersion" && updatedTopic:
			return "a graph.node.updated event carries no firstVersion of its own"
		case field == "firstVersion":
			return fmt.Sprintf("%s may write a row that already exists, so whether this is its first version is known only at run time", mut)
		case spec.Kind == string(ast.MutationKindUpdate):
			return fmt.Sprintf("%s updates the row without setting %s, so %s", mut, field, keeps)
		}
		return fmt.Sprintf("%s does not set %s where the load can read it", mut, field)
	}
	return row
}

// callSiteArg reads one argument a call passes: whether it is passed at all,
// and its value when that is a literal.
func callSiteArg(args map[string]any, name string) (value any, passed, literal bool) {
	v, ok := args[name]
	if !ok {
		return nil, false, false
	}
	lit, isLit := literalArg(v)
	return lit, true, isLit
}

// literalArg is a step value's literal: plain JSON, or a container holding
// only literals. An expression leaf -- parsed or compiled -- is not one.
func literalArg(v any) (any, bool) {
	switch x := v.(type) {
	case *ExprLeaf:
		if x == nil {
			return nil, true
		}
		switch n := ast.Unparen(x.Node).(type) {
		case *ast.LiteralExpr:
			return normalizeLoopValue(n.Value), true
		case *ast.NilExpr:
			return nil, true
		}
		return nil, false
	case map[string]any:
		if src, ok := exprLeafSource(x); ok {
			node, err := languageParser.ParseV1Expression(src)
			if err != nil {
				return nil, false
			}
			return literalArg(&ExprLeaf{Src: src, Node: node})
		}
		out := make(map[string]any, len(x))
		for k, el := range x {
			lit, ok := literalArg(el)
			if !ok {
				return nil, false
			}
			out[k] = lit
		}
		return out, true
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			lit, ok := literalArg(el)
			if !ok {
				return nil, false
			}
			out[i] = lit
		}
		return out, true
	}
	return normalizeLoopValue(v), true
}

// publishProduction is the topic an event step publishes, with its payload
// read as the row a trigger filter on that topic binds: a non-graph event's
// row is its payload, with no columns but an empty concept.
func publishProduction(p graphPublish, via []string) production {
	row := knownRow{Fields: map[string]any{}, Unknown: map[string]bool{}, OthersKnown: true, FirstVersion: triUnknown}
	why := map[string]string{}
	for _, k := range sortedKeys(p.payload) {
		if lit, ok := literalArg(p.payload[k]); ok {
			row.Fields[k] = lit
			continue
		}
		row.Unknown[k] = true
		why[k] = fmt.Sprintf("the event's %s is computed at run time", k)
	}
	if fv, ok := row.Fields["firstVersion"].(bool); ok {
		row.FirstVersion = triOf(fv)
	}
	row.explain = func(field string) string {
		if s, ok := why[field]; ok {
			return s
		}
		return fmt.Sprintf("the event's %s is known only at run time", field)
	}
	return production{kind: kindPublish, topic: p.topic, via: via, row: row}
}

// node is one automation as the graph reports it, and its calls counted.
func (b *graphBuilder) node(i int, a *Automation, cov *GraphCoverage) GraphAutomation {
	n := GraphAutomation{Name: a.Name, Origin: a.Origin, Schedule: a.Schedule, Template: a.Template, Loop: a.Loop, Mode: a.Mode, BeforeWrite: a.BeforeWrite, Cycle: -1}
	if a.Trigger != nil {
		n.Trigger = a.Trigger.Event
		n.Filter = a.Trigger.Filter
	}
	if a.BeforeWrite != nil {
		n.Trigger = "before." + a.BeforeWrite.On + "." + a.BeforeWrite.Concept
		n.Filter = a.BeforeWrite.Filter
	}
	var writes, publishes, opaque, callNames []string
	seen := map[int]bool{}
	var gather func(j int, own bool)
	gather = func(j int, own bool) {
		seen[j] = true
		f := b.facts[j]
		for _, c := range f.calls {
			callNames = append(callNames, c.name)
			opaque = append(opaque, b.reachedBuiltins(c.name)...)
			if own {
				t, ok := b.reg[c.name]
				switch {
				case !ok:
					cov.Unresolved++
				case t.ConstructKind == work.ConstructBuiltin:
					cov.Opaque++
				default:
					cov.Resolved++
				}
			}
		}
		for _, p := range f.publishes {
			if p.topic == "" {
				publishes = append(publishes, topicKnownAtRunTime)
			} else {
				publishes = append(publishes, p.topic)
			}
		}
		opaque = append(opaque, f.opaque...)
		if own {
			cov.Opaque += len(f.opaque)
		}
		for _, name := range f.subs {
			k, ok := b.byName[name]
			if own {
				if ok {
					cov.Resolved++
				} else {
					cov.Unresolved++
				}
			}
			if ok && !seen[k] {
				gather(k, false)
			}
		}
	}
	gather(i, true)
	writes = append(writes, work.UnionFootprint(callNames, b.reg).Concepts...)
	n.Writes = sortedUniqueStrings(writes)
	n.Publishes = sortedUniqueStrings(publishes)
	n.Opaque = sortedUniqueStrings(opaque)
	return n
}

// reachedBuiltins is every builtin a call reaches, itself included, as
// "builtin <name>".
func (b *graphBuilder) reachedBuiltins(name string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		t, ok := b.reg[n]
		if !ok {
			return
		}
		if t.ConstructKind == work.ConstructBuiltin {
			out = append(out, "builtin "+n)
		}
		for _, c := range t.Calls {
			walk(c)
		}
	}
	walk(name)
	return out
}

// edge decides whether from's productions fire to, and picks the one the
// edge reports: a decided one when there is one, else the first undecided,
// in the productions' sorted order.
func (b *graphBuilder) edge(from, to *Automation, prods []production) (GraphEdge, bool) {
	pattern := to.Trigger.Event
	filter, filterErr := triggerFilterLambda(to)
	args := declaredArgs(to)
	best, bestDecided := -1, false
	var bestTri tri
	var bestWhy string
	for k, p := range prods {
		var match bool
		if p.topic != "" {
			match = events.Match(pattern, p.topic)
		} else {
			match = p.kind == kindPublish && !strings.HasPrefix(pattern, "graph.")
		}
		if !match {
			continue
		}
		t, why := triTrue, ""
		if filterErr != "" {
			t, why = triUnknown, filterErr
		} else if filter != nil {
			t, why = decideFilter(filter, p.row, args)
		}
		if t == triFalse {
			continue
		}
		decided := t == triTrue && p.topic != ""
		if best == -1 || (decided && !bestDecided) {
			best, bestDecided, bestTri, bestWhy = k, decided, t, why
		}
		if bestDecided {
			break
		}
	}
	if best == -1 {
		return GraphEdge{}, false
	}
	p := prods[best]
	return GraphEdge{
		From:    from.Name,
		To:      to.Name,
		Concept: p.concept,
		Topic:   p.topic,
		Via:     p.via,
		Decided: bestDecided,
		Reason:  edgeReason(from, to, p, bestTri, bestWhy),
	}, true
}

// declaredArgs is the field set an automation's args block declares, which
// is all a trigger filter's `args` binds (bindEventArgs); nil for no block.
func declaredArgs(a *Automation) map[string]bool {
	if a.Args == nil || len(a.Args.Fields) == 0 {
		return nil
	}
	out := make(map[string]bool, len(a.Args.Fields))
	for _, f := range a.Args.Fields {
		if f != nil && strings.TrimSpace(f.Name) != "" {
			out[f.Name] = true
		}
	}
	return out
}

// triggerFilterLambda is an automation's @filter as its lambda: the one
// prepared at load, else parsed here for an automation built in Go. A filter
// that does not parse is reported, and the edge stays undecided.
func triggerFilterLambda(a *Automation) (*ast.LambdaExpr, string) {
	if a.Trigger == nil || strings.TrimSpace(a.Trigger.Filter) == "" {
		return nil, ""
	}
	if a.Trigger.FilterLambda != nil {
		return a.Trigger.FilterLambda, ""
	}
	lam, err := languageParser.ParseV1Lambda(a.Trigger.Filter)
	if err != nil {
		return nil, fmt.Sprintf("the filter does not parse: %v", err)
	}
	return lam, ""
}

// sortedUniqueStrings sorts in and drops duplicates and empty strings.
func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// cycles records every strongly connected component that holds a cycle:
// permitted when the component minus its @loop automations holds none,
// refused as loop_cycle otherwise.
func (b *graphBuilder) cycles(g *LoopGraph, idx *graphIndex) {
	isLoop := func(i int) bool { return b.nodes[i].Loop != nil }
	var cyclic [][]int
	for _, members := range idx.comps {
		if len(members) > 1 || hasSelfEdge(idx.succ, members[0]) {
			cyclic = append(cyclic, members)
		}
	}
	sort.Slice(cyclic, func(i, j int) bool { return cyclic[i][0] < cyclic[j][0] })
	for c, members := range cyclic {
		cycle := GraphCycle{Members: b.names(members)}
		for _, m := range members {
			g.Automations[m].Cycle = c
		}
		if covered(members, idx.succ, isLoop) {
			for _, m := range members {
				if isLoop(m) {
					cycle.PermittedBy = append(cycle.PermittedBy, b.nodes[m].Name)
				}
			}
			sort.Strings(cycle.PermittedBy)
			cycle.Path = b.names(cyclePath(members, idx.succ, func(int) bool { return false }))
		} else {
			path := cyclePath(members, idx.succ, isLoop)
			cycle.Path = b.names(path)
			if len(path) > 0 {
				start := b.nodes[path[0]]
				g.Problems = append(g.Problems, LoopProblem{
					Automation: start.Name, Origin: start.Origin, Code: codeLoopCycle,
					Message: renderCycleProblem(g, path),
				})
			}
		}
		g.Cycles = append(g.Cycles, cycle)
	}
}

func (b *graphBuilder) names(idx []int) []string {
	if len(idx) == 0 {
		return nil
	}
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = b.nodes[j].Name
	}
	return out
}
