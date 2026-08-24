package memql

// THE CONNECTOR-NAME BOOT CHECK (epic memql#4378, D2).
//
// A concept's @origin("<connector>") says "somebody else owns this data
// and a connector by this name keeps our copy faithful". A
// @mirroredTo("<connector>") says "our changes are pushed to a system
// this connector drains". Neither is true if no such connector exists,
// and NEITHER FAILS VISIBLY when it is false:
//
//   - A mirror nobody fills is an empty concept that looks like an
//     empty catalog. Every read succeeds and returns nothing.
//   - A mirror target nobody drains is an outbox that accumulates
//     entries forever. Every write succeeds and nothing arrives.
//
// Both are silent data loss dressed as normal operation, which is why
// the name is resolved at boot and a miss REFUSES rather than warns.
//
// # Why this cannot simply always refuse
//
// The connector registry is populated by init() in the packages that
// implement connectors, which live under integrations/ -- and
// integrations/ imports component/memql, so component/memql's own tests
// cannot import them back. Those tests drive Init over the embedded
// tree. A check that refused any unresolvable name would therefore fail
// the engine's own suite the moment the first concept declared an
// origin, and would keep failing for a reason that is about the test
// binary's import graph rather than about the tree.
//
// So the check asks a prior question first: HAS THIS BUILD WIRED
// CONNECTORS AT ALL? A build that declares none cannot distinguish a
// typo from a correct name -- every name is unknown to it -- and a
// refusal from a blind instrument is a statement about the tool, not
// about the code.
//
// A blind build therefore does not refuse. IT SAYS SO, at Warn, naming
// every concept it could not verify. That is the whole difference
// between an honest gap and a hidden one: an operator reading the boot
// log of such a build learns that these declarations went unchecked,
// rather than reading a clean boot and concluding they were checked.
//
// Every node binary blank-imports the core plug-in set (app/plugins_core.go,
// untagged), so every deployed binary IS a sighted build and does
// refuse. cmd/memqllint imports it for the same reason: a lint pass
// whose claim is "this tree boots" has to know the connector names the
// engine will.

import (
	"fmt"
	"sort"
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// conceptConnectorRef is one place a concept names a connector.
type conceptConnectorRef struct {
	Concept   string
	Connector string
}

// collectDeclaredConnectorRefs gathers every (concept, connector) pair
// the tree declares, sorted for a stable diagnostic.
func collectDeclaredConnectorRefs(all []*concept.Concept) []conceptConnectorRef {
	var refs []conceptConnectorRef
	for _, c := range all {
		if c == nil {
			continue
		}
		for _, name := range c.DeclaredConnectors() {
			refs = append(refs, conceptConnectorRef{Concept: c.Name, Connector: name})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Concept != refs[j].Concept {
			return refs[i].Concept < refs[j].Concept
		}
		return refs[i].Connector < refs[j].Connector
	})
	return refs
}

// checkDeclaredConnectors resolves every connector name the tree
// declares against the connector registry.
//
// Returns an error naming the concept and the name on the first miss,
// which refuses boot. See the file header for the one case that does
// not refuse, and for why it announces itself instead.
func (e *MemQLEngine) checkDeclaredConnectors(all []*concept.Concept) error {
	return e.checkConnectorRefs(collectDeclaredConnectorRefs(all), memqlsync.DeclaredNames(), memqlsync.IsDeclared)
}

// checkConnectorRefs is the decision, separated from where the answer
// comes from.
//
// The registry is a PROCESS GLOBAL populated by init(), and that is not
// a detail a test can work around: declaring a name inside a test flips
// the whole binary from blind to sighted, at which point the embedded
// tree's own @origin("shopify") -- declared by a package component/memql
// cannot import -- becomes unresolvable and every later Init in that
// binary fails. A test that mutated the registry would therefore break
// its neighbours in a way that looks like their own bug.
//
// So the name set arrives as arguments and the tests drive this
// directly.
func (e *MemQLEngine) checkConnectorRefs(refs []conceptConnectorRef, known []string, isDeclared func(string) bool) error {
	if len(refs) == 0 {
		return nil
	}
	if len(known) == 0 || isDeclared == nil {
		e.warnConnectorCheckIsBlind(refs)
		return nil
	}

	for _, ref := range refs {
		if isDeclared(ref.Connector) {
			continue
		}
		return fmt.Errorf(
			"concept %q names connector %q, which this build does not serve. "+
				"A mirror nobody fills reads as an empty catalog and a mirror target nobody drains "+
				"accumulates outbox entries forever, so an unresolvable name refuses boot rather than "+
				"degrading quietly (epic memql#4378). Connectors this build serves: %s. "+
				"Either correct the name, or register the connector by calling sync.Declare(%q) from an "+
				"init() in the package that implements it",
			ref.Concept, ref.Connector, renderKnownConnectors(known), ref.Connector)
	}
	return nil
}

// warnConnectorCheckIsBlind announces that this build could not verify
// the declarations, and names every one of them.
//
// Named separately so its single caller reads as a decision rather than
// a logging aside, and so a test can assert the announcement happens: a
// gate that hides what it could not examine turns its own silence into
// a claim about the code.
func (e *MemQLEngine) warnConnectorCheckIsBlind(refs []conceptConnectorRef) {
	if e == nil || e.Logger == nil {
		return
	}
	unverified := make([]string, 0, len(refs))
	for _, ref := range refs {
		unverified = append(unverified, ref.Concept+" -> "+ref.Connector)
	}
	e.Logger.Warn(
		"connector names went UNVERIFIED: this build has wired no connectors, so it cannot tell a correct name from a typo. "+
			"Every deployed binary wires the core plug-in set and does check; a build that does not is a test binary or a tool",
		"component", "memql.engine",
		"unverified", strings.Join(unverified, ", "),
		"count", len(refs))
}

// renderKnownConnectors lists what this build serves, or says plainly
// that it serves none.
func renderKnownConnectors(known []string) string {
	if len(known) == 0 {
		return "(none)"
	}
	return strings.Join(known, ", ")
}
