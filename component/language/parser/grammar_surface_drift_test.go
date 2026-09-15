package parser

// grammar_surface_drift_test.go -- memql#3089.
//
// The mechanism that makes an unbumped grammar narrowing IMPOSSIBLE TO LAND
// GREEN, replacing the four-word denylist that could not.
//
// # What was wrong with the thing this replaces
//
// TestGrammarFingerprintPinned hashes InvocationKindKeywords() -- the
// invocation-kind keyword set -- against a hand-pinned literal. Two defects,
// both measured on PR #3134:
//
//  1. It cannot see a NEW CLAUSE. Adding a `project` clause to
//     parseStructQueryBody -- a real change to what authors may write -- moves
//     no invocation keyword, so the fingerprint is unchanged and the test is
//     green with GrammarVersion untouched.
//  2. Re-pinning the literal restores green with the version UNBUMPED. So even
//     when it did fire, the cheapest way past it was to edit the pin, which is
//     how the constant sat unmoved across six grammar moves in both directions.
//
// # The mechanism here
//
// There is NO hand-pinned literal to edit. The surface digest is COMPUTED, and
// GrammarVersion is required to END WITH IT:
//
//	GrammarVersion = "<year>.<month>-<epic-slug>-<8 hex surface digest>"
//
// Change the authored surface and the digest changes; the suffix no longer
// matches; the only edit that restores green is to GrammarVersion itself --
// which IS the bump. Defect 2 is closed structurally rather than by discipline,
// because there is nothing else to re-pin.
//
// Making the bump mandatory only became affordable because memql#3089 also
// inverted the stamp guard (component/memql/authoring_promote_durable.go):
// recompile is attempted FIRST and a stale stamp merely explains a failure, so a
// bump no longer unregisters durable rows whose source is still valid. Under the
// old ordering, "bump on every surface change" would have meant "lose every
// stored construct on every surface change", which is the real reason the
// contract was ignored.
//
// # What the digest covers
//
// Three arms, because narrowings and additions are not visible in the same
// place:
//
//   - BEHAVIOURAL: a corpus of authored snippets, each observed to parse or to
//     be rejected. A narrowing flips accept -> reject even when NOTHING IN TREE
//     uses the form -- which is the case for five of the six moves this issue
//     lists, and the reason "no in-tree usage" is not evidence of safety.
//   - STRUCTURAL: the clause literals of the struct-body parsers, read out of
//     rewriter.go's own `case` arms. This is the arm that catches a new clause,
//     by name, without anyone having to predict the name.
//   - KEYWORDS: InvocationKindKeywords(), the surface the old fingerprint
//     covered. Kept so nothing regresses relative to it.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// grammarSurfaceCorpus is the behavioural arm: authored snippets and the
// outcome each one currently has.
//
// `accept` is the observed outcome, and it is also asserted per-entry so that a
// flip reports WHICH authored form changed rather than only that a hash moved.
// Every retired form listed in memql#3089's narrowing table has an entry, so the
// six moves that went unrecorded cannot happen silently a seventh time.
//
// memqlmigrate:keep-file -- a retired spelling here is a corpus entry whose
// refusal is the recorded fact; the fixture codemod rewriting it in place
// would turn a narrowing into an unchanged digest, which is how a grammar move
// lands unrecorded.
var grammarSurfaceCorpus = []struct {
	name   string
	accept bool
	src    string
}{
	// ---- currently legal forms -------------------------------------------
	{"concept decl", true, `concept probe {
  a string
}`},
	{"struct query: args + filter + shape", true, `query thing probe {
  args {
    id string @required
  }
  filter row => row.id == args.id
  shape probeCard
}`},
	{"struct query: sort + paginate", true, `query thing probe {
  filter row => row.id != ""
  sort "row.createdAt", "desc"
  paginate 25
}`},
	{"struct query: count", true, `query thing probe {
  filter row => row.id != ""
  count
}`},
	{"struct query: asOf with the ?? latest fallback", true, `query thing probe {
  args {
    at string
  }
  filter row => row.id != ""
  asOf args.at ?? latest
}`},
	{"struct mutation: insert", true, `mutation thing probe {
  args {
    id string @required
  }
  insert {
    id: args.id
    createdAt: now
  }
}`},
	{"struct mutation: update", true, `mutation thing probe {
  args {
    id string @required
  }
  update {
    id: args.id
    updatedAt: now
  }
}`},
	{"struct mutation: accept + stamp sugar", true, `mutation thing probe {
  args {
    name string @required
  }
  accept { name }
  stamp { createdAt: now }
}`},
	{"logic: args + statements + return", true, `logic probe {
  args {
    x string!
  }
  return args.x
}`},
	{"shape: @row path list", true, `@row
shape probe {
  row.id
  name
}`},
	{"doc comment above a declaration", true, `/// A probe concept.
concept probe {
  a string
}`},
	// Edition-2026 predicate positions (memql#5364): since the flip, the only
	// spellings of a filter, a spec or trait body, and an @filter.
	{"struct query: v1 lambda filter", true, `query thing probe {
  args {
    id string @required
  }
  filter row => row.id == args.id && isX(row)
}`},
	{"struct query: refine over paginate", true, `query thing probe {
  filter row => row.id != ""
  paginate 25
  refine row => row.title.includes("x")
}`},
	{"spec: = lambda form", true, `spec thing isProbe = row => row.a == 1`},
	{"trait: = lambda form", true, `trait isProbe = row => row.active == true`},
	{"automation: @filter lambda", true, `@filter(row => row.status == "x")
@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  logic probe(x: 1)
}`},
	{"struct query: refine without paginate", false, `query thing probe {
  filter row => row.id != ""
  refine row => row.title.includes("x")
}`},
	// Edition-2026 in-process positions (memql#5364, the flip): a logic
	// statement, a mutation value, and a call's arguments and an if's
	// condition parse the v1 grammar -- `+` joins strings, `??` and
	// `p ? a : b` choose, a method reads a collection, a named argument is
	// `name: value`, and an assignment's right-hand side may be any
	// expression, a literal included.
	{"logic: edition-2026 statements", true, `logic probe {
  args {
    items []object!
    name string
  }
  head := args.items.first()
  label := args.name ?? "none"
  note := head != nil ? "has " + label : label
  return query probeQuery(label: note)
}`},
	{"logic: a literal assignment right-hand side", true, `logic probe {
  args {
    x string!
  }
  n := 5
  return args.x + n
}`},
	{"struct mutation: edition-2026 values", true, `mutation thing probe {
  args {
    id string @required
    name string
  }
  insert {
    id: "thing-" + args.id
    name: args.name ?? "unnamed"
  }
}`},
	{"automation: an if block with named arguments", true, `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  args {
    kind any
    x    any
  }
  if args.x != nil && !(args.kind in ["a", "b"]) {
    logic probe(x: args.x)
  }
}`},
	// The edition-2026 statement body (memql#5370): every construct kind is
	// called by name as a statement of its own, a loop filters with a
	// trailing `if`, a switch takes literal case labels, a parallel's
	// branches join at the block, and the trailing clauses come after the
	// call they qualify.
	{"automation: every call kind as a statement", true, `automation probe {
  rows := query activeThings(status: "active")
  written := mutation touchThing(id: rows.first().id)
  decided := logic decide(n: rows.count())
  sized := builtin measure(rows: rows)
  sub := automation sweep(limit: 10)
  released := action applyRelease(version: "1.2.3")
}`},
	{"automation: for, switch and parallel blocks", true, `automation probe {
  args {
    items []object!
    mode  string
  }
  for item in args.items if item.active == true {
    mutation touchThing(id: item.id)
  }
  switch args.mode {
    case "a", "b" {
      logic handleA(x: 1)
    }
    default {
      logic handleD(x: 2)
    }
  }
  parallel {
    branch left {
      builtin measure(side: "left")
    }
    branch right {
      builtin measure(side: "right")
    }
  } wait any
}`},
	{"automation: retry, on error and publish", true, `automation probe {
  fetched := query activeThings(status: "active") retry(3) on error continue
  if fetched != nil {
    publish "probe.fetched" { count: fetched.count() }
  }
}`},

	// ---- the narrowings memql#3089 records --------------------------------
	// Each of these PARSED under some earlier grammar epoch and does not now.
	// An entry flipping back to accept is a widening; a NEW entry flipping from
	// accept to reject is a narrowing. Either way the digest moves.
	{"repeated annotation argument (83574995, memql#2968)", false,
		"@relationship(type=\"parent\", field=\"a\", field=\"b\")\nconcept probe {\n  a string\n}"},
	{"bare `asOf args.X` without ?? latest (6e7d09ac added, memql#3028/#3085 removed)", false, `query thing probe {
  args {
    at string
  }
  filter row => row.id != ""
  asOf args.at
}`},
	{"retired expression builtin `year()` (93b365ed, memql#2707)", false, `logic probe {
  args {
    at string
  }
  return year(args.at)
}`},
	{"inline `concept` line in a struct query", false, `query thing probe {
  concept v1:probe:thing
  filter row => row.id != ""
}`},
	{"unknown struct-query clause", false, `query thing probe {
  filter row => row.id != ""
  project name
}`},
	{"named write block `insert <Concept> { }` (memql#988)", false, `mutation thing probe {
  args {
    id string @required
  }
  insert Probe {
    id: args.id
  }
}`},
	{"two write blocks in one mutation", false, `mutation thing probe {
  args {
    id string @required
  }
  insert {
    id: args.id
  }
  update {
    id: args.id
  }
}`},
	// ---- the annotation registry's parse-time narrowings (memql#5359) -----
	// The registry became the one annotation gate, at PARSE time, for every
	// construct and field list. Each of these parsed on this path before it:
	// the field lists accepted any annotation, the four function kinds were
	// checked only by a load-time text scan of their names, an action had no
	// check at all, and no check looked at an annotation's arguments.
	{"unknown annotation on a prompt field (memql#5359)", false, `prompt probe {
  topic string @bogusFieldAnnotation
}`},
	{"unknown annotation on a builtin field (memql#5359)", false, `builtin probe {
  topic string @bogusFieldAnnotation
}`},
	{"a flag annotation given an argument (memql#5359)", false, `@serverOnly("yes")
query thing probe {
  filter row => row.id != ""
}`},
	{"unknown annotation on a query, refused at parse (memql#5359)", false, `@bogusAnnotation
query thing probe {
  filter row => row.id != ""
}`},
	{"unknown annotation on an action (memql#5359)", false, `@bogusAnnotation
action probe {
  capability script(script: "x")
}`},
	{"unknown keyword key on @trigger (memql#5359)", false, `@trigger(evnt="node.created")
automation probe {
  mutation createThing(id: "x")
}`},
	{"a non-repeatable annotation written twice (memql#5359)", false, `@description("one")
@description("two")
query thing probe {
  filter row => row.id != ""
}`},
	{"@when() with empty parentheses on a rule", true, `@when()
@policy("localFirst")
rule probe { }`},
	// A keyword key written in the shape its placement does not take: the
	// parser stores a bare key as `true`, so a valued key written bare read
	// as "" downstream -- this tool registered with no rate limit.
	{"a valued keyword key written bare (memql#5359)", false, `@handler(type="function", name="x")
@rateLimit(maxCalls, periodSeconds)
tool probe {
  x string
}`},
	{"the @trigger(on=...) synonym for event= (memql#5359)", true, `@trigger(on=participant.created)
automation probe {
  mutation createThing(id: "x")
}`},
	// Logic, automation and mutation bodies refuse a clause they do not
	// take; each emitter kept what it recognised and dropped the rest.
	{"an unlisted clause in an automation body (memql#5359)", false, `automation probe {
  filter row.id != ""
  mutation createThing(id: "x")
}`},
	{"an unlisted block in a logic body (memql#5359)", false, `logic probe {
  precondition ready {
    check: 1 == 1
  }
  return 1
}`},
	{"a stray top-level line in a mutation body (memql#5359)", false, `mutation thing probe {
  insert {
    id: "x"
  }
  filter row.id != ""
}`},
	// The named block written without its name, and an unnamed block written
	// twice.
	{"a precondition block without its name (memql#5359)", false, `automation probe {
  precondition {
    check: 1 == 1
  }
  mutation createThing(id: "y")
}`},
	{"a second args block in a logic body (memql#5359)", false, `logic probe {
  args {
    x string
  }
  args {
    y string
  }
  return args.x
}`},

	// ---- the edition-2026 flip (memql#5364, memql#5368) --------------------
	// Legal until the tree's migration made edition 2026 the only authoring
	// grammar; each is now refused naming its replacement and
	// memqlmigrate --rewrite=expressions, and V1RetiredForms is the full list.
	// The first five were this corpus's own legal entries, in the spellings
	// the entries of the same name above replaced.
	{"struct query: args + filter + shape, legacy filter (retired_filter_without_lambda)", false, `query thing probe {
  args {
    id string @required
  }
  filter row.id==args.id
  shape probeCard
}`},
	{"struct query: sort + paginate, legacy filter (retired_filter_without_lambda)", false, `query thing probe {
  filter row.id!=""
  sort "row.createdAt", "desc"
  paginate 25
}`},
	{"struct query: count, legacy filter (retired_filter_without_lambda)", false, `query thing probe {
  filter row.id!=""
  count
}`},
	{"struct query: asOf ?? latest, legacy filter (retired_filter_without_lambda)", false, `query thing probe {
  args {
    at string
  }
  filter row.id!=""
  asOf args.at ?? latest
}`},
	{"spec: bare return", false, `spec thing probe {
  return active == true
}`},
	{"trait: `{ return ... }` body (retired_trait_return_body)", false, `trait probe {
  return active == true
}`},
	{"automation: raw-text @filter (retired_filter_annotation)", false, `@filter(payload.status == "x")
@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  logic probe(x: 1)
}`},
	{"automation: @trigger filter= string (retired_filter_annotation)", false, `@trigger(event="node.created", concept="v1:probe:thing", filter="payload.status == 1")
automation probe {
  logic probe(x: 1)
}`},
	{"logic: cond(p, a, b) (retired_cond_call)", false, `logic probe {
  args {
    x string!
  }
  return cond(args.x == "a", 1, 2)
}`},
	{"logic: coalesce(a, b) (retired_coalesce_call)", false, `logic probe {
  args {
    x string
  }
  return coalesce(args.x, "none")
}`},
	{"logic: first(x) (retired_first_call)", false, `logic probe {
  args {
    items []object!
  }
  return first(args.items)
}`},
	{"logic: null (retired_null)", false, `logic probe {
  args {
    x string
  }
  return args.x == null
}`},
	{"struct mutation: concat(a, b) value (retired_concat_call)", false, `mutation thing probe {
  args {
    id string @required
  }
  insert {
    id: concat("thing-", args.id)
  }
}`},
	{"struct mutation: a quoted map key", false, `mutation thing probe {
  args {
    id string @required
  }
  insert {
    id: args.id
    meta: {"source": "probe"}
  }
}`},
	{"automation: a name=value call argument", false, `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  args {
    x any
  }
  logic probe(x=args.x)
}`},

	// ---- the edition-2026 bodies (memql#5370) ------------------------------
	// Legal until the tree's migration made the statement body the only
	// body grammar; each is now refused by the statement parser, by name,
	// naming its replacement and memqlmigrate --rewrite=bodies.
	// memqlmigrate:keep -- the retired declaration verb is the case.
	{"a mutation declared with the retired verb `mutate` (construct_unknown)", false, `mutate thing probe {
  insert {
    id: "x"
  }
}`},
	{"logic: a `body { }` block (body_block_retired)", false, `logic probe {
  args {
    x string!
  }
  body {
    return args.x
  }
}`},
	{"automation: a `step` block (body_step_retired)", false, `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step run {
    logic probe(x: 1)
  }
}`},
	{"automation: the terse header (body_terse_retired)", false, `automation probe @trigger(event="node.created", concept="v1:probe:thing") @filter(row => row.a != nil) => logic probe`},
	{"automation: a steps.<id> read (body_steps_reference_retired)", false, `automation probe {
  first := query activeThings(status: "active")
  mutation touchThing(id: steps.first.result)
}`},
	{"automation: forEach (body_foreach_retired)", false, `automation probe {
  args {
    items []object!
  }
  forEach item in args.items {
    mutation touchThing(id: item.id)
  }
}`},
	{"automation: for x := range (body_for_range_retired)", false, `automation probe {
  args {
    items []object!
  }
  for item := range args.items {
    mutation touchThing(id: item.id)
  }
}`},
	{"automation: x := if (body_conditional_assign_retired)", false, `automation probe {
  args {
    ready bool
  }
  x := if args.ready {
    query activeThings(status: "active")
  }
}`},
	{"automation: publishEvent(...) (body_publish_event_retired)", false, `automation probe {
  publishEvent(topic: "probe.done", payload: {})
}`},
	{"automation: a call with no kind (body_call_kind_missing)", false, `automation probe {
  touchThing(id: "x")
}`},
	{"automation: the argument pun (body_positional_argument)", false, `automation probe {
  args {
    event any
  }
  logic probe(event)
}`},
	{"automation: @trigger partition= (trigger_partition_retired)", false, `@trigger(event="node.created", concept="v1:probe:thing", partition="*")
automation probe {
  logic probe(x: 1)
}`},
	{"automation: @schedule (trigger_schedule_synonym_retired)", false, `@schedule(cron="0 * * * * *")
automation probe {
  logic probe(x: 1)
}`},
	// The step bodies' accessors parsed as calls to functions nothing
	// defines, and were refused only at load; they are refused by name now.
	{"logic: the step(\"x\") accessor (body_accessor_retired)", false, `logic probe {
  first := query activeThings(status: "active")
  return step("first")
}`},
	{"logic: the error() accessor (body_accessor_retired)", false, `logic probe { return error() }`},
	{"logic: error with a message remains a catalog function", true, `logic probe { return error("failed") }`},
	{"automation: the input() accessor (body_accessor_retired)", false, `automation probe {
  mutation touchThing(id: input())
}`},
	{"automation: the item() accessor (body_accessor_retired)", false, `automation probe {
  args {
    items []object!
  }
  for x in args.items {
    mutation touchThing(id: item())
  }
}`},
	{"logic: the index() accessor (body_accessor_retired)", false, `logic probe {
  args {
    items []object!
  }
  for x in args.items {
    n := index()
  }
  return 0
}`},

	// NOT in this corpus: the retired procedural `func (Query) name(ctx any)`
	// author-side form. It is refused, but NOT by NormaliseAll + ParseFile --
	// measured here, it parses clean at this layer, so an entry asserting
	// `accept: false` would be recording a fact about a different component
	// while looking like a parser-surface fact. The corpus states only what
	// this path actually decides.
}

// grammarSurfaceOutcome runs one corpus entry through the authored-source path:
// NormaliseAll (the struct-form rewriter) then ParseFile. Accepted means both
// succeed -- which is what "an author may write this" means.
func grammarSurfaceOutcome(src string) bool {
	normalised, err := NormaliseAll(src)
	if err != nil {
		return false
	}
	_, err = ParseFile(normalised)
	return err == nil
}

// structClauseSurface is the structural arm: the clause literals the
// struct-body parsers accept, read out of their own `case` arms in rewriter.go.
//
// Source-derived rather than restated, because a restated list is a denylist
// with extra steps -- it can only contain clauses someone thought to write down,
// and the clause nobody wrote down is precisely the one that lands unbumped.
// Reading the `case` arms means a new arm shows up here the moment it is added,
// by whatever name its author chose.
func structClauseSurface(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("rewriter.go")
	if err != nil {
		t.Fatalf("read rewriter.go: %v", err)
	}
	src := string(raw)

	litRE := regexp.MustCompile(`"([^"\\]*)"`)
	var out []string
	for _, fn := range []string{"parseStructQueryBody", "parseStructMutationBody"} {
		body := functionSource(t, src, fn)
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "case ") {
				continue
			}
			for _, m := range litRE.FindAllStringSubmatch(line, -1) {
				if m[1] != "" {
					out = append(out, fn+":"+m[1])
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// functionSource slices one top-level func's body out of a Go source file, from
// its `func <name>(` header to the next line that is exactly `}`.
func functionSource(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "\nfunc "+name+"(")
	if start < 0 {
		t.Fatalf("cannot find func %s in rewriter.go -- the structural arm of the grammar surface digest is reading nothing, so it would silently stop detecting a new clause. Update the function list in structClauseSurface.", name)
	}
	rest := src[start+1:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// grammarSurfaceDigest is the computed surface fingerprint: 8 bytes of sha256
// over the three arms, in a stable order.
func grammarSurfaceDigest(t *testing.T) string {
	t.Helper()
	var lines []string
	for _, e := range grammarSurfaceCorpus {
		outcome := "reject"
		if grammarSurfaceOutcome(e.src) {
			outcome = "accept"
		}
		lines = append(lines, "behaviour\t"+e.name+"\t"+outcome)
	}
	for _, c := range structClauseSurface(t) {
		lines = append(lines, "clause\t"+c)
	}
	kws := InvocationKindKeywords()
	sort.Strings(kws)
	for _, k := range kws {
		lines = append(lines, "keyword\t"+k)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:4])
}

// TestGrammarVersionCarriesTheSurfaceDigest is the gate. It fails when the
// authored surface changes without a GrammarVersion bump, and there is no pin to
// edit instead.
func TestGrammarVersionCarriesTheSurfaceDigest(t *testing.T) {
	digest := grammarSurfaceDigest(t)
	if !strings.HasSuffix(GrammarVersion, "-"+digest) {
		t.Fatalf("the authored grammar surface has changed and GrammarVersion was not bumped.\n"+
			"  computed surface digest: %s\n"+
			"  GrammarVersion:          %q\n"+
			"\n"+
			"  GrammarVersion must end with `-<digest>`. Bump it in grammar_version.go to\n"+
			"  \"<year>.<month>-<epic-slug>-%s\" naming the epic that changed the surface.\n"+
			"  Nothing else here is editable -- the digest is computed, not pinned, which is\n"+
			"  the point (memql#3089: re-pinning a literal is how six grammar moves landed\n"+
			"  with the constant untouched).\n"+
			"\n"+
			"  Run TestGrammarSurfaceCorpusOutcomes -v to see WHICH authored form changed.\n"+
			"  On whether a `memqlmigrate --rewrite` mode is required, see the contract note\n"+
			"  in grammar_version.go: a narrowing with no in-tree usage and no durable\n"+
			"  stored-row exposure needs none.", digest, GrammarVersion, digest)
	}
}

// TestGrammarSurfaceCorpusOutcomes asserts each corpus entry individually, so a
// flip names the authored form rather than only moving a hash. This is the
// diagnostic half of the gate above.
func TestGrammarSurfaceCorpusOutcomes(t *testing.T) {
	for _, e := range grammarSurfaceCorpus {
		t.Run(e.name, func(t *testing.T) {
			got := grammarSurfaceOutcome(e.src)
			if got == e.accept {
				return
			}
			if e.accept {
				t.Errorf("this form USED to be legal and is now rejected -- a NARROWING.\n" +
					"  If that is intended: bump GrammarVersion, flip this entry's `accept` to false,\n" +
					"  and record the move in grammar_version.go's narrowing list.\n" +
					"  If it is not: the change that broke it is a regression against authored source.")
				return
			}
			t.Errorf("this form USED to be rejected and now parses -- a WIDENING (a new clause, a\n" +
				"  revived annotation, a relaxed rule).\n" +
				"  If that is intended: bump GrammarVersion and flip this entry's `accept` to true.\n" +
				"  A widening needs no migration mode (nothing previously-valid stopped being\n" +
				"  valid), but it is still a grammar epoch.")
		})
	}
}

// TestGrammarSurfaceArmsAreLive is the coverage tripwire on the digest itself. A
// digest computed over three arms that have quietly gone empty is stable,
// meaningless, and green -- which is the shape of every failure in this area
// (memql#3043, and the denylist this file replaces).
func TestGrammarSurfaceArmsAreLive(t *testing.T) {
	if len(grammarSurfaceCorpus) < 15 {
		t.Errorf("behavioural arm has only %d entries -- it must cover every recorded narrowing plus the legal forms", len(grammarSurfaceCorpus))
	}
	accepted, rejected := 0, 0
	for _, e := range grammarSurfaceCorpus {
		if e.accept {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Errorf("the behavioural arm must contain BOTH accepted and rejected forms (got %d accepted, %d rejected) -- one-sided, it cannot detect a flip in the other direction", accepted, rejected)
	}

	clauses := structClauseSurface(t)
	if len(clauses) < 8 {
		t.Errorf("structural arm found only %d clause literals: %v\n  This arm is what catches a NEW CLAUSE. If it is reading nothing, the digest cannot notice one being added.", len(clauses), clauses)
	}
	// Anchored on clauses that must exist for the arm to be reading the right
	// switch at all.
	for _, want := range []string{
		"parseStructQueryBody:filter",
		"parseStructQueryBody:shape",
		"parseStructMutationBody:insert",
		"parseStructMutationBody:update",
	} {
		found := false
		for _, c := range clauses {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("structural arm is missing %q -- it is not reading the struct-body clause switch. Found: %v", want, clauses)
		}
	}
	if len(InvocationKindKeywords()) == 0 {
		t.Error("keyword arm is empty")
	}
}
