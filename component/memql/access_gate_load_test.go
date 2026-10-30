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

// gateOpens is one thing a bundle that loads clean must open.
type gateOpens struct{ deployable, gate, construct, kind string }

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
			name: "two constructs named alike under one deployable",
			domains: map[string]string{
				// Different gates on purpose: the call names the construct and
				// not the gate, so the rule is about the name under the
				// deployable, whichever gate each one opens.
				"desk":     gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
				"deskmore": gateQuery(gateOverview, "deskBriefs", codeArgs, codeFilter),
			},
			refused: []string{"deskBriefs", "desk", "deskmore", `"client-desk"`, "Rename one of them"},
		},

		// ---- accepted ------------------------------------------------------
		{
			name: "a query and a mutation under one gate",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter) +
				gateMutation(gateDashboard, "deskNote", codeArgs+"    title string!\n",
					"      accessCodeId: args.accessCodeId\n")},
			opens: []gateOpens{
				{"client-desk", "dashboard", "deskBriefs", "query"},
				{"client-desk", "dashboard", "deskNote", "mutation"},
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
				{"client-desk", "dashboard", "deskBriefs", "query"},
				{"shipping-desk", "dashboard", "deskBriefs", "query"},
			},
		},
		{
			name: "a reserved argument declared as an optional string",
			domains: map[string]string{"desk": gateQuery(gateDashboard, "deskBriefs",
				codeArgs+"    accessSubject string\n", codeFilter+` && row.accessSubject == args.accessSubject`)},
			opens: []gateOpens{{"client-desk", "dashboard", "deskBriefs", "query"}},
		},
		{
			// A disabled construct is out of service, so it opens nothing and
			// cannot collide with a live one that shares its name.
			name: "a disabled construct sharing a live one's name",
			domains: map[string]string{
				"desk":     gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
				"deskmore": gateQuery(gateOverview+"\n@disabled", "deskBriefs", codeArgs, codeFilter),
			},
			opens: []gateOpens{{"client-desk", "dashboard", "deskBriefs", "query"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := loadGateBundle(t, gateBundleDomains(tc.domains), true)

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
				got := AccessGateFor(want.deployable, want.gate, want.construct)
				if got == nil {
					t.Errorf("%s/%s/%s opens nothing", want.deployable, want.gate, want.construct)
					continue
				}
				if got.Kind != want.kind {
					t.Errorf("%s/%s/%s Kind = %q, want %q", want.deployable, want.gate, want.construct, got.Kind, want.kind)
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
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs"); got != nil {
		t.Fatalf("a refused construct opens a gate anyway: %+v", got)
	}
	if got := AccessGatesFor("client-desk"); len(got) != 0 {
		t.Fatalf("a refused construct is listed as a gate: %+v", got)
	}
}

// Both of two same-named constructs are refused, and neither opens. Keeping
// whichever loaded first would let load order decide which construct a code
// reaches, and nothing resolves load order.
func TestAccessGateTwoConstructsWithOneNameBothStayClosed(t *testing.T) {
	problems := loadGateBundle(t, gateBundleDomains(map[string]string{
		"desk":     gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
		"deskmore": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter),
	}), true)
	if len(problems) != 1 {
		t.Fatalf("want one problem naming both, got %v", problems)
	}
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs"); got != nil {
		t.Fatalf("one of two colliding constructs opens a gate: %+v", got)
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
// declares a caller argument and a code, a second gate, and another
// deployable's gate that must stay out of this one's answers.
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
	})
}

func TestAccessGateRegistryAnswers(t *testing.T) {
	if problems := loadGateBundle(t, registryBundle(), true); len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}

	t.Run("a hit carries the declaration", func(t *testing.T) {
		got := AccessGateFor("client-desk", "dashboard", "deskBriefs")
		if got == nil {
			t.Fatal("a declared gate opens nothing")
		}
		want := AccessGateEntry{
			Deployable: "client-desk", Gate: "dashboard", Construct: "deskBriefs", Kind: "query",
			Args:    []string{"topic"},
			Stamped: []string{"accessCodeId", "accessSubject"},
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("entry = %+v\nwant   %+v", *got, want)
		}
	})

	t.Run("Args exclude the reserved names and Stamped lists them", func(t *testing.T) {
		note := AccessGateFor("client-desk", "dashboard", "deskNote")
		if note == nil {
			t.Fatal("the mutation opens nothing")
		}
		if note.Kind != "mutation" || !reflect.DeepEqual(note.Args, []string{"title"}) ||
			!reflect.DeepEqual(note.Stamped, []string{"accessCodeId"}) {
			t.Fatalf("mutation entry = %+v", *note)
		}
		overview := AccessGateFor("client-desk", "overview", "deskOverview")
		if overview == nil || len(overview.Args) != 0 || !reflect.DeepEqual(overview.Stamped, []string{"accessCodeId"}) {
			t.Fatalf("a construct with no caller argument has Args = %+v", overview)
		}
	})

	t.Run("it misses, and a miss is nil", func(t *testing.T) {
		for _, miss := range []struct{ name, deployable, gate, construct string }{
			{"the wrong deployable", "shipping-desk", "dashboard", "deskBriefs"},
			{"the wrong gate", "client-desk", "overview", "deskBriefs"},
			{"an unknown construct", "client-desk", "dashboard", "deskMissing"},
			{"another deployable's construct", "client-desk", "desk", "shippingDeskBriefs"},
			{"a different case", "client-desk", "dashboard", "deskbriefs"},
			{"a padded name", "client-desk", "dashboard", " deskBriefs"},
			{"nothing at all", "", "", ""},
		} {
			if got := AccessGateFor(miss.deployable, miss.gate, miss.construct); got != nil {
				t.Errorf("%s: opened %+v", miss.name, got)
			}
		}
	})

	t.Run("a gate lists its constructs and whether it uses a subject", func(t *testing.T) {
		got := AccessGatesFor("client-desk")
		want := []AccessGateSummary{
			{Gate: "dashboard", UsesSubject: true, Constructs: []string{"deskBriefs", "deskNote"}},
			{Gate: "overview", UsesSubject: false, Constructs: []string{"deskOverview"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("gates = %+v\nwant    %+v", got, want)
		}
		if other := AccessGatesFor("shipping-desk"); len(other) != 1 || other[0].Gate != "desk" {
			t.Errorf("another deployable's gates = %+v", other)
		}
		if none := AccessGatesFor("nobody-here"); len(none) != 0 {
			t.Errorf("an unknown deployable has gates: %+v", none)
		}
		if none := AccessGatesFor(""); len(none) != 0 {
			t.Errorf("an empty deployable has gates: %+v", none)
		}
	})

	t.Run("what comes back is a copy", func(t *testing.T) {
		got := AccessGateFor("client-desk", "dashboard", "deskBriefs")
		got.Args[0], got.Stamped[0] = "tampered", "tampered"
		again := AccessGateFor("client-desk", "dashboard", "deskBriefs")
		if again.Args[0] != "topic" || again.Stamped[0] != "accessCodeId" {
			t.Fatalf("a caller rewrote the registry through a returned entry: %+v", again)
		}
		gates := AccessGatesFor("client-desk")
		gates[0].Constructs[0] = "tampered"
		if AccessGatesFor("client-desk")[0].Constructs[0] != "deskBriefs" {
			t.Fatal("a caller rewrote the registry through a returned summary")
		}
	})
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
	if AccessGateFor("client-desk", "dashboard", "deskBriefs") == nil {
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
		if AccessGateFor("client-desk", "candidate", "deskCandidate") != nil {
			t.Error("validating a candidate opened its gate on the live node")
		}
		if AccessGateFor("client-desk", "dashboard", "deskBriefs") == nil {
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
		if AccessGateFor("client-desk", "overview", "deskOverview") == nil {
			t.Error("the new load did not publish its gate")
		}
		for _, dropped := range []struct{ deployable, gate, construct string }{
			{"client-desk", "dashboard", "deskBriefs"},
			{"client-desk", "dashboard", "deskNote"},
			{"shipping-desk", "desk", "shippingDeskBriefs"},
		} {
			if AccessGateFor(dropped.deployable, dropped.gate, dropped.construct) != nil {
				t.Errorf("%s/%s/%s survived a load that no longer declares it", dropped.deployable, dropped.gate, dropped.construct)
			}
		}
	})

	t.Run("a load with no gated construct empties it", func(t *testing.T) {
		if problems := loadGateBundle(t, gateBundleDomains(map[string]string{
			"desk": gateQuery("", "deskPlain", codeArgs, codeFilter),
		}), true); len(problems) != 0 {
			t.Fatalf("the plain load was refused: %v", problems)
		}
		if got := AccessGatesFor("client-desk"); len(got) != 0 {
			t.Errorf("a bundle with no @accessGate left gates open: %+v", got)
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
		if AccessGateFor("client-desk", "dashboard", "deskBriefs") != nil {
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
