package memql

// belongs_to.go -- the half of `@belongsTo` that needs EVERY concept at once
// (design record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md).
//
// A declaration is checked in three places, each the first one that can answer
// its question:
//
//   - which combinations of keys mean something is the parser's
//     (component/language/parser/belongsto_binding.go), read off one annotation;
//   - whether the fields a declaration names exist is the concept translator's
//     (memory-nodes' validateBelongsTo), read off one concept;
//   - whether a `via` chain is SOUND is this file's, read off the whole
//     registry: the concept a `via` field points at has to exist, has to be a
//     concept whose rows can have an account, and the chain has to end.
//
// It also holds the two things built on the same walk: the order the
// engine-managed concepts are assigned in (parents first), and the report of
// concepts that declared nothing at all.
//
// WHY THE CHAIN IS A LOAD-TIME QUESTION AND NOT A WRITE-TIME ONE. None of these
// fails a write. A row whose `via` leads to a cluster-wide concept, or to a
// `parent=` that names no concept, finds nothing to follow, falls through to
// the next source and is stamped with the account of whoever made the call.
// Nothing errors. The row is filed under the wrong account, and the first
// person to notice is whoever exports the right one and finds it missing. A
// loop is quieter still: every concept in it says its rows take their account
// from another's, so none of them is where an account starts, and which rows
// end up with one depends on the order they happened to be written in. So the
// tree is refused at load, by name, where the author is still looking at it.
//
// NOTHING HERE WRITES. The stamp that fills `accountId` and the job that
// assigns existing rows live elsewhere and read these functions; this file only
// answers questions about declarations.

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/metrics"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// belongsToDeclFor returns the `@belongsTo` declaration of the named concept
// from the live registry, or nil when the concept is unnamed, unregistered or
// declared nothing.
//
// Nil is one answer for three situations on purpose: none of them has said
// which account a row belongs to, and every caller on the write path reads nil
// as "nothing to stamp". The sibling of rowAuthzDeclFor, for the same reason
// that one exists -- the write path holds a concept NAME, and a second way of
// turning a name into a declaration would be a second answer about it.
func belongsToDeclFor(conceptName string) *langparser.BelongsToDecl {
	name := strings.TrimSpace(conceptName)
	if name == "" {
		return nil
	}
	c, err := memorynodes.Get(name)
	if err != nil || c == nil {
		return nil
	}
	return c.BelongsTo
}

// TriggerAccountTrusted reports whether the account a row of this concept
// records may place the work that row triggers: an automation run started by
// the row acts in the row's account (component/automations'
// contextWithTriggerAccount), and every row the run then writes is recorded
// under it.
//
// TWO KINDS OF CONCEPT ANSWER YES, each because something other than the row's
// writer stands behind the value:
//
//   - an ENGINE-MANAGED concept, whose `accountId` the engine fills in itself
//     and holds to the accounts a person may write to (account_stamp.go);
//   - an ORGANIZATION-OWNED concept, `stated`, whose account
//     validateOrganizationOwnership holds to an active account the writer may
//     work in on every write.
//
// EVERY OTHER `stated` CONCEPT ANSWERS NO. Its own mutations name the account
// and nothing in the engine checks the value against the writer, so a run
// placed by it would act wherever a row's writer said. Such a run states no
// account: it keeps its caller's, and the rows it writes follow their parents
// or are assigned later. That is a narrower answer than the truth for a concept
// whose writers are all server code, and deliberately so: widening it is a
// decision about that concept's writers, made where they are.
func TriggerAccountTrusted(c *memorynodes.Concept) bool {
	if c == nil || !c.BelongsTo.AccountScoped() {
		return false
	}
	return c.BelongsTo.EngineManaged() || organizationOwnedConcept(strings.TrimSpace(c.Name))
}

// accountParentConcept names the concept whose rows a `via` concept's rows
// follow, or "" when there is none to name.
//
// In order:
//
//   - the declared `parent=`, when the author wrote one. It is what THIS
//     declaration says the field holds, so it wins over anything else the field
//     carries;
//   - else the target of the `via` field's own @relationship -- the usual case,
//     and the reason `parent=` is rarely written;
//   - else "": the field carries no relationship and names no parent, which is
//     an index row's reference (a Library artifact's `sourceConceptRef`). Such
//     a field holds canonical ids of several concepts, so no single concept is
//     the parent and the id itself says which one at write time.
//
// ONLY AN OUTGOING RELATIONSHIP COUNTS. `via` is a field of THIS row. An
// incoming relationship's `field` is a field of the FAR concept (memql#3654),
// so one that happens to spell the same name says nothing about what this
// row's field points at.
//
// A field carrying relationships to MORE THAN ONE concept answers "" as well,
// for the reason the unrelated field does: it holds ids of several concepts.
// Picking the first would make the answer depend on the order two annotations
// were written in.
//
// "" is also the answer for a concept that follows nothing -- undeclared,
// cluster-wide, or account-scoped without `via` -- so a caller that has to tell
// "follows nothing" from "follows something the id decides" reads the
// declaration's Via beside this.
func accountParentConcept(c *memorynodes.Concept) string {
	if c == nil || !c.BelongsTo.AccountScoped() {
		return ""
	}
	via := strings.TrimSpace(c.BelongsTo.Via)
	if via == "" {
		return ""
	}
	if parent := strings.TrimSpace(c.BelongsTo.Parent); parent != "" {
		return parent
	}
	target := ""
	for _, rel := range c.Relationships {
		if strings.TrimSpace(rel.Field) != via {
			continue
		}
		// Compared loosely because this runs over concepts the loader built
		// and Init has not normalized yet as well as over ones it has.
		if strings.EqualFold(strings.TrimSpace(rel.Direction), relationshipDirectionIncoming) {
			continue
		}
		candidate := strings.TrimSpace(rel.TargetConcept)
		if candidate == "" {
			continue
		}
		if target != "" && target != candidate {
			return ""
		}
		target = candidate
	}
	return target
}

// belongsToViaGraph is the one walk the cross-concept checks and the assignment
// order share: every registered concept by name, and for each concept that
// follows a parent, the concept its `via` resolves to.
//
// One structure for both because they must not disagree about what a concept
// follows. A check that refused a cycle the order did not see -- or an order
// that listed a chain the check had refused -- would be two answers to one
// question, and the second would be found by the assignment job at run time
// rather than by the load.
type belongsToViaGraph struct {
	byName map[string]*memorynodes.Concept
	// names is every registered concept name, sorted, so that every walk and
	// every report comes out in the same order whatever order a registry
	// (a map underneath) happened to list them in.
	names []string
	// follows maps a concept that declares `via` to the concept it resolves
	// to. A concept that declares `via` and resolves to nothing -- the id
	// decides at write time -- is present with "". A concept that follows
	// nothing is absent.
	follows map[string]string
}

func newBelongsToViaGraph(all []*memorynodes.Concept) *belongsToViaGraph {
	g := &belongsToViaGraph{
		byName:  make(map[string]*memorynodes.Concept, len(all)),
		follows: map[string]string{},
	}
	for _, c := range all {
		if c == nil {
			continue
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		if _, seen := g.byName[name]; seen {
			continue // a duplicate name is Init's own refusal; the first stands here
		}
		g.byName[name] = c
		g.names = append(g.names, name)
		if c.BelongsTo.AccountScoped() && strings.TrimSpace(c.BelongsTo.Via) != "" {
			g.follows[name] = accountParentConcept(c)
		}
	}
	sort.Strings(g.names)
	return g
}

// cycles returns every loop the `via` declarations form, each as its member
// concepts in the direction a row follows them, starting at the member whose
// name sorts first. The cycles themselves are sorted by that member.
//
// A concept follows AT MOST ONE concept, so the graph is a set of chains and
// every loop is a simple one: walking up from any concept either ends, joins a
// chain already walked, or closes back on the walk itself. Each loop is
// therefore found exactly once, by the first walk that enters it, and a concept
// that only LEADS INTO a loop is not part of it -- which is why the result is
// the slice from the point the walk re-entered, not the whole walk.
//
// A concept that follows ITSELF is a loop of one, and deliberately so. The
// declaration is about concepts: a self-following concept names no OTHER
// concept as the place its rows' account comes from, and the assignment of
// existing rows orders CONCEPTS (engineManagedOrder), so it could not put a
// parent row before its child inside one.
func (g *belongsToViaGraph) cycles() [][]string {
	const walked = 1
	state := make(map[string]int, len(g.follows))
	var out [][]string
	for _, start := range g.names {
		if state[start] == walked {
			continue
		}
		var path []string
		at := map[string]int{}
		for current := start; ; {
			if state[current] == walked {
				break // joins a chain an earlier walk already judged
			}
			if i, onPath := at[current]; onPath {
				out = append(out, belongsToRotateToFirst(path[i:]))
				break
			}
			at[current] = len(path)
			path = append(path, current)
			next, followsOne := g.follows[current]
			if !followsOne || next == "" {
				break // follows nothing, or follows whatever the id names
			}
			if _, registered := g.byName[next]; !registered {
				break // an unknown parent is its own refusal, not a loop
			}
			current = next
		}
		for _, name := range path {
			state[name] = walked
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// belongsToRotateToFirst returns the loop starting at the member whose name
// sorts first, keeping its direction. A loop has no first member of its own, so
// without this the same loop would be reported as a different chain depending
// on which concept the walk happened to enter it from.
func belongsToRotateToFirst(loop []string) []string {
	first := 0
	for i, name := range loop {
		if name < loop[first] {
			first = i
		}
	}
	out := make([]string, 0, len(loop))
	out = append(out, loop[first:]...)
	return append(out, loop[:first]...)
}

// belongsToProblem is one cross-concept refusal and the concept it is filed
// under on the load report: the declaring concept, or for a loop the member
// whose name sorts first.
type belongsToProblem struct {
	concept string
	err     *langparser.BelongsToError
}

// belongsToProblems is every cross-concept refusal in the registry, sorted by
// the concept each is filed under and then by code, so one broken declaration
// names the same concept on every boot.
//
// FOUR REFUSALS:
//
//   - belongs_to_parent_unknown: `parent=` names a concept the registry does
//     not hold. A `via` field's relationship is resolved (and a missing target
//     refused) by the relationship derivation before this runs, so the written
//     `parent=` is the one name here nothing else has checked.
//
//   - belongs_to_via_cluster: the concept a `via` resolves to DECLARED
//     `@belongsTo(cluster)`. Its rows are inside no account, by its author's
//     own statement, so a row that follows one has nothing to take.
//
//   - belongs_to_via_cycle: the declarations form a loop (see cycles).
//
//   - belongs_to_via_stays: a kind that TRAVELS follows a kind that declares
//     `stays`. An account export would carry the child and leave behind the
//     row its account comes from, so the file would hold rows whose account
//     nothing in it explains. Filed under the follower: it is the declaration
//     to change (declare the same `stays`, or follow something else). The
//     other direction is sound -- a kind that stays may follow any account
//     kind -- and a chain needs only the direct parent checked, because every
//     link of it is checked as a child in its own turn.
//
// AN UNDECLARED PARENT IS ACCEPTED, and that is a decision rather than a gap.
// Its rows count as cluster-wide until it declares, so a child that follows it
// gets no account from it today -- the same outcome as the refused case. But
// "nobody has classified this concept yet" is not "its author said it is never
// inside an account": the first is a declaration still to be written, usually
// in another repository, and refusing the child for it would make a product
// bundle unloadable the moment a concept it follows fell behind. The child
// loads, and once the parent declares an account form the rows written from
// then on follow it.
//
// A STATED or SHARED parent is accepted for the plain reason that its rows can
// have an account.
func belongsToProblems(all []*memorynodes.Concept) []belongsToProblem {
	g := newBelongsToViaGraph(all)
	var out []belongsToProblem
	for _, name := range g.names {
		resolved, followsOne := g.follows[name]
		if !followsOne {
			continue
		}
		c := g.byName[name]
		written := langparser.FormatBelongsTo(*c.BelongsTo)
		via := strings.TrimSpace(c.BelongsTo.Via)

		if declared := strings.TrimSpace(c.BelongsTo.Parent); declared != "" {
			if _, registered := g.byName[declared]; !registered {
				out = append(out, belongsToProblem{concept: name, err: langparser.NewBelongsToError(
					langparser.CodeBelongsToParentUnknown,
					"concept %q declares %s, and %q is not a registered concept. parent= is the canonical id "+
						"of the concept whose rows the %q field names -- v1:<namespace>:<name> -- written out "+
						"because the field carries no @relationship to say so.%s",
					name, written, declared, via, suggestBelongsToParent(declared, g.names))})
				continue
			}
		}

		parent := g.byName[resolved]
		if parent != nil && c.Travels() && parent.Stays() != "" {
			out = append(out, belongsToProblem{concept: name, err: langparser.NewBelongsToError(
				langparser.CodeBelongsToViaStays,
				"concept %q declares %s, and its %q field names a row of %q, which declares %s. "+
					"An account export carries this concept's rows and leaves that one's behind, so the file "+
					"would hold rows whose account nothing in it explains. Declare the same stays=%q here, or "+
					"follow a concept whose rows travel",
				name, written, via, parent.Name, langparser.FormatBelongsTo(*parent.BelongsTo), parent.Stays())})
			continue
		}
		if parent == nil || !parent.ClusterWide() {
			continue
		}
		out = append(out, belongsToProblem{concept: name, err: langparser.NewBelongsToError(
			langparser.CodeBelongsToViaCluster,
			"concept %q declares %s, and its %q field names a row of %q, which declares @%s(cluster). "+
				"A cluster-wide row is inside no account, so a row that follows one has no account to take "+
				"and would be filed under whichever account the call that wrote it was acting in. Follow a "+
				"concept whose rows belong to an account, or declare @%s(account) and let each row take the "+
				"account of the call that writes it",
			name, written, via, parent.Name, langparser.BelongsToAnnotation, langparser.BelongsToAnnotation)})
	}
	for _, loop := range g.cycles() {
		out = append(out, belongsToProblem{concept: loop[0], err: belongsToCycleError(loop)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].concept != out[j].concept {
			return out[i].concept < out[j].concept
		}
		return out[i].err.Code < out[j].err.Code
	})
	return out
}

// belongsToCycleError is the refusal of one loop, walking it once round so the
// author sees which declarations close it.
func belongsToCycleError(loop []string) *langparser.BelongsToError {
	walk := strings.Join(append(append([]string(nil), loop...), loop[0]), " -> ")
	return langparser.NewBelongsToError(langparser.CodeBelongsToViaCycle,
		"the @%s via declarations form a cycle: %s. Each concept in the loop says its rows take their "+
			"account from another's, so none of them is where an account starts, and rows that already "+
			"exist could not be assigned parents first. One concept in the loop has to take its account "+
			"directly: declare @%s(account) on it",
		langparser.BelongsToAnnotation, walk, langparser.BelongsToAnnotation)
}

// suggestBelongsToParent returns a trailing hint naming the concept an
// unresolvable `parent=` was probably reaching for, or "".
//
// A BARE NAME gets its own answer, because it is the mistake an author who
// knows @relationship makes: `target=store` there is a bare name resolved
// through an import, and `parent=` here is a canonical id. Telling them the
// name is "not registered" would leave them comparing two spellings that look
// alike; naming the id they meant does the work reading the engine would.
func suggestBelongsToParent(declared string, registered []string) string {
	var near []string
	if !strings.Contains(declared, ":") {
		for _, name := range registered {
			if conceptShortName(name) == declared {
				near = append(near, name)
			}
		}
	} else {
		// The threshold scales with the id's length, as it does for an
		// unresolvable @relationship target, so a short id does not match
		// half the registry.
		limit := 3
		if n := len(declared) / 5; n < limit {
			limit = n
		}
		if limit < 1 {
			limit = 1
		}
		for _, name := range registered {
			if levenshtein(declared, name) <= limit {
				near = append(near, name)
			}
		}
	}
	if len(near) == 0 {
		return ""
	}
	if len(near) > 3 {
		near = near[:3]
	}
	return fmt.Sprintf(" Did you mean %s?", strings.Join(quoteAll(near), " or "))
}

// validateBelongsToAcrossConcepts returns every cross-concept `@belongsTo`
// refusal in the registry, each a *langparser.BelongsToError carrying its
// stable code. Empty means every `via` chain is sound.
//
// It is the load-time check: Init turns each into a skip on the load report,
// which strict boot refuses, so a product bundle mounted at MEMQL_DSL_PATH is
// held to it exactly as the engine's own tree is.
func validateBelongsToAcrossConcepts(all []*memorynodes.Concept) []error {
	problems := belongsToProblems(all)
	if len(problems) == 0 {
		return nil
	}
	out := make([]error, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.err)
	}
	return out
}

// engineManagedOrder returns the name of every concept whose account the ENGINE
// decides -- account-scoped and not `stated` -- in the order existing rows are
// assigned: a concept after the concept its rows follow.
//
// ONLY ENGINE-MANAGED CONCEPTS ARE LISTED. A `stated` concept's own writers
// name its account and the assignment leaves its rows alone, so it has no place
// in an assignment order; cluster-wide and undeclared concepts have no account
// at all.
//
// THE ORDER, and the key it is sorted by -- (after-everything, depth, name):
//
//   - depth: a concept that follows nothing is at depth 0, and a concept that
//     follows an engine-managed concept is one deeper than it. So a goal comes
//     before its runs and a run before its steps, which is what lets each row
//     take an account its parent already has.
//
//     A concept that follows a concept which is NOT engine-managed -- stated,
//     undeclared, or not registered -- is at depth 0 too. Its parent's rows are
//     not assigned by this order, so nothing has to come first; the child takes
//     whatever the parent row carries.
//
//   - after-everything: a concept whose `via` resolves to no concept (the id
//     decides at write time) can be following a row of ANY concept, so it comes
//     after every concept it could be following -- and a concept that follows
//     one of THOSE comes after it, which is why the flag is inherited down a
//     chain and sorts before depth rather than being a large depth.
//
//   - name: concepts the two rules above do not separate are in name order, so
//     the order is the same on every node and every run.
//
// A cycle has no such order, and is returned as the refusal the load makes of
// it. A loaded tree has none -- Init refuses it -- so the error is for a caller
// handed a registry the load never judged.
func engineManagedOrder(all []*memorynodes.Concept) ([]string, error) {
	g := newBelongsToViaGraph(all)
	if loops := g.cycles(); len(loops) > 0 {
		return nil, belongsToCycleError(loops[0])
	}

	type place struct {
		depth           int
		afterEverything bool
	}
	places := map[string]place{}
	var placeOf func(name string) place
	placeOf = func(name string) place {
		if p, done := places[name]; done {
			return p
		}
		var p place
		if resolved, followsOne := g.follows[name]; followsOne {
			switch parent := g.byName[resolved]; {
			case resolved == "":
				p.afterEverything = true
			case parent.AccountEngineManaged():
				// Terminates: cycles() found no loop, so every chain ends.
				above := placeOf(resolved)
				p = place{depth: above.depth + 1, afterEverything: above.afterEverything}
			}
		}
		places[name] = p
		return p
	}

	var order []string
	for _, name := range g.names {
		if g.byName[name].AccountEngineManaged() {
			placeOf(name)
			order = append(order, name)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := places[order[i]], places[order[j]]
		if a.afterEverything != b.afterEverything {
			return !a.afterEverything
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return order[i] < order[j]
	})
	return order, nil
}

// ---------------------------------------------------------------------
// Concepts that declared nothing

// belongsToForms is the list of declarations an author is shown wherever a
// concept is reported for having none. One copy, so the load's log line and the
// linter's warning cannot drift into teaching two lists.
const belongsToForms = "@belongsTo(account) beside an `accountId string` field when each row belongs to one account; " +
	"@belongsTo(account, via=\"<field>\") when a row takes the account of the row that field names; " +
	"@belongsTo(account, shared) when the concept also holds rows the cluster provides to every account; " +
	"@belongsTo(account, stated) when the concept's own mutations name the account; " +
	"@belongsTo(cluster) when its rows are never inside an account"

// belongsToTreeDomain returns the top-level DSL domain a concept id was
// assembled under: the first path segment of its namespace, so both
// `v1:acme:widget` and `v1:acme/sub:widget` answer "acme".
//
// It reads the ID, not the file the concept was declared in, because the id is
// what Init holds -- a registered Concept does not carry its origin. The two
// agree except where a domain directory pins a namespace that is not its own
// name (namespace.pin): the engine's own pins name core domains
// (dsl/deployment pins "cluster"), so a core concept reads as core either way,
// but a PRODUCT directory that pinned a core domain's name would have its
// concepts read as core here and left out of the report. The engine does not
// police that pin anywhere, and the linter -- which scans files, not ids --
// still reports those concepts.
func belongsToTreeDomain(conceptName string) string {
	namespace := conceptRootDomain(strings.TrimSpace(conceptName))
	if slash := strings.IndexByte(namespace, '/'); slash >= 0 {
		namespace = namespace[:slash]
	}
	return namespace
}

// undeclaredBelongsTo returns the names of the concepts that declare no
// `@belongsTo` and are not the engine's own, sorted.
//
// THE ENGINE'S OWN TREE IS LEFT OUT, by domain (dsl.IsCoreDomain). A build gate
// holds the embedded tree to "every concept declares", so a core concept with
// no declaration is a failed build rather than something to tell an operator
// about on every boot of every node -- and until that gate's tree is fully
// declared, naming some 270 core concepts here would bury the handful that a
// bundle's author can actually act on.
//
// What is left is everything mounted beside the embedded tree: a product
// bundle at MEMQL_DSL_PATH, and a pack registered into the binary.
func undeclaredBelongsTo(all []*memorynodes.Concept) []string {
	var out []string
	for _, c := range all {
		if c == nil || c.BelongsTo != nil {
			continue
		}
		name := strings.TrimSpace(c.Name)
		if name == "" || memqldsl.IsCoreDomain(belongsToTreeDomain(name)) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// warnUndeclaredBelongsTo reports the mounted concepts that declare no
// `@belongsTo`: ONE warning naming all of them and what to write, and -- when
// this load is the node's own -- the memql_dsl_belongs_to_undeclared gauge set
// to their count. It returns the names it reported.
//
// A WARNING AND A GAUGE, AND NOTHING ON THE LOAD REPORT. An undeclared concept
// still loads, and its rows are treated as they always were, so this is not a
// refusal. It is also deliberately not a LoadReport warning, which is the
// channel a deprecated language form uses: that channel is read by the
// conformance corpus, where a case that draws a warning is no longer a case
// that "loads clean", and by the linter as a per-use diagnostic. Some hundreds
// of corpus cases declare a concept with no `@belongsTo` because they are about
// something else, and each would stop being the clean load it is pinned as.
// The linter reports undeclared concepts through its own scan
// (ConceptsDeclaringNoBelongsTo), per file and line, where an author reads it.
//
// ONE LINE, for the reason warnUndeclaredRowAuthz gives: the list is the
// actionable part, and a line per concept at every boot of every node is how a
// signal gets filtered out of a log and stops being read.
//
// THE GAUGE IS SET ONLY BY A PUBLISHING LOAD. A candidate validation -- a
// package deploy's analysis, the linter's parity pass -- loads a tree that is
// not the node's, and the series says what THIS node mounted. Zero is set like
// any other count, so a bundle that has since declared takes the gauge back
// down. logger may be nil.
func warnUndeclaredBelongsTo(logger *slog.Logger, all []*memorynodes.Concept, publish bool) []string {
	undeclared := undeclaredBelongsTo(all)
	if publish {
		metrics.BelongsToUndeclared(len(undeclared))
	}
	if len(undeclared) == 0 || logger == nil {
		return undeclared
	}
	logger.Warn("DSL concepts declare no @belongsTo: their rows count as cluster-wide until they do",
		"component", "memql.belongsTo",
		"count", len(undeclared),
		"concepts", strings.Join(undeclared, ", "),
		"hint", "nothing records which account a row of these concepts belongs to, so an account's inventory "+
			"and export leave them out. Declare one of: "+belongsToForms+
			". `memqllint` names each undeclared concept by file and line.")
	return undeclared
}

// UndeclaredBelongsToConcept is one concept declaration that carries no
// `@belongsTo`: the name its header declares and the line of its `concept`
// keyword, 1-based -- the line an author goes to in order to add the
// annotation, rather than the first line of its preamble.
type UndeclaredBelongsToConcept struct {
	Name string
	Line int
}

// Warning is what an author is told about the concept: what the missing
// declaration costs, and every form there is to write. The linter prints it
// after the file and line.
func (u UndeclaredBelongsToConcept) Warning() string {
	return fmt.Sprintf("concept %q declares no @belongsTo, so nothing records which account its rows belong to "+
		"and they count as cluster-wide. Write one above it: %s", u.Name, belongsToForms)
}

// ConceptsDeclaringNoBelongsTo scans one .memql source and returns every
// concept it declares that carries no `@belongsTo`, in the order they are
// written.
//
// It is the linter's view of the rule the load reports in its log
// (warnUndeclaredBelongsTo), and it reads SOURCE rather than the registry for
// the reason the linter's scan of deprecated forms does: a file can be linted
// on its own, in a directory no mount would read as a domain, where no engine
// ever builds its concepts. Reading the source also gives the line, which the
// registry does not keep.
//
// Unlike the load's report it does not leave the engine's own domains out. A
// linter is pointed at files, and "these concepts say nothing about their
// account" is as true of the engine's tree as of a bundle's.
//
// A concept that does not PARSE is not reported here. The load refuses it by
// name (concept_unparsed), and a second finding about the same concept -- one
// that could only say "we could not see a declaration" -- would send its
// author to fix the wrong thing first.
func ConceptsDeclaringNoBelongsTo(source string) []UndeclaredBelongsToConcept {
	slices, _ := conceptSlicesReporting(source)
	var out []UndeclaredBelongsToConcept
	for _, slice := range slices {
		file, err := langparser.ParseFile(slice.Source)
		if err != nil {
			continue
		}
		for _, def := range file.Definitions {
			decl, ok := def.(*languageAst.ConceptDecl)
			if !ok || conceptDeclaresBelongsTo(decl) {
				continue
			}
			out = append(out, UndeclaredBelongsToConcept{Name: decl.Name, Line: conceptKeywordLine(source, slice)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// conceptDeclaresBelongsTo reports whether a concept's preamble carries a
// `@belongsTo`, well-formed or not. A malformed one is a declaration its author
// made and the load refuses with its own code; reporting the concept as having
// none beside that refusal would contradict it.
func conceptDeclaresBelongsTo(decl *languageAst.ConceptDecl) bool {
	for _, attr := range decl.Attributes {
		if attr != nil && attr.Name == langparser.BelongsToAnnotation {
			return true
		}
	}
	return false
}

// conceptKeywordLine is the 1-based line of a concept slice's `concept`
// keyword in its file. A slice starts at its preamble, so the keyword is found
// on the comment-blanked view -- a `concept` inside a comment above the
// declaration is not it -- exactly as conceptParseError places a parse failure.
func conceptKeywordLine(source string, slice langparser.DeclarationSlice) int {
	line := 1 + strings.Count(source[:slice.Start], "\n")
	if loc := conceptHeaderRe.FindStringIndex(langparser.BlankComments(slice.Source)); loc != nil {
		line += strings.Count(slice.Source[:loc[0]], "\n")
	}
	return line
}
