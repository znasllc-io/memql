package memql

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// access_gate_load_test.go -- @accessGate: the declaration a construct makes,
// the load rules that hold it, and the registry it feeds (design record
// 2026-10-29, section 7).
//
// The rules are exercised through a REAL DSL load: a bundle of domains goes
// through the same function and tool loaders a node boots with, then through
// the registration pass, and what comes out is the wording package analysis
// shows an author. That is the tier the rules live in, and the only one that
// reaches a product bundle no Go test in this repository walks. The few rules
// a real load cannot reach -- the annotation registry refuses them first, so
// the loader's own copy is the second line -- are pure-conversion tests, and
// each says so.

// ---------------------------------------------------------------------------
// FIXTURES
// ---------------------------------------------------------------------------

// gateConceptSource is the concept every fixture domain reads and writes. It
// carries the fields a gated construct filters or stamps on, plus an int so a
// reserved argument can be declared as something other than a string without
// the filter that reads it failing to lower first.
const gateConceptSource = `
/// A brief a gated dashboard shows.
@rowAuthz(owner="ownerUserId")
concept deskBrief {
  ownerUserId   string
  accessCodeId  string
  accessSubject string
  siteId        string
  title         string
  count         int
}
`

const (
	gateDashboard = `@accessGate(deployable="client-desk", gate="dashboard")`
	gateOverview  = `@accessGate(deployable="client-desk", gate="overview")`

	// gateImport is the file-top line that lets a construct declared in one
	// file namespace bind to the desk domain's concept. A gate is bound to
	// its package through the domain of the concept, not through the file it is
	// written in, and this is the fixture that tells the two apart.
	gateImport = "use desk.concepts.{ deskBrief }\n\n"

	// codeArgs and codeFilter agree on purpose: a declared argument the body
	// never reads is a load error of its own, which would drown the rule a row
	// is about.
	codeArgs   = "    accessCodeId string!\n"
	codeFilter = `row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId`
)

// gateQuery renders a query over the fixture concept with header's
// annotations above it.
func gateQuery(header, name, args, filter string) string {
	return header + "\n@actor\nquery deskBrief " + name + " {\n  args {\n" + args + "  }\n" +
		"  filter row => " + filter + "\n  paginate 20\n  shape deskBriefFull\n}\n"
}

// gateMutation renders an insert over the fixture concept. It accepts title,
// so args must declare it; stamps are whole lines of the stamp block.
func gateMutation(header, name, args, stamps string) string {
	return header + "\n@actor\nmutation deskBrief " + name + " {\n  args {\n" + args + "  }\n" +
		"  insert {\n    accept { title }\n    stamp {\n" + stamps + "      ownerUserId: actor.userId\n    }\n  }\n}\n"
}

// gateOpens is one thing a bundle that loads clean must open, for the one
// domain that declares it.
type gateOpens struct{ deployable, gate, construct, kind, domain string }

// restoreAccessGatesAfter puts the live registry back when the test ends.
// Publishing is what a boot does, and the registry is process-wide, so a test
// that publishes must not leak into the next one.
func restoreAccessGatesAfter(t *testing.T) {
	t.Helper()
	accessGateMu.RLock()
	previous := accessGates
	accessGateMu.RUnlock()
	t.Cleanup(func() {
		accessGateMu.Lock()
		defer accessGateMu.Unlock()
		accessGates = previous
	})
}

// loadGateBundle loads a bundle of DSL files, path to source, through the
// loaders a node boots with and then the access gate pass, and returns every
// problem strict boot would print for it. The wording is the diagnostic
// package analysis gives an author (skipDiagnostic), so a test reads what
// they read.
//
// publish is what a boot passes: true writes the live registry, false is the
// candidate validation a package deploy runs. A caller that publishes gets the
// registry restored when the test ends.
func loadGateBundle(t *testing.T, files map[string]string, publish bool) []string {
	t.Helper()
	if publish {
		restoreAccessGatesAfter(t)
	}
	tree := fstest.MapFS{}
	for path, src := range files {
		tree[path] = &fstest.MapFile{Data: []byte(src)}
	}
	withLanguageLines(tree)

	concepts, conceptSkips, err := BuildUnifiedConcepts(discardLogger(), tree)
	if err != nil || len(conceptSkips) > 0 {
		t.Fatalf("the fixture's concepts did not build: %v %+v", err, conceptSkips)
	}
	functions, tools, report := newFunctionRegistry(), newToolRegistry(), newLoadReport()
	if _, _, err := loadUnifiedFunctionsFromTree(tree, discardLogger(), functions, memoryNodes.NewRegistry(concepts), report); err != nil {
		t.Fatalf("function loader: %v", err)
	}
	if _, err := loadUnifiedToolsFromTree(tree, discardLogger(), tools, report); err != nil {
		t.Fatalf("tool loader: %v", err)
	}

	// The same fold the engine does (engine_bootstrap.go), so a refusal here
	// is shaped exactly as it is on a node.
	for _, fault := range loadAccessGates(functions, publish) {
		report.AddSkip(accessGateSkip(fault))
	}
	var problems []string
	for _, skip := range report.Skipped {
		problems = append(problems, skipDiagnostic(skip))
	}
	return problems
}

func gateBundleDomains(domains map[string]string) map[string]string {
	files := map[string]string{}
	for domain, src := range domains {
		files[domain+"/concepts.memql"] = gateConceptSource
		files[domain+"/constructs.memql"] = src
	}
	return files
}

// ---------------------------------------------------------------------------
// THE LOAD RULES
// ---------------------------------------------------------------------------

// One row per rule in the design record's section 7, refused and accepted.
// A refused row names the fragments its ONE problem must carry, and they are
// always the construct and what to change: a refusal that names neither sends
// an author grepping a bundle for the cause.
func TestAccessGateLoadRules(t *testing.T) {
	const tool = `@handler(type="query", query="builtin help(name: args.name)")
tool deskTool {
  name string @required @description("function name")
}
`
	cases := []struct {
		name    string
		domains map[string]string
		// more are extra files by path, beside the standard domains: a second
		// file namespace that binds to a domain's concept through an import.
		more map[string]string
		// refused: the fragments the single problem carries. Empty means the
		// bundle must load clean and open every gate in opens.
		refused []string
		opens   []gateOpens
	}{
		// ---- refused -------------------------------------------------------
		{
			name:    "on a logic",
			domains: map[string]string{"desk": gateDashboard + "\nlogic deskRun {\n  return true\n}\n"},
			refused: []string{"deskRun", "@accessGate", "accepted on a query and a mutation"},
		},
		{
			name:    "on a tool",
			domains: map[string]string{"desk": gateDashboard + "\n" + tool},
			refused: []string{"deskTool", "@accessGate", "accepted on a query and a mutation"},
		},
		{
			name: "written twice",
			domains: map[string]string{"desk": gateQuery(gateDashboard+"\n"+gateOverview,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "@accessGate is written more than once", "keep one"},
		},
		{
			name: "without deployable=",
			domains: map[string]string{"desk": gateQuery(`@accessGate(gate="dashboard")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "requires both deployable= and gate=", "deployable= is missing"},
		},
		{
			name: "without gate=",
			domains: map[string]string{"desk": gateQuery(`@accessGate(deployable="client-desk")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "requires both deployable= and gate=", "gate= is missing"},
		},
		{
			name: "with a value that is not a string",
			domains: map[string]string{"desk": gateQuery(`@accessGate(deployable=5, gate="dashboard")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "deployable= must be a quoted string"},
		},
		{
			name: `a deployable written "Client_Desk"`,
			domains: map[string]string{"desk": gateQuery(`@accessGate(deployable="Client_Desk", gate="dashboard")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", `"Client_Desk"`, "lowercase letters, digits or hyphens"},
		},
		{
			name: `a gate written "9lives"`,
			domains: map[string]string{"desk": gateQuery(`@accessGate(deployable="client-desk", gate="9lives")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", `"9lives"`, "starting with a letter"},
		},
		{
			name: "with @serverOnly",
			domains: map[string]string{"desk": gateQuery(gateDashboard+"\n@serverOnly",
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "@serverOnly", "Remove @serverOnly"},
		},
		{
			name: "with @requiresRank",
			domains: map[string]string{"desk": gateQuery(gateDashboard+"\n"+`@requiresRank("admin")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "@requiresRank", "Remove @requiresRank"},
		},
		{
			name: "with @requiresCapability",
			domains: map[string]string{"desk": gateQuery(gateDashboard+"\n"+`@requiresCapability("read", "principal")`,
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "@requiresCapability", "Remove @requiresCapability"},
		},
		{
			// @disabled takes a construct out of service, not out of the tree:
			// it is still maintained and must still be valid, because
			// re-enabling it is only removing the annotation. A disabled gate
			// is not a way to park a rule it breaks.
			name: "a disabled construct that breaks a rule",
			domains: map[string]string{"desk": gateQuery(gateDashboard+"\n@serverOnly\n@disabled",
				"deskBriefs", codeArgs, codeFilter)},
			refused: []string{"deskBriefs", "@serverOnly", "Remove @serverOnly"},
		},
		{
			name: "a reserved argument declared as int",
			domains: map[string]string{"desk": gateQuery(gateDashboard,
				"deskBriefs", "    accessCodeId int!\n", `row.count == args.accessCodeId`)},
			refused: []string{"deskBriefs", "accessCodeId", "int", "Declare it as string"},
		},
		{
			// Two file namespaces, one domain: the second construct binds to the
			// desk concept through an import, so both belong to desk.
			// Different gates on purpose: the call names the construct and not
			// the gate, so the rule is about the name under the deployable in
			// the domain, whichever gate each one opens.
			name:    "two constructs named alike under one deployable in one domain",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter)},
			more: map[string]string{
				"deskmore/constructs.memql": gateImport + gateQuery(gateOverview, "deskBriefs", codeArgs, codeFilter),
			},
			refused: []string{"deskBriefs", `DSL domain "desk"`, "desk (", "deskmore (", `"client-desk"`, "Rename one of them"},
		},

		{
			// A call names a construct as namespace.name, and the namespace of a
			// construct in a nested directory is a path, "desk/sub", which
			// the call syntax cannot write: the slash reads as division. The
			// gate could never call it, so it is refused where an author reads
			// it and not left to fail as a visitor's 502.
			name: "a construct in a nested directory",
			more: map[string]string{
				"desk/concepts.memql":       gateConceptSource,
				"desk/sub/constructs.memql": gateImport + gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
			},
			refused: []string{"deskBriefs", "nested directory", `"desk/sub"`, "Move the construct"},
		},

		// ---- accepted ------------------------------------------------------
		{
			name: "a query and a mutation under one gate",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter) +
				gateMutation(gateDashboard, "deskNote", codeArgs+"    title string!\n",
					"      accessCodeId: args.accessCodeId\n")},
			opens: []gateOpens{
				{"client-desk", "dashboard", "deskBriefs", "query", "desk"},
				{"client-desk", "dashboard", "deskNote", "mutation", "desk"},
			},
		},
		{
			name: "the same construct name under two deployables",
			domains: map[string]string{
				"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
				"shipping": gateQuery(`@accessGate(deployable="shipping-desk", gate="dashboard")`,
					"deskBriefs", codeArgs, codeFilter),
			},
			opens: []gateOpens{
				{"client-desk", "dashboard", "deskBriefs", "query", "desk"},
				{"shipping-desk", "dashboard", "deskBriefs", "query", "shipping"},
			},
		},
		{
			// The same name under the same deployable and gate in a second DSL
			// domain is another product's declaration, not a collision. Neither
			// is closed by the other, and each opens for its own domain alone.
			name: "the same construct name and deployable in two domains",
			domains: map[string]string{
				"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
				"partner":   gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
			},
			opens: []gateOpens{
				{"client-desk", "dashboard", "deskBriefs", "query", "desk"},
				{"client-desk", "dashboard", "deskBriefs", "query", "partner"},
			},
		},
		{
			name: "a reserved argument declared as an optional string",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs",
				codeArgs+"    accessSubject string\n", codeFilter+` && row.accessSubject == args.accessSubject`)},
			opens: []gateOpens{{"client-desk", "dashboard", "deskBriefs", "query", "desk"}},
		},
		{
			// A disabled construct is out of service, so it opens nothing and
			// cannot collide with a live one that shares its name in its domain.
			// It cannot break the live one's call either, which is a different
			// thing and holds because the call is qualified: the engine's bare
			// lookup counts a disabled sibling, and a lookup by namespace.name
			// does not. TestAccessGateCallsReachTheirOwnConstructsThroughTheEngine
			// runs a disabled sibling through the engine.
			name:    "a disabled construct sharing a live one's name in one domain",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter)},
			more: map[string]string{
				"deskmore/constructs.memql": gateImport + gateQuery(gateOverview+"\n@disabled", "deskBriefs", codeArgs, codeFilter),
			},
			opens: []gateOpens{{"client-desk", "dashboard", "deskBriefs", "query", "desk"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := gateBundleDomains(tc.domains)
			for path, src := range tc.more {
				files[path] = src
			}
			problems := loadGateBundle(t, files, true)

			if len(tc.refused) > 0 {
				if len(problems) != 1 {
					t.Fatalf("want exactly one problem, got %d:\n  %s", len(problems), strings.Join(problems, "\n  "))
				}
				for _, fragment := range tc.refused {
					if !strings.Contains(problems[0], fragment) {
						t.Errorf("the refusal does not carry %q:\n  %s", fragment, problems[0])
					}
				}
				return
			}

			if len(problems) != 0 {
				t.Fatalf("a bundle that must load clean was refused:\n  %s", strings.Join(problems, "\n  "))
			}
			for _, want := range tc.opens {
				got := AccessGateFor(want.deployable, want.gate, want.construct, []string{want.domain})
				if got == nil {
					t.Errorf("%s/%s/%s opens nothing for %s", want.deployable, want.gate, want.construct, want.domain)
					continue
				}
				if got.Kind != want.kind || got.Domain != want.domain {
					t.Errorf("%s/%s/%s is a %s of %s, want a %s of %s", want.deployable, want.gate, want.construct,
						got.Kind, got.Domain, want.kind, want.domain)
				}
			}
		})
	}
}

// A refusal opens nothing. Strict boot stops a node on the fault, but
// MEMQL_DSL_ALLOW_SKIPS is the break-glass, and a construct that was refused
// must not be the one that quietly opens when the fault is waved through.
func TestAccessGateARefusedConstructOpensNothing(t *testing.T) {
	problems := loadGateBundle(t, gateBundleDomains(map[string]string{
		"desk": gateQuery(gateDashboard+"\n@serverOnly", "deskBriefs", codeArgs, codeFilter),
	}), true)
	if len(problems) != 1 {
		t.Fatalf("setup: want the one refusal, got %v", problems)
	}
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"}); got != nil {
		t.Fatalf("a refused construct opens a gate anyway: %+v", got)
	}
	if got := AccessGatesFor("client-desk", []string{"desk"}); len(got) != 0 {
		t.Fatalf("a refused construct is listed as a gate: %+v", got)
	}
}

// Both of two same-named constructs in one domain are refused, and neither
// opens. Keeping whichever loaded first would let load order decide which
// construct a code reaches, and nothing resolves load order.
//
// The refusal stops at the domain. A second product's construct with the same
// name under the same deployable is not part of the collision, so it stays
// open: closing it would let any other package shut a gate that is not theirs.
func TestAccessGateTwoConstructsOfOneDomainWithOneNameBothStayClosed(t *testing.T) {
	files := gateBundleDomains(map[string]string{
		"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
		"partner":   gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
	})
	files["deskmore/constructs.memql"] = gateImport + gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter)

	problems := loadGateBundle(t, files, true)
	if len(problems) != 1 {
		t.Fatalf("want one problem naming both, got %v", problems)
	}
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"}); got != nil {
		t.Fatalf("one of two colliding constructs opens a gate: %+v", got)
	}
	got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"partner"})
	if got == nil || got.Domain != "partner" {
		t.Fatalf("another domain's construct was closed by a collision that is not its own: %+v", got)
	}
}

// The refusals of the registration pass land on the LoadReport as
// memql.accessGateRegistration, naming the construct and its file, which is
// what lets strict boot refuse them and a package deploy report them.
func TestAccessGateFaultsAreShapedForTheLoadReport(t *testing.T) {
	fn := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
	fn.ServerOnly = true
	fn.Origin = "unified:desk/queries.memql"

	functions := newFunctionRegistry()
	if err := functions.Upsert(fn); err != nil {
		t.Fatal(err)
	}
	faults := loadAccessGates(functions, false)
	if len(faults) != 1 {
		t.Fatalf("want one fault, got %v", faults)
	}
	skip := accessGateSkip(faults[0])
	if skip.Component != "memql.accessGateRegistration" {
		t.Errorf("Component = %q, want memql.accessGateRegistration", skip.Component)
	}
	if skip.Name != "deskBriefs" || skip.Keyword != "query" || skip.File != "desk/queries.memql" {
		t.Errorf("the skip does not name the construct: %+v", skip)
	}
	if skip.Err == "" {
		t.Error("the skip carries no text")
	}
}

// ---------------------------------------------------------------------------
// THE RULES A REAL LOAD CANNOT REACH
// ---------------------------------------------------------------------------

// gatedFunction is a loaded construct as the registration pass sees it.
func gatedFunction(name, kind, deployable, gate string) *Function {
	return &Function{
		Name:         name,
		FunctionKind: kind,
		Enabled:      true,
		BoundConcept: "v1:desk:deskBrief",
		Origin:       "unified:desk/constructs.memql",
		AccessGate:   &AccessGateDecl{Deployable: deployable, Gate: gate},
	}
}

func gateAttribute(args map[string]any) *languageParser.Attribute {
	return &languageParser.Attribute{Name: languageParser.AttrAccessGate, Args: args}
}

// The annotation registry refuses a second @accessGate at parse time, so a
// real load never reaches the loader's own copy of the rule. It stays because
// a loader that folded annotations one at a time would let the second silently
// win, and open a gate the reader did not see.
func TestAccessGateDeclarationRefusesAWrittenTwiceAnnotation(t *testing.T) {
	def := &languageParser.FunctionDef{Attributes: []*languageParser.Attribute{
		gateAttribute(map[string]any{"deployable": "client-desk", "gate": "dashboard"}),
		gateAttribute(map[string]any{"deployable": "client-desk", "gate": "overview"}),
	}}
	_, err := accessGateDeclaration(def)
	if err == nil || !strings.Contains(err.Error(), "written twice") {
		t.Fatalf("a second @accessGate was not refused as written twice: %v", err)
	}
}

func TestAccessGateDeclarationReadsBothKeysAndOnlyThose(t *testing.T) {
	def := &languageParser.FunctionDef{Attributes: []*languageParser.Attribute{
		{Name: "actor"},
		gateAttribute(map[string]any{"deployable": " client-desk ", "gate": "dashboard"}),
	}}
	decl, err := accessGateDeclaration(def)
	if err != nil {
		t.Fatalf("a valid declaration was refused: %v", err)
	}
	if decl == nil || decl.Deployable != "client-desk" || decl.Gate != "dashboard" {
		t.Fatalf("decl = %+v, want client-desk/dashboard (whitespace trimmed)", decl)
	}

	for _, plain := range []*languageParser.FunctionDef{nil, {}, {Attributes: []*languageParser.Attribute{{Name: "actor"}}}} {
		if decl, err := accessGateDeclaration(plain); decl != nil || err != nil {
			t.Errorf("a construct without @accessGate read as %+v, %v; want nothing", decl, err)
		}
	}
}

// Only a query or a mutation can carry a gate. The parser's registry refuses
// the other kinds first, so this is the conversion's own copy, and it matters
// for a Function that reached the registry some other way: a gate pointed at a
// logic would put arbitrary builtin calls within reach of anyone with a code.
func TestAccessGateEntryRefusesAnythingButAQueryOrAMutation(t *testing.T) {
	for _, kind := range []string{"logic", "automation", "builtin", "tool"} {
		t.Run(kind, func(t *testing.T) {
			_, err := accessGateEntryFromFunction(gatedFunction("deskRun", kind, "client-desk", "dashboard"))
			if err == nil {
				t.Fatalf("@accessGate on a %s was accepted", kind)
			}
			for _, fragment := range []string{"deskRun", kind, "query or a mutation"} {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("the refusal does not carry %q: %v", fragment, err)
				}
			}
		})
	}

	// An empty kind is how Function documents a query, so it is one.
	entry, err := accessGateEntryFromFunction(gatedFunction("deskBriefs", "", "client-desk", "dashboard"))
	if err != nil || entry.Kind != AccessKindQuery {
		t.Errorf("an empty FunctionKind read as %+v, %v; want a query", entry, err)
	}
}

// A gate is bound to the package that ships it through the DSL domain of the
// concept the construct is bound to, as a shopper extension's declaring domain
// is taken. A construct bound to no concept has no domain to bind to, and a gate
// that belonged to no package could be reached from every one. Every query and
// mutation a real load produces carries a signature concept, so this is the
// conversion's own copy of the rule, for a Function built some other way.
func TestAccessGateEntryNeedsABoundConceptAndTakesItsDomainFromIt(t *testing.T) {
	for _, bound := range []string{"", "deskBrief", "v1:desk"} {
		fn := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
		fn.BoundConcept = bound
		_, err := accessGateEntryFromFunction(fn)
		if err == nil {
			t.Errorf("a construct bound to %q was accepted; its domain is unknowable", bound)
			continue
		}
		for _, fragment := range []string{"deskBriefs", "binds no concept", "Bind the construct to a concept"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("bound to %q, the refusal does not carry %q: %v", bound, fragment, err)
			}
		}
	}

	// The domain is the concept's and not the file's. A construct written in
	// one file namespace and bound to another domain's concept belongs to the
	// domain of the concept, which is the package that ships it.
	fn := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
	fn.Origin = "unified:deskmore/constructs.memql"
	fn.BoundConcept = "v1:partner:thing"
	entry, err := accessGateEntryFromFunction(fn)
	if err != nil || entry.Domain != "partner" {
		t.Fatalf("Domain = %q, %v; want partner, from v1:partner:thing and not from the file", entry.Domain, err)
	}
	// The NAMESPACE is the other way round: it is where the engine keeps the
	// construct, which is the file's, and has nothing to do with the concept.
	if entry.Namespace != "deskmore" {
		t.Fatalf("Namespace = %q, want deskmore, from the file and not from the concept", entry.Namespace)
	}
	// A nested namespace belongs to its root, as it does for a shopper extension.
	fn.BoundConcept = "v1:partner:orders:thing"
	if entry, err := accessGateEntryFromFunction(fn); err != nil || entry.Domain != "partner" {
		t.Fatalf("a nested namespace read as %q, %v; want its root, partner", entry.Domain, err)
	}
}

// The call is namespace.name, and only a namespace that is one word can be
// written in it. A real load reaches this with a construct in a nested
// directory (the table row above); this pins the rule on the conversion and
// shows the single-directory namespaces it leaves alone.
func TestAccessGateEntryRefusesANamespaceTheCallSyntaxCannotWrite(t *testing.T) {
	fn := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
	for _, origin := range []string{
		"unified:desk/sub/constructs.memql",
		"unified:desk/sub/deeper/constructs.memql",
	} {
		fn.Origin = origin
		_, err := accessGateEntryFromFunction(fn)
		if err == nil {
			t.Errorf("a construct loaded from %s was accepted", origin)
			continue
		}
		for _, fragment := range []string{"deskBriefs", "nested directory", "Move the construct"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("%s: the refusal does not carry %q: %v", origin, fragment, err)
			}
		}
	}
	for _, origin := range []string{"unified:desk/constructs.memql", "unified:client_desk2/queries.memql", ""} {
		fn.Origin = origin
		if _, err := accessGateEntryFromFunction(fn); err != nil {
			t.Errorf("a construct loaded from %q was refused: %v", origin, err)
		}
	}
}

// The names are checked again where the entry is made, because a Function can
// be built without going through the reader that checks them.
func TestAccessGateEntryChecksTheNamesAgain(t *testing.T) {
	for _, decl := range []AccessGateDecl{
		{Deployable: "", Gate: "dashboard"},
		{Deployable: "client-desk", Gate: ""},
		{Deployable: "Desk", Gate: "dashboard"},
		{Deployable: "client-desk", Gate: "Dashboard"},
		{Deployable: "client-desk", Gate: strings.Repeat("a", 41)},
		{Deployable: strings.Repeat("a", 64), Gate: "dashboard"},
	} {
		fn := gatedFunction("deskBriefs", "query", "x", "y")
		fn.AccessGate = &decl
		if _, err := accessGateEntryFromFunction(fn); err == nil {
			t.Errorf("%+v was accepted", decl)
		}
	}
	for _, decl := range []AccessGateDecl{
		{Deployable: "k", Gate: "g"},
		{Deployable: "0-day", Gate: "a-b-9"},
		{Deployable: strings.Repeat("a", 63), Gate: strings.Repeat("g", 40)},
	} {
		fn := gatedFunction("deskBriefs", "query", "x", "y")
		fn.AccessGate = &decl
		if _, err := accessGateEntryFromFunction(fn); err != nil {
			t.Errorf("%+v was refused: %v", decl, err)
		}
	}
}

// ---------------------------------------------------------------------------
// THE REGISTRY
// ---------------------------------------------------------------------------

// registryBundle is a bundle with every shape the registry answers about: a
// query that declares caller, code and subject arguments, a mutation that
// declares a caller argument and a code, a second gate, another deployable's
// gate that must stay out of this one's answers, and a second DSL domain that
// names the SAME deployable, the same gate and the same construct as the
// first. That last one is the collision the domain binding exists for: two
// products that happen to name a deployable alike.
func registryBundle() map[string]string {
	return gateBundleDomains(map[string]string{
		"desk": gateQuery(gateDashboard, "deskBriefs",
			"    accessCodeId string!\n    accessSubject string!\n    topic string\n",
			codeFilter+` && row.accessSubject == args.accessSubject && row.title == args.topic`) +
			gateMutation(gateDashboard, "deskNote", codeArgs+"    title string!\n",
				"      accessCodeId: args.accessCodeId\n") +
			gateQuery(gateOverview, "deskOverview", codeArgs, codeFilter),
		"shipping": gateQuery(`@accessGate(deployable="shipping-desk", gate="desk")`,
			"shippingDeskBriefs", codeArgs, codeFilter),
		"partner": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter) +
			gateQuery(`@accessGate(deployable="client-desk", gate="partner-only")`,
				"partnerOnlyBriefs", codeArgs, codeFilter),
	})
}

func TestAccessGateRegistryAnswers(t *testing.T) {
	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}
	ko := []string{"desk"}

	t.Run("a hit carries the declaration and its domain", func(t *testing.T) {
		got := AccessGateFor("client-desk", "dashboard", "deskBriefs", ko)
		if got == nil {
			t.Fatal("a declared gate opens nothing")
		}
		want := AccessGateEntry{
			Deployable: "client-desk", Gate: "dashboard", Construct: "deskBriefs", Kind: "query",
			Domain:    "desk",
			Namespace: "desk",
			Args:      []string{"topic"},
			Stamped:   []string{"accessCodeId", "accessSubject"},
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("entry = %+v\nwant   %+v", *got, want)
		}
	})

	t.Run("Args exclude the reserved names and Stamped lists them", func(t *testing.T) {
		note := AccessGateFor("client-desk", "dashboard", "deskNote", ko)
		if note == nil {
			t.Fatal("the mutation opens nothing")
		}
		if note.Kind != "mutation" || !reflect.DeepEqual(note.Args, []string{"title"}) ||
			!reflect.DeepEqual(note.Stamped, []string{"accessCodeId"}) {
			t.Fatalf("mutation entry = %+v", *note)
		}
		overview := AccessGateFor("client-desk", "overview", "deskOverview", ko)
		if overview == nil || len(overview.Args) != 0 || !reflect.DeepEqual(overview.Stamped, []string{"accessCodeId"}) {
			t.Fatalf("a construct with no caller argument has Args = %+v", overview)
		}
	})

	t.Run("it misses, and a miss is nil", func(t *testing.T) {
		for _, miss := range []struct {
			name                        string
			deployable, gate, construct string
			domains                     []string
		}{
			{"the wrong deployable", "shipping-desk", "dashboard", "deskBriefs", ko},
			{"the wrong gate", "client-desk", "overview", "deskBriefs", ko},
			{"an unknown construct", "client-desk", "dashboard", "deskMissing", ko},
			{"another deployable's construct", "client-desk", "desk", "shippingDeskBriefs", ko},
			{"a different case", "client-desk", "dashboard", "deskbriefs", ko},
			{"a padded name", "client-desk", "dashboard", " deskBriefs", ko},
			{"nothing at all", "", "", "", ko},
			// The package's domains are what bind a gate to a site. A construct
			// declared by a domain the package does not ship opens nothing, and
			// so does a package whose domains are not known.
			{"a domain outside the list", "client-desk", "overview", "deskOverview", []string{"partner"}},
			{"a domain that declares nothing here", "client-desk", "dashboard", "deskNote", []string{"shipping"}},
			{"a domain spelled differently", "client-desk", "dashboard", "deskBriefs", []string{"Desk"}},
			{"an empty list", "client-desk", "dashboard", "deskBriefs", []string{}},
			{"no list at all", "client-desk", "dashboard", "deskBriefs", nil},
			{"a list of blanks", "client-desk", "dashboard", "deskBriefs", []string{"", ""}},
		} {
			if got := AccessGateFor(miss.deployable, miss.gate, miss.construct, miss.domains); got != nil {
				t.Errorf("%s: opened %+v", miss.name, got)
			}
		}
	})

	t.Run("a gate lists its constructs and whether it uses a subject", func(t *testing.T) {
		got := AccessGatesFor("client-desk", ko)
		want := []AccessGateSummary{
			{Gate: "dashboard", UsesSubject: true, Constructs: []string{"deskBriefs", "deskNote"}},
			{Gate: "overview", UsesSubject: false, Constructs: []string{"deskOverview"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("gates = %+v\nwant    %+v", got, want)
		}
		if other := AccessGatesFor("shipping-desk", []string{"shipping"}); len(other) != 1 || other[0].Gate != "desk" {
			t.Errorf("another deployable's gates = %+v", other)
		}
		for _, none := range []struct {
			name       string
			deployable string
			domains    []string
		}{
			{"an unknown deployable", "nobody-here", ko},
			{"an empty deployable", "", ko},
			{"an empty list", "client-desk", []string{}},
			{"no list at all", "client-desk", nil},
			{"a list of blanks", "client-desk", []string{""}},
			{"a domain that declares nothing under it", "client-desk", []string{"shipping"}},
		} {
			if got := AccessGatesFor(none.deployable, none.domains); len(got) != 0 {
				t.Errorf("%s has gates: %+v", none.name, got)
			}
		}
	})

	t.Run("what comes back is a copy", func(t *testing.T) {
		got := AccessGateFor("client-desk", "dashboard", "deskBriefs", ko)
		got.Args[0], got.Stamped[0] = "tampered", "tampered"
		again := AccessGateFor("client-desk", "dashboard", "deskBriefs", ko)
		if again.Args[0] != "topic" || again.Stamped[0] != "accessCodeId" {
			t.Fatalf("a caller rewrote the registry through a returned entry: %+v", again)
		}
		gates := AccessGatesFor("client-desk", ko)
		gates[0].Constructs[0] = "tampered"
		if AccessGatesFor("client-desk", ko)[0].Constructs[0] != "deskBriefs" {
			t.Fatal("a caller rewrote the registry through a returned summary")
		}
	})
}

// Two products can name a deployable alike, and then a gate, and then a
// construct. The registry holds both, and a site's package decides which one a
// code reaches by the domains it ships: each answers for its own domain alone.
func TestAccessGateEachDomainAnswersOnlyForItsOwnDeclaration(t *testing.T) {
	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}

	desk := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"})
	partner := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"partner"})
	if desk == nil || partner == nil {
		t.Fatalf("a domain's own declaration does not open for it: desk=%+v partner=%+v", desk, partner)
	}
	if desk.Domain != "desk" || partner.Domain != "partner" {
		t.Errorf("the entries do not record their own domain: %q and %q", desk.Domain, partner.Domain)
	}
	if !reflect.DeepEqual(desk.Stamped, []string{"accessCodeId", "accessSubject"}) || len(desk.Args) != 1 {
		t.Errorf("desk answered with another declaration: %+v", desk)
	}
	if !reflect.DeepEqual(partner.Stamped, []string{"accessCodeId"}) || len(partner.Args) != 0 {
		t.Errorf("partner answered with another declaration: %+v", partner)
	}

	// The partner's declarations are its own: nothing of desk' leaks into
	// its answers, and nothing of the partner's into desk'.
	if AccessGateFor("client-desk", "dashboard", "deskNote", []string{"partner"}) != nil {
		t.Error("the partner's package reaches a construct only desk declares")
	}
	if AccessGateFor("client-desk", "partner-only", "partnerOnlyBriefs", []string{"desk"}) != nil {
		t.Error("desk' package reaches a construct only the partner declares")
	}

	// Naming a domain twice is naming it once.
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"partner", "partner"}); got == nil || got.Domain != "partner" {
		t.Errorf("a list naming one domain twice answered %+v", got)
	}
}

// A package that ships two domains declaring one name under one deployable has
// two constructs a visitor's request cannot tell apart, because it names the
// construct without a namespace. The registry does not pick one: it opens
// neither, and a gate it lists is a gate it can answer for.
func TestAccessGateAPackageShippingTwoDeclarationsOfOneNameOpensNeither(t *testing.T) {
	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}
	both := []string{"desk", "partner"}

	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", both); got != nil {
		t.Errorf("a name two of the package's domains declare opened for %s: %+v", got.Domain, got)
	}
	// The order of the list does not matter: it is the NAME that is ambiguous,
	// whichever declaration comes first.
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"partner", "desk"}); got != nil {
		t.Errorf("the order of the list decided an ambiguous name: %+v", got)
	}
	// What only one of them declares is not ambiguous.
	if got := AccessGateFor("client-desk", "dashboard", "deskNote", both); got == nil || got.Domain != "desk" {
		t.Errorf("an unambiguous construct was closed along with the ambiguous one: %+v", got)
	}

	// The operator surface lists what the call can reach. The one construct on
	// the dashboard gate that stamps a subject is the ambiguous one, so the gate
	// no longer offers a subject either.
	want := []AccessGateSummary{
		{Gate: "dashboard", UsesSubject: false, Constructs: []string{"deskNote"}},
		{Gate: "overview", UsesSubject: false, Constructs: []string{"deskOverview"}},
		{Gate: "partner-only", UsesSubject: false, Constructs: []string{"partnerOnlyBriefs"}},
	}
	if got := AccessGatesFor("client-desk", both); !reflect.DeepEqual(got, want) {
		t.Errorf("gates = %+v\nwant    %+v", got, want)
	}
}

// AccessGatesFor reads one package's domains and nothing else. A gate another
// domain declares under the same deployable, with the same name, is not merged
// into it, and neither is what that other domain's constructs stamp.
func TestAccessGatesForFiltersByDomain(t *testing.T) {
	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}

	partner := AccessGatesFor("client-desk", []string{"partner"})
	wantPartner := []AccessGateSummary{
		// No subject: the dashboard construct desk stamps one on is not the partner's.
		{Gate: "dashboard", UsesSubject: false, Constructs: []string{"deskBriefs"}},
		{Gate: "partner-only", UsesSubject: false, Constructs: []string{"partnerOnlyBriefs"}},
	}
	if !reflect.DeepEqual(partner, wantPartner) {
		t.Errorf("the partner's gates = %+v\nwant            %+v", partner, wantPartner)
	}

	desk := AccessGatesFor("client-desk", []string{"desk"})
	wantDesk := []AccessGateSummary{
		{Gate: "dashboard", UsesSubject: true, Constructs: []string{"deskBriefs", "deskNote"}},
		{Gate: "overview", UsesSubject: false, Constructs: []string{"deskOverview"}},
	}
	if !reflect.DeepEqual(desk, wantDesk) {
		t.Errorf("desk' gates = %+v\nwant            %+v", desk, wantDesk)
	}
}

// A gate is bound to its package through the domain of the concept its
// construct is bound to, and not through the file the construct is written in.
func TestAccessGateEntryRecordsTheDomainOfItsBinding(t *testing.T) {
	files := gateBundleDomains(map[string]string{
		"desk": gateQuery(gateOverview, "deskOverview", codeArgs, codeFilter),
	})
	// Written in the deskmore namespace, bound to a desk concept.
	files["deskmore/constructs.memql"] = gateImport + gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter)
	if problems := loadGateBundle(t, files, true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}

	got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"})
	if got == nil || got.Domain != "desk" {
		t.Fatalf("a construct bound to a desk concept is not desk': %+v", got)
	}
	// It belongs to desk, and the engine keeps it in deskmore, which
	// is the name a call has to use to reach it.
	if got.Namespace != "deskmore" || got.Call() != "query deskmore.deskBriefs" {
		t.Fatalf("Namespace = %q, Call = %q; want deskmore and the call through it", got.Namespace, got.Call())
	}
	if AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"deskmore"}) != nil {
		t.Error("the namespace of the file a construct is written in opened it")
	}
}

// AccessGateEntries is what package analysis reads: every entry, in an order
// that does not change between boots, as copies.
func TestAccessGateEntriesAreSortedCopies(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()
	if got := AccessGateEntries(); got == nil || len(got) != 0 {
		t.Fatalf("an empty registry lists %#v, want an empty list", got)
	}

	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}
	var got []string
	entries := AccessGateEntries()
	for _, entry := range entries {
		got = append(got, entry.Deployable+"/"+entry.Gate+"/"+entry.Construct+"@"+entry.Domain)
	}
	want := []string{
		"client-desk/dashboard/deskBriefs@desk",
		"client-desk/dashboard/deskBriefs@partner",
		"client-desk/dashboard/deskNote@desk",
		"client-desk/overview/deskOverview@desk",
		"client-desk/partner-only/partnerOnlyBriefs@partner",
		"shipping-desk/desk/shippingDeskBriefs@shipping",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v\nwant     %v", got, want)
	}

	entries[0].Construct, entries[0].Args = "tampered", []string{"tampered"}
	entries[1].Stamped = []string{"tampered"}
	again := AccessGateEntries()
	if again[0].Construct != "deskBriefs" || len(again[0].Args) != 1 || again[0].Args[0] != "topic" ||
		len(again[1].Stamped) != 1 || again[1].Stamped[0] != "accessCodeId" {
		t.Fatalf("a caller rewrote the registry through a returned entry: %+v", again[:2])
	}
}

// The test seam keys entries as the boot pass does, so a package that stands a
// gate up with it gets the registry's real behaviour: two domains' entries stay
// apart, and an entry with no domain is out of every site's reach.
func TestAccessGateSetForTestKeepsDomainsApart(t *testing.T) {
	restoreAccessGatesAfter(t)
	mine := AccessGateEntry{Deployable: "d", Gate: "g", Construct: "c", Kind: AccessKindQuery, Domain: "mine"}
	theirs := mine
	theirs.Domain, theirs.Stamped = "theirs", []string{AccessArgCodeID}
	orphan := AccessGateEntry{Deployable: "d", Gate: "g", Construct: "orphan", Kind: AccessKindQuery}
	SetAccessGatesForTest(mine, theirs, orphan)

	if got := AccessGateFor("d", "g", "c", []string{"mine"}); got == nil || got.Domain != "mine" || len(got.Stamped) != 0 {
		t.Errorf("mine answered %+v", got)
	}
	if got := AccessGateFor("d", "g", "c", []string{"theirs"}); got == nil || got.Domain != "theirs" || len(got.Stamped) != 1 {
		t.Errorf("theirs answered %+v", got)
	}
	if got := AccessGateFor("d", "g", "orphan", []string{"mine", "theirs", ""}); got != nil {
		t.Errorf("an entry with no domain was reached: %+v", got)
	}
	if n := len(AccessGateEntries()); n != 3 {
		t.Errorf("the seam stored %d entries, want 3", n)
	}
}

// Call is the exact text the handler renders before the arguments. It names
// the construct as namespace.name, the engine's own form, because the engine
// resolves a bare name cluster-wide and refuses an ambiguous one: a same-named
// construct anywhere -- a sibling that is disabled, a core construct a later
// release adds -- would otherwise break a product's gate.
func TestAccessGateCallIsTheQualifiedNameOfTheConstruct(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry AccessGateEntry
		want  string
	}{
		{"a query", AccessGateEntry{Kind: AccessKindQuery, Namespace: "desk", Construct: "deskBriefs"},
			"query desk.deskBriefs"},
		{"a mutation", AccessGateEntry{Kind: AccessKindMutation, Namespace: "desk", Construct: "deskNote"},
			"desk.deskNote"},
		{"a namespace that is not the domain", AccessGateEntry{Kind: AccessKindQuery, Domain: "desk", Namespace: "deskmore", Construct: "deskBriefs"},
			"query deskmore.deskBriefs"},
		{"an entry that has no namespace", AccessGateEntry{Kind: AccessKindQuery, Construct: "deskBriefs"},
			"query deskBriefs"},
	} {
		if got := tc.entry.Call(); got != tc.want {
			t.Errorf("%s: Call() = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The text is built from the same two parts the function registry keys the
	// construct by, so it names exactly the construct the entry was made from.
	fn := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
	fn.Origin = "unified:deskmore/constructs.memql"
	entry, err := accessGateEntryFromFunction(fn)
	if err != nil {
		t.Fatal(err)
	}
	registryKey := QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name)
	if entry.Call() != AccessCallPrefix(entry.Kind)+registryKey {
		t.Errorf("Call() = %q, but the registry keys the construct %q", entry.Call(), registryKey)
	}
}

func TestAccessGateCallPrefixMirrorsTheShopperOne(t *testing.T) {
	for kind, want := range map[string]string{
		AccessKindQuery:    "query ",
		AccessKindMutation: "",
		"":                 "",
		"builtin":          "",
	} {
		if got := AccessCallPrefix(kind); got != want {
			t.Errorf("AccessCallPrefix(%q) = %q, want %q", kind, got, want)
		}
	}
}

// The reserved names are the contract between this registry and the handler
// that stamps them: a name here the handler does not stamp is an argument no
// caller can supply, and a name stamped that is not here is one a caller can.
func TestAccessGateReservedArgumentsArePinned(t *testing.T) {
	want := []string{"siteId", "accessCodeId", "accessGate", "accessSubject"}
	if !reflect.DeepEqual(ReservedAccessArgs, want) {
		t.Fatalf("ReservedAccessArgs = %v, want %v", ReservedAccessArgs, want)
	}
}

// A domain dropped from a bundle must stop opening anything at the next boot,
// and a candidate validation (a package deploy's analysis) must not touch what
// the node is serving.
func TestAccessGateRegistryIsRebuiltOnLoad(t *testing.T) {
	restoreAccessGatesAfter(t)

	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: %v", problems)
	}
	if AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"}) == nil {
		t.Fatal("setup: the gate was not published")
	}

	t.Run("a candidate does not publish", func(t *testing.T) {
		candidate := gateBundleDomains(map[string]string{
			"desk": gateQuery(`@accessGate(deployable="client-desk", gate="candidate")`,
				"deskCandidate", codeArgs, codeFilter),
		})
		if problems := loadGateBundle(t, candidate, false); len(problems) != 0 {
			t.Fatalf("the candidate was refused: %v", problems)
		}
		if AccessGateFor("client-desk", "candidate", "deskCandidate", []string{"desk"}) != nil {
			t.Error("validating a candidate opened its gate on the live node")
		}
		if AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"desk"}) == nil {
			t.Error("validating a candidate closed a gate the live node serves")
		}
	})

	t.Run("a load replaces the whole view", func(t *testing.T) {
		next := gateBundleDomains(map[string]string{
			"desk": gateQuery(gateOverview, "deskOverview", codeArgs, codeFilter),
		})
		if problems := loadGateBundle(t, next, true); len(problems) != 0 {
			t.Fatalf("the next load was refused: %v", problems)
		}
		if AccessGateFor("client-desk", "overview", "deskOverview", []string{"desk"}) == nil {
			t.Error("the new load did not publish its gate")
		}
		for _, dropped := range []struct {
			deployable, gate, construct, domain string
		}{
			{"client-desk", "dashboard", "deskBriefs", "desk"},
			{"client-desk", "dashboard", "deskNote", "desk"},
			{"client-desk", "dashboard", "deskBriefs", "partner"},
			{"shipping-desk", "desk", "shippingDeskBriefs", "shipping"},
		} {
			if AccessGateFor(dropped.deployable, dropped.gate, dropped.construct, []string{dropped.domain}) != nil {
				t.Errorf("%s/%s/%s of %s survived a load that no longer declares it",
					dropped.deployable, dropped.gate, dropped.construct, dropped.domain)
			}
		}
	})

	t.Run("a load with no gated construct empties it", func(t *testing.T) {
		if problems := loadGateBundle(t, gateBundleDomains(map[string]string{
			"desk": gateQuery("", "deskPlain", codeArgs, codeFilter),
		}), true); len(problems) != 0 {
			t.Fatalf("the plain load was refused: %v", problems)
		}
		if got := AccessGatesFor("client-desk", []string{"desk", "partner"}); len(got) != 0 {
			t.Errorf("a bundle with no @accessGate left gates open: %+v", got)
		}
		if got := AccessGateEntries(); len(got) != 0 {
			t.Errorf("a bundle with no @accessGate left entries behind: %+v", got)
		}
	})
}

// The registry hands out CLONES, so a field Function.clone does not name is a
// field every registered construct loses. A declaration that parsed, validated
// and then vanished from the clone would be a gate that opens nothing, and the
// only symptom would be a visitor's 404.
func TestAccessGateClonePreservesTheDeclaration(t *testing.T) {
	t.Run("a hand-built function", func(t *testing.T) {
		original := gatedFunction("deskBriefs", "query", "client-desk", "dashboard")
		clone := original.clone()
		if clone.AccessGate == nil || *clone.AccessGate != *original.AccessGate {
			t.Fatalf("the clone lost the declaration: %+v", clone.AccessGate)
		}
		clone.AccessGate.Gate = "tampered"
		if original.AccessGate.Gate != "dashboard" {
			t.Error("the clone shares its declaration with the original")
		}
	})

	t.Run("a registered construct", func(t *testing.T) {
		tree := withLanguageLines(fstest.MapFS{
			"desk/concepts.memql":   {Data: []byte(gateConceptSource)},
			"desk/constructs.memql": {Data: []byte(gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter))},
		})
		concepts, _, err := BuildUnifiedConcepts(discardLogger(), tree)
		if err != nil {
			t.Fatal(err)
		}
		functions := newFunctionRegistry()
		if _, _, err := loadUnifiedFunctionsFromTree(tree, discardLogger(), functions, memoryNodes.NewRegistry(concepts), newLoadReport()); err != nil {
			t.Fatal(err)
		}
		fn, ok := functions.Lookup("deskBriefs")
		if !ok {
			t.Fatal("the construct did not register")
		}
		if fn.AccessGate == nil || fn.AccessGate.Deployable != "client-desk" || fn.AccessGate.Gate != "dashboard" {
			t.Fatalf("the registry's copy lost the declaration: %+v", fn.AccessGate)
		}
		listed := functions.List()
		if len(listed) != 1 || listed[0].AccessGate == nil {
			t.Fatalf("List() hands out constructs without the declaration: %+v", listed)
		}
	})
}

// ---------------------------------------------------------------------------
// THE ENGINE'S WIRING, END TO END
// ---------------------------------------------------------------------------

// The rules above run the loaders by hand. This one runs the engine's own
// boot-grade validation over a candidate package, which is where a deploy
// meets them: a fault must come out as a diagnostic that refuses the deploy,
// a clean bundle must come out empty, and neither may publish.
func TestAccessGateFaultsRefuseAPackageDeploy(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	shapes := "@row\nshape deskBrief deskBriefFull { }\n"
	bundle := func(construct string) fstest.MapFS {
		return withLanguageLines(fstest.MapFS{
			"candidategate/concepts.memql": {Data: []byte(gateConceptSource)},
			"candidategate/shapes.memql":   {Data: []byte(shapes)},
			"candidategate/queries.memql":  {Data: []byte(construct)},
		})
	}

	t.Run("a fault is a diagnostic", func(t *testing.T) {
		result, err := AnalyzePackageDSL(discardLogger(),
			bundle(gateQuery(gateDashboard+"\n@serverOnly", "deskBriefs", codeArgs, codeFilter)))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range result.Diagnostics {
			found = found || (strings.Contains(d.Message, "deskBriefs") && strings.Contains(d.Message, "@serverOnly"))
		}
		if !found {
			t.Fatalf("package analysis does not report the fault: %+v", result.Diagnostics)
		}
	})

	t.Run("a clean bundle is clean, and publishes nothing", func(t *testing.T) {
		result, err := AnalyzePackageDSL(discardLogger(),
			bundle(gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter)))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Diagnostics) != 0 {
			t.Fatalf("a clean bundle was refused: %+v", result.Diagnostics)
		}
		if AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{"candidategate"}) != nil {
			t.Error("analysing a candidate opened its gate on the live node")
		}
	})
}

// ---------------------------------------------------------------------------
// THE RESULT CACHE
// ---------------------------------------------------------------------------

// Requirement R5 is that a code stops working the moment it expires, is
// regenerated or is removed. The engine's default result cache would answer a
// read of either concept from memory for up to a minute, so both are on the
// denylist; the prefix covers the pair without naming them one by one.
func TestSiteAccessConceptsAreNotDefaultCached(t *testing.T) {
	for _, concept := range []string{"v1:platform:siteAccessCode", "v1:platform:siteAccessSession"} {
		if !isCacheDenylistedConcept(concept) {
			t.Errorf("%s can be served from the default result cache; a revoked code would keep opening the site", concept)
		}
	}
	if !anyConceptCacheDenylisted([]string{"v1:cognition:space", "v1:platform:siteAccessCode"}) {
		t.Error("a read that depends on an access code is cacheable")
	}
	// The prefix must not swallow the site's own reads, which are cached and
	// whose staleness is harmless.
	for _, concept := range []string{"v1:platform:site", "v1:platform:siteBinding", "v1:platform:sitePreviewGrant"} {
		if isCacheDenylistedConcept(concept) {
			t.Errorf("%s is denylisted; the access prefix is too loose", concept)
		}
	}
}
