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
  filter row.id==args.id
  shape probeCard
}`},
	{"struct query: sort + paginate", true, `query thing probe {
  filter row.id!=""
  sort "row.createdAt", "desc"
  paginate 25
}`},
	{"struct query: count", true, `query thing probe {
  filter row.id!=""
  count
}`},
	{"struct query: asOf with the ?? latest fallback", true, `query thing probe {
  args {
    at string
  }
  filter row.id!=""
  asOf args.at ?? latest
}`},
	{"struct mutation: insert", true, `mutate thing probe {
  args {
    id string @required
  }
  insert {
    id: args.id
    createdAt: now
  }
}`},
	{"struct mutation: update", true, `mutate thing probe {
  args {
    id string @required
  }
  update {
    id: args.id
    updatedAt: now
  }
}`},
	{"struct mutation: accept + stamp sugar", true, `mutate thing probe {
  args {
    name string @required
  }
  accept { name }
  stamp { createdAt: now }
}`},
	{"logic: args + body + return", true, `logic probe {
  args {
    x string @required
  }
  body {
    return x
  }
}`},
	{"spec: bare return", true, `spec thing probe {
  return active == true
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
  filter row.id!=""
  asOf args.at
}`},
	{"retired expression builtin `year()` (93b365ed, memql#2707)", false, `logic probe {
  args {
    at string
  }
  body {
    return year(at)
  }
}`},
	{"inline `concept` line in a struct query", false, `query thing probe {
  concept v1:probe:thing
  filter row.id!=""
}`},
	{"unknown struct-query clause", false, `query thing probe {
  filter row.id!=""
  project name
}`},
	{"named write block `insert <Concept> { }` (memql#988)", false, `mutate thing probe {
  args {
    id string @required
  }
  insert Probe {
    id: args.id
  }
}`},
	{"two write blocks in one mutation", false, `mutate thing probe {
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
