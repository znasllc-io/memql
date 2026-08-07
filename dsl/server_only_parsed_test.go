package dsl

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageCompiler "github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// server_only_parsed_test.go -- memql#2875.
//
// The audit gates used to decide `@serverOnly` by REGEX over source, while the
// LOADER decides by PARSING and sets Function.ServerOnly. Those two can
// disagree, and the disagreement is FAIL-OPEN in the audit: a line beginning
// `@serverOnly` inside a multi-line annotation string, or inside a block
// comment opened on an `@`-line, satisfies the regex -- so the audit EXEMPTS
// the construct from per-row-authz classification and sdk/gen drops it from the
// client surface, while Function.ServerOnly stays false and NOTHING is enforced
// at runtime. #2861 (block comments in the DSL) made that more reachable.
//
// It is not attacker-reachable -- it requires authoring the construct in-repo.
// The reason to close it is that the classification test is the backstop that
// hard-fails on any new unclassified construct, and a construct that can slip
// out of that gate while ALSO having no runtime enforcement is the one shape
// the gate exists to make impossible.
//
// # This reads the PARSED tree, which is what the issue asked for
//
// An earlier attempt argued the audit could not read a parsed verdict from
// package `dsl` because `component/memql` imports `dsl`, so `dsl` cannot import
// back to load a function registry. The registry half of that is true and the
// conclusion was wrong: `component/memql/dslimports` is importable from here
// and ALREADY IS (dsl/filter_field_coverage_test.go, dsl/embed_test.go), and
// `dslimports.Load` hands back `Files map[path]*ast.File` -- the parsed AST for
// every file in the tree, in ~0.2s.
//
// The loader's rule is `hasFlagAttribute(def.Attributes, "serverOnly")`
// (component/memql/function_loader.go). serverOnlyConstructs applies exactly
// that rule to exactly the same parse, so "the gate exempted it" and "the
// runtime enforces it" cannot diverge by construction rather than merely
// failing a comparison. That is the difference between this and a parity test.

// serverOnlyKey identifies a construct for audit purposes. Name alone is not
// enough: two domains may declare the same construct name.
type serverOnlyKey struct {
	Path string
	Name string
}

var (
	serverOnlyOnce sync.Once
	serverOnlySet  map[serverOnlyKey]bool
	serverOnlyErr  error
	serverOnlySeen int
)

// serverOnlyConstructs returns the set of constructs carrying a REAL
// `@serverOnly` annotation, derived from the parsed AST rather than from source
// text.
//
// Cached: the parse is ~0.2s and several gates consult it.
func serverOnlyConstructs(t *testing.T) map[serverOnlyKey]bool {
	t.Helper()
	serverOnlyOnce.Do(func() {
		tree, err := dslimports.Load(Tree())
		if err != nil {
			// A tree that does not load is a different failure than a
			// divergence, and callers should say so rather than reporting an
			// empty @serverOnly set as "nothing is annotated".
			serverOnlyErr = err
			return
		}
		out := map[serverOnlyKey]bool{}
		seen := 0
		for path, file := range tree.Files {
			if file == nil {
				continue
			}
			for _, def := range file.Definitions {
				name, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				seen++
				for _, a := range attrs {
					// Same rule as the loader's hasFlagAttribute: presence of
					// the attribute NAME. An `@serverOnly` inside a string
					// literal or a comment is not an attribute, so it never
					// appears here -- which is the whole point.
					if a != nil && a.Name == "serverOnly" {
						out[serverOnlyKey{Path: path, Name: name}] = true
					}
				}
			}
		}
		serverOnlySet = out
		serverOnlySeen = seen
	})
	if serverOnlyErr != nil {
		t.Fatalf("dslimports.Load: %v -- the tree did not parse, so the @serverOnly set cannot "+
			"be derived. Fix the parse failure; do not read this as 'no construct is annotated'.",
			serverOnlyErr)
	}
	if serverOnlySeen == 0 {
		t.Fatal("walked 0 named declarations -- every gate keyed on this set would then exempt " +
			"nothing and pass vacuously, which is the failure mode that made the previous " +
			"user-scope detector report a meaningless zero (#2799). Check declNameAndAttributes " +
			"against the AST definition types.")
	}
	// A CLONE: four gates now hold this, and one of them writing to it would
	// corrupt the others' verdicts. Cheap -- eleven entries.
	return maps.Clone(serverOnlySet)
}

// declNameAndAttributes pulls the name + annotations off any named top-level
// declaration.
//
// Every Attributes-bearing kind must be listed. The `default: return false` is
// a SILENT DROP -- Go's type switch has no exhaustiveness check -- so
// TestServerOnlyParsedSetCoversEveryAttributedDeclKind walks the real tree and
// fails on any node type carrying an `Attributes` field that this switch does
// not handle. Without it, the first five kinds were missing (ShapeDecl 87,
// SpecDecl 36, PromptDecl 25, ActionDecl 9, CapabilityDecl 8 = 165
// declarations), which is the exact class of hole this file exists to close.
//
// SeedDecl / ToolDecl / ProviderDecl / PolicyDecl are deliberately absent: they
// carry no Attributes field, so there is nothing to read.
func declNameAndAttributes(def languageAst.Node) (string, []*languageAst.Attribute, bool) {
	switch d := def.(type) {
	case *languageAst.FunctionDef:
		return d.Name, d.Attributes, true
	case *languageAst.ConceptDecl:
		return d.Name, d.Attributes, true
	case *languageAst.BuiltinDecl:
		return d.Name, d.Attributes, true
	case *languageAst.ShapeDecl:
		return d.Name, d.Attributes, true
	case *languageAst.SpecDecl:
		return d.Name, d.Attributes, true
	case *languageAst.PromptDecl:
		return d.Name, d.Attributes, true
	case *languageAst.ActionDecl:
		return d.Name, d.Attributes, true
	case *languageAst.CapabilityDecl:
		return d.Name, d.Attributes, true
	default:
		return "", nil, false
	}
}

// TestServerOnlyParsedSetMatchesTheTree pins the derived set against the tree,
// so a change that empties or inflates it is visible.
func TestServerOnlyParsedSetMatchesTheTree(t *testing.T) {
	set := serverOnlyConstructs(t)
	if len(set) == 0 {
		t.Fatal("the parsed tree reports ZERO @serverOnly constructs. Either the annotation has " +
			"no live users -- in which case check whether the enforcement in " +
			"component/memql/function_validator.go is still exercised by anything -- or the " +
			"attribute name changed, which would silently exempt nothing and gate nothing.")
	}
	want := map[serverOnlyKey]bool{
		{Path: "identity/queries.memql", Name: "activeUsers"}:    true,
		{Path: "identity/queries.memql", Name: "userByEmail"}:    true,
		{Path: "identity/queries.memql", Name: "userByIdSystem"}: true,
		// memql#3217. The complete-set sibling of activeUsers, behind the
		// startup per-user seed sweep. @serverOnly for activeUsers' reason
		// minus most of the exposure -- its projection is userIdRef (row.id
		// alone, no @pii field), but the read still enumerates every user in
		// the cluster and cannot be caller-scoped: it runs under the seed
		// materializer's system actor at boot, where there is no requesting
		// user for actor.userId to name.
		{Path: "identity/queries.memql", Name: "usersForSeedSweep"}:         true,
		{Path: "identity/queries.memql", Name: "usersInDeletionCooldown"}:   true,
		{Path: "identity/queries.memql", Name: "usersScheduledForDeletion"}: true,
		{Path: "worker/queries.memql", Name: "runningPlansForUser"}:         true,
		// memql#2991. Caller-scoping is not merely hard here, it is
		// INEXPRESSIBLE: `update { id: args.userId; args.payload }` relates the
		// target to nothing, and no mutation in the tree carries a filter --
		// the grammar has no way to say "and the row is mine". An owner
		// predicate on the update's row selection is memql#2803 Phase 5.
		//
		// Meanwhile the row holds the caller's own cluster-wide `role`, which
		// is a plain settable enum, and a payload SPLAT names no fields so
		// validateMutationCallerArgs cannot see it. The one legitimate caller
		// is the identity admin server, which already authorizes in
		// admin/auth.go -- so origin gating puts the boundary where the
		// authorization already lives.
		{Path: "identity/mutations.memql", Name: "updateUser"}: true,
		// memql#2987, the node-token trio. All three were @public while
		// projecting identityFull -- which includes `credentials` -- and
		// nodeTokenIdentities had NO filter at all, so one unparameterised call
		// returned every node credential in the cluster to any authenticated
		// caller. @public enforces nothing; it only suppressed the classifier.
		//
		// Caller-scoping here does not merely fail, it names nobody: a
		// node_token row belongs to a cluster NODE, and its `userId` is the
		// synthetic bootstrap user the mint ran as, never a reader's id. So
		// `userId==actor.userId` returns zero rows for every real caller -- the
		// human admin on /admin/tokens, and the bootstrap handler, which has no
		// resolved actor at all because it runs BEFORE the node has an identity.
		// Origin is the only boundary that exists.
		//
		// Every caller is component/identity/store.go, allowlisted in
		// call_origin_conformance_test.go as server-initiated, so the internal
		// stamp these now require is legitimate rather than the request-derived
		// stamp refuted on memql#2989.
		{Path: "identity/queries.memql", Name: "nodeTokenIdentityByBinding"}: true,
		{Path: "identity/queries.memql", Name: "nodeTokenIdentities"}:        true,
		{Path: "identity/queries.memql", Name: "nodeTokenIdentityById"}:      true,
		// memql#3063. The item memql#2987 deferred and then closed: same shape
		// as the trio above -- caller-supplied id, no actor check, identityFull
		// projecting `credentials` (keyHash, registeredBy, lastSeenAt,
		// lastConnectedFromIP) -- so it was on the wire for any authenticated
		// caller who guessed a userId. Narrowing the projection was not
		// available (the Go parser reads the nested credentials struct and a
		// shape keys paths by their terminal segment), and userId==actor.userId
		// was not either (the revoke ownership check runs under
		// contextWithSystemActor). Its only caller is
		// component/identity/workertoken. That package is allowlisted in
		// call_origin_conformance_test.go NOT for pat's reason (server-initiated)
		// but as a request-derived exception whose precondition -- the userId is
		// always the authenticated caller's subject -- is asserted by
		// component/grpc/worker_token_caller_scope_test.go. See that entry.
		{Path: "identity/queries.memql", Name: "workerTokensForUser"}: true,
		// memql#3209. The row set behind the in-process agent registry
		// (component/memql/agents.go LoadFromRows). Caller-scoping is circular
		// in the #2800 sense: the registry is a process-wide catalog built ONCE
		// at startup under the seed materializer's system actor, so at the only
		// moment this query runs there is no requesting user for actor.userId to
		// name -- the filter would evaluate against an empty actor, return zero
		// rows, and silently disable every specialist.
		//
		// It is @serverOnly rather than merely unscoped because agentFull
		// projects `systemPrompt`: an unscoped catalog read of every user's
		// persona is admissible inside the server and not admissible on the
		// generated client SDK.
		//
		// Its sibling allAgents stays on the wire, paged and classified as
		// before -- this is an ADDITIONAL complete read, not a widening of that
		// one. Sole caller: component/memql/agents.go, which stamps
		// auth.ContextWithInternalOrigin.
		{Path: "agents/queries.memql", Name: "agentsForRegistry"}: true,
	}
	for k := range want {
		if !set[k] {
			t.Errorf("%s in %s no longer carries @serverOnly in the PARSED tree. If that is "+
				"deliberate it needs a caller-scope filter instead -- each of these is "+
				"server-only because caller-scoping is circular or because it is a sweep "+
				"(#2800 / #2883).", k.Name, k.Path)
		}
	}
	for k := range set {
		if !want[k] {
			t.Errorf("%s in %s GAINED @serverOnly. That exempts it from per-row-authz "+
				"classification (the gate short-circuits on hasServerOnly) and drops it from the "+
				"generated SDK, so it needs the same argument the others carry: why is "+
				"caller-scoping impossible here? If the answer is good, add it to `want`.", k.Name, k.Path)
		}
	}
	// These are the live set, keyed by PATH AND NAME. The prose used to say
	// "these six" and went on saying it at nine and at ten -- a hardcoded count
	// in a comment beside a map that grows is a lie with a delay fuse, so it is
	// gone rather than corrected (memql#3063).
	// Collapsing to name-only would defeat the key's stated purpose -- two
	// domains may declare the same construct name, so `activeUsers` in a
	// different file would satisfy the assertion.
	//
	// The assertion is two-way, and the ADDITION direction is the riskier one.
	// A construct DISAPPEARING means it silently lost its origin gate. A
	// construct APPEARING is how an author silences the per-row-authz
	// classification gate, which short-circuits on hasServerOnly -- so a new
	// @serverOnly on a client-facing query must not land with zero test signal.
	// The count is the covered set, not the whole tree. Attributes-bearing kinds
	// only: FunctionDef 476, ConceptDecl 100, ShapeDecl 87, BuiltinDecl 74,
	// SpecDecl 36, PromptDecl 25, ActionDecl 9, CapabilityDecl 8 = 815 of 1091
	// definitions. SeedDecl (185), ToolDecl (43), ProviderDecl (41) and
	// PolicyDecl (7) carry no Attributes field, so there is nothing to read --
	// TestServerOnlyParsedSetCoversEveryAttributedDeclKind is what keeps that
	// honest rather than assumed.
	t.Logf("parsed tree: %d @serverOnly construct(s) across %d attributed declarations", len(set), serverOnlySeen)
}

// TestServerOnlyParsedSetIgnoresSmuggledAnnotations is the failing-first half.
//
// It asserts the property that makes the parsed verdict better than the regex:
// an `@serverOnly` that is not an ANNOTATION -- because it sits inside a
// multi-line string, or inside a block comment -- must not appear in the set.
//
// Both fixtures are the REACHABLE spellings, verified by parsing them rather
// than by hand-reasoning. An earlier version of this test used
// "/*\n@serverOnly\n*/\nquery ..." which does not reach the audit at all: the
// preamble walk breaks on `*/`, so no gate ever saw it. The reachable
// block-comment form opens the comment on an `@`-line so it lands IN the
// preamble.
func TestServerOnlyParsedSetIgnoresSmuggledAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		// POSITIVE CONTROL first. Without it every case below passes on a
		// parser that ignores annotations entirely, which is the tautology this
		// test would otherwise be.
		{
			"a real annotation IS honoured",
			"@serverOnly\nconcept probeRealAnnotation {\n  a string\n}\n",
			true,
		},
		{
			"inside a multi-line annotation string",
			"@description(\"note\n@serverOnly\")\nconcept probeSmuggledViaString {\n  a string\n}\n",
			false,
		},
		{
			"inside a block comment opened on an @-line",
			"@description(\"a\") /*\n@serverOnly */\nconcept probeSmuggledViaComment {\n  a string\n}\n",
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parseOneFileForAudit(tc.src)
			if err != nil {
				t.Fatalf("fixture did not parse (%v).\nA fixture that does not parse cannot "+
					"register, so it proves nothing about the audit -- fix the fixture rather "+
					"than deleting the case.", err)
			}

			var got bool
			var decls int
			for _, def := range file.Definitions {
				_, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				decls++
				for _, a := range attrs {
					if a != nil && a.Name == "serverOnly" {
						got = true
					}
				}
			}
			if decls != 1 {
				t.Fatalf("fixture yielded %d named declarations, want 1 -- the case is not "+
					"measuring what it says", decls)
			}
			if got != tc.want {
				t.Errorf("parsed @serverOnly = %v, want %v (%s)", got, tc.want, tc.name)
			}

			// The CONTRAST that justifies the parsed verdict: for the smuggled
			// shapes the old source regex says YES where the parser says no.
			// That gap is the fail-open the issue reported -- the audit exempts
			// the construct while nothing enforces it at runtime.
			byRegex := smuggledServerOnlyRe.MatchString(tc.src)
			if !tc.want && !byRegex {
				t.Errorf("the source regex does NOT match %s either, so this case demonstrates "+
					"no divergence and guards nothing. Either the shape stopped being reachable "+
					"or the fixture is wrong -- check before deleting the case.", tc.name)
			}
		})
	}
}

// smuggledServerOnlyRe is the pattern the audit gates used before this change.
// It exists only so the tests above can show the DIVERGENCE the parsed verdict
// removes; nothing in the audit path consults it.
var smuggledServerOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\b`)

// parseOneFileForAudit parses a fixture through the entry point the TREE LOAD
// uses -- languageCompiler.ParseFileSource, which applies the full rewrite chain
// (StripNonProceduralBlocks + NormaliseAll).
//
// It used to call languageParser.ParseFile, the bare lexer/parser, under a
// comment claiming it was "the same entry point the tree load uses". That was
// false, and the two demonstrably disagree: `@serverOnly` above an
// `automation a {...}` is a parse ERROR to ParseFile and a successful
// FunctionDef with serverOnly=true to ParseFileSource. A fixture validated on
// the bare parser is validated on a path production never runs.
func parseOneFileForAudit(src string) (*languageAst.File, error) {
	return languageCompiler.ParseFileSource(src)
}

// TestServerOnlyParsedSetCoversEveryAttributedDeclKind is the exhaustiveness
// check the type switch cannot give us.
//
// It reflects over every node in the real tree and fails on any type that has an
// `Attributes []*ast.Attribute` field but is not handled by
// declNameAndAttributes. Reflection is the right tool ONLY here: the walk itself
// stays an explicit switch (fast, obvious), and this test is what makes the
// switch's completeness checkable rather than assumed.
func TestServerOnlyParsedSetCoversEveryAttributedDeclKind(t *testing.T) {
	tree, err := dslimports.Load(Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}

	seen := map[string]int{}
	uncovered := map[string]int{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			typeName := fmt.Sprintf("%T", def)
			seen[typeName]++

			_, _, covered := declNameAndAttributes(def)
			if covered {
				continue
			}
			// Not covered -- does it carry Attributes anyway?
			v := reflect.Indirect(reflect.ValueOf(def))
			if v.Kind() != reflect.Struct {
				continue
			}
			f := v.FieldByName("Attributes")
			if f.IsValid() && f.Kind() == reflect.Slice {
				uncovered[typeName]++
			}
		}
	}

	if len(seen) == 0 {
		t.Fatal("walked 0 definitions; this test would pass vacuously")
	}

	names := make([]string, 0, len(uncovered))
	for n := range uncovered {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Errorf("%s carries an Attributes field and appears %d time(s) in the tree, but "+
			"declNameAndAttributes does not handle it -- so every construct of that kind is "+
			"SILENTLY EXCLUDED from the @serverOnly audit. Add a case for it.", n, uncovered[n])
	}

	kinds := make([]string, 0, len(seen))
	for n := range seen {
		kinds = append(kinds, fmt.Sprintf("%s=%d", n, seen[n]))
	}
	sort.Strings(kinds)
	t.Logf("declaration kinds in the tree: %s", strings.Join(kinds, " "))
}
