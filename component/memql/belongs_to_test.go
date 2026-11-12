package memql

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// belongs_to_test.go -- the half of @belongsTo that needs every concept at
// once: whether a `via` chain is sound, the order the engine-managed concepts
// are assigned in, and the report of concepts that declared nothing.
//
// Every tree here is built through BuildUnifiedConcepts, the loader a node
// boots with, rather than from hand-made Concept values: what these checks read
// is a relationship's RESOLVED target, and only the loader resolves one (a bare
// `target=goal` becomes `v1:<domain>:goal` there). A hand-made fixture would
// pin the checks to a spelling no real concept ever carries.

// belongsToConcepts builds every concept the given files declare and returns
// them sorted by name. Each key is a path in a DSL tree (`desk/concepts.memql`);
// every domain gets the engine's own language line.
func belongsToConcepts(t *testing.T, files map[string]string) []*memorynodes.Concept {
	t.Helper()
	tree := fstest.MapFS{}
	for path, src := range files {
		tree[path] = &fstest.MapFile{Data: []byte(src)}
	}
	built, skips, err := BuildUnifiedConcepts(nil, withLanguageLines(tree))
	if err != nil {
		t.Fatalf("BuildUnifiedConcepts: %v", err)
	}
	if len(skips) > 0 {
		t.Fatalf("the fixture does not build, so the test would measure a smaller tree than it wrote: %v", skips[0])
	}
	out := make([]*memorynodes.Concept, 0, len(built))
	for _, c := range built {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// belongsToProblemCodes returns the rule code of each error, requiring each to
// be the coded refusal the load report keys on.
func belongsToProblemCodes(t *testing.T, problems []error) []string {
	t.Helper()
	codes := make([]string, 0, len(problems))
	for _, problem := range problems {
		var refusal *langparser.BelongsToError
		if !errors.As(problem, &refusal) {
			t.Fatalf("problem is %T, want *BelongsToError: %v", problem, problem)
		}
		if !strings.HasSuffix(problem.Error(), " ["+refusal.Code+"]") {
			t.Fatalf("the refusal does not end with its code in brackets: %q", problem)
		}
		codes = append(codes, refusal.Code)
	}
	return codes
}

const (
	// A goal whose rows belong to an account, and a run that follows its goal
	// through a field carrying a relationship.
	belongsToGoal = "@belongsTo(account)\nconcept goal {\n  accountId  string\n  title      string\n}\n"
	belongsToRun  = "@belongsTo(account, via=\"goalId\")\nconcept run {\n  accountId  string\n  goalId     string\n\n" +
		"  @relationship(type=\"parent\", field=\"goalId\", target=goal, direction=\"outgoing\")\n}\n"
	belongsToStep = "@belongsTo(account, via=\"runId\")\nconcept step {\n  accountId  string\n  runId      string\n\n" +
		"  @relationship(type=\"parent\", field=\"runId\", target=run, direction=\"outgoing\")\n}\n"
)

// ---------------------------------------------------------------------
// accountParentConcept

func TestBelongsToParentConceptIsWhatAViaFieldPointsAt(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": belongsToGoal + "\n" + belongsToRun + "\n" +
			// A field with no relationship, its concept written out.
			"@belongsTo(account, via=\"storeId\", parent=\"v1:desk:goal\")\nconcept note {\n  accountId  string\n  storeId    string\n}\n\n" +
			// A field with no relationship and no parent: an index row's
			// reference, holding canonical ids of several concepts.
			"@belongsTo(account, via=\"sourceRef\")\nconcept indexRow {\n  accountId  string\n  sourceRef  string\n}\n\n" +
			// A declared parent wins over the field's relationship: it is what
			// the author wrote about THIS declaration.
			"@belongsTo(account, via=\"goalId\", parent=\"v1:desk:run\")\nconcept override {\n  accountId  string\n  goalId     string\n\n" +
			"  @relationship(type=\"references\", field=\"goalId\", target=goal, direction=\"outgoing\")\n}\n\n" +
			// Two relationships on one field naming two concepts: the field
			// holds ids of more than one concept, so no single concept is the
			// parent and the id decides at write time.
			"@belongsTo(account, via=\"subjectId\")\nconcept twoTargets {\n  accountId  string\n  subjectId  string\n\n" +
			"  @relationship(type=\"parent\", field=\"subjectId\", target=goal, direction=\"outgoing\")\n" +
			"  @relationship(type=\"references\", field=\"subjectId\", target=run, direction=\"outgoing\")\n}\n\n" +
			// Two relationships on one field naming ONE concept agree.
			"@belongsTo(account, via=\"goalId\")\nconcept twoEdges {\n  accountId  string\n  goalId     string\n\n" +
			"  @relationship(type=\"parent\", field=\"goalId\", target=goal, direction=\"outgoing\")\n" +
			"  @relationship(type=\"references\", as=\"plannedUnder\", field=\"goalId\", target=goal, direction=\"outgoing\")\n}\n\n" +
			"@belongsTo(account, stated)\nconcept campaign {\n  accountId  string\n}\n\n" +
			"@belongsTo(cluster)\nconcept node {\n  hostname  string\n}\n\n" +
			"concept undeclared {\n  title  string\n}\n",
	})
	want := map[string]string{
		"v1:desk:goal":       "",
		"v1:desk:run":        "v1:desk:goal",
		"v1:desk:note":       "v1:desk:goal",
		"v1:desk:indexRow":   "",
		"v1:desk:override":   "v1:desk:run",
		"v1:desk:twoTargets": "",
		"v1:desk:twoEdges":   "v1:desk:goal",
		"v1:desk:campaign":   "",
		"v1:desk:node":       "",
		"v1:desk:undeclared": "",
	}
	if len(concepts) != len(want) {
		t.Fatalf("built %d concepts, want %d", len(concepts), len(want))
	}
	for _, c := range concepts {
		if got := accountParentConcept(c); got != want[c.Name] {
			t.Errorf("accountParentConcept(%s) = %q, want %q", c.Name, got, want[c.Name])
		}
	}
	if got := accountParentConcept(nil); got != "" {
		t.Errorf("accountParentConcept(nil) = %q, want empty", got)
	}
}

// An INCOMING relationship's field belongs to the far concept: `hub` declaring
// field="hubId" incoming from `spoke` says spoke rows carry hubId. A `via` is a
// field of THIS row, so an incoming edge that happens to spell the same field
// name says nothing about what this row's field points at.
func TestBelongsToParentConceptIgnoresAnIncomingRelationship(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(account, via=\"hubId\")\nconcept hub {\n  accountId  string\n  hubId      string\n\n" +
			"  @relationship(type=\"parent\", field=\"hubId\", target=spoke, direction=\"incoming\")\n}\n\n" +
			"@belongsTo(account)\nconcept spoke {\n  accountId  string\n  hubId      string\n}\n",
	})
	for _, c := range concepts {
		if c.Name != "v1:desk:hub" {
			continue
		}
		if got := accountParentConcept(c); got != "" {
			t.Fatalf("accountParentConcept(hub) = %q: an incoming relationship named the far concept's field, not this row's", got)
		}
		return
	}
	t.Fatal("the fixture did not build v1:desk:hub")
}

// ---------------------------------------------------------------------
// validateBelongsToAcrossConcepts

func TestBelongsToViaAClusterWideParentIsRefused(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(cluster)\nconcept goal {\n  title  string\n}\n\n" + belongsToRun,
	})
	problems := validateBelongsToAcrossConcepts(concepts)
	if codes := belongsToProblemCodes(t, problems); !reflect.DeepEqual(codes, []string{langparser.CodeBelongsToViaCluster}) {
		t.Fatalf("codes = %v, want one %s\n%v", codes, langparser.CodeBelongsToViaCluster, problems)
	}
	msg := problems[0].Error()
	for _, want := range []string{`"v1:desk:run"`, `@belongsTo(account, via="goalId")`, `"v1:desk:goal"`, "@belongsTo(cluster)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must say %s: %s", want, msg)
		}
	}
}

// The same refusal when the parent is named with parent= rather than through a
// relationship: what is refused is the concept the field points at, however the
// declaration came to know it.
func TestBelongsToViaADeclaredClusterWideParentIsRefused(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(cluster)\nconcept store {\n  shop  string\n}\n\n" +
			"@belongsTo(account, via=\"storeId\", parent=\"v1:desk:store\")\nconcept review {\n  accountId  string\n  storeId    string\n}\n",
	})
	problems := validateBelongsToAcrossConcepts(concepts)
	if codes := belongsToProblemCodes(t, problems); !reflect.DeepEqual(codes, []string{langparser.CodeBelongsToViaCluster}) {
		t.Fatalf("codes = %v, want one %s\n%v", codes, langparser.CodeBelongsToViaCluster, problems)
	}
}

func TestBelongsToParentNamingNoConceptIsRefused(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(account, via=\"storeId\", parent=\"v1:nowhere:store\")\n" +
			"concept review {\n  accountId  string\n  storeId    string\n}\n",
	})
	problems := validateBelongsToAcrossConcepts(concepts)
	if codes := belongsToProblemCodes(t, problems); !reflect.DeepEqual(codes, []string{langparser.CodeBelongsToParentUnknown}) {
		t.Fatalf("codes = %v, want one %s\n%v", codes, langparser.CodeBelongsToParentUnknown, problems)
	}
	msg := problems[0].Error()
	for _, want := range []string{`"v1:desk:review"`, `parent="v1:nowhere:store"`, "not a registered concept"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must say %s: %s", want, msg)
		}
	}
}

// A parent written as a bare name is the mistake an author who knows
// @relationship makes: `target=store` is resolved through an import there, and
// `parent=` is a concept id. The refusal has to say which, or the author is
// left comparing two spellings that look alike.
func TestBelongsToParentWrittenAsABareNameSaysToWriteTheId(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(account)\nconcept store {\n  accountId  string\n}\n\n" +
			"@belongsTo(account, via=\"storeId\", parent=\"store\")\nconcept review {\n  accountId  string\n  storeId    string\n}\n",
	})
	problems := validateBelongsToAcrossConcepts(concepts)
	if codes := belongsToProblemCodes(t, problems); !reflect.DeepEqual(codes, []string{langparser.CodeBelongsToParentUnknown}) {
		t.Fatalf("codes = %v, want one %s\n%v", codes, langparser.CodeBelongsToParentUnknown, problems)
	}
	if msg := problems[0].Error(); !strings.Contains(msg, `"v1:desk:store"`) {
		t.Errorf("the refusal does not name the concept id the bare name was reaching for: %s", msg)
	}
}

func TestBelongsToViaCycleIsRefusedOncePerCycle(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantPath string
	}{
		{
			name: "two concepts following each other",
			src: "@belongsTo(account, via=\"bId\")\nconcept a {\n  accountId  string\n  bId        string\n\n" +
				"  @relationship(type=\"references\", field=\"bId\", target=b, direction=\"outgoing\")\n}\n\n" +
				"@belongsTo(account, via=\"aId\")\nconcept b {\n  accountId  string\n  aId        string\n\n" +
				"  @relationship(type=\"references\", field=\"aId\", target=a, direction=\"outgoing\")\n}\n",
			wantPath: "v1:desk:a -> v1:desk:b -> v1:desk:a",
		},
		{
			// A concept following ITSELF is a cycle of one. The concept-level
			// chain names no row that has an account of its own to start from,
			// and the assignment of existing rows orders CONCEPTS, so it could
			// not put a parent row before its child inside one.
			name: "a concept following itself",
			src: "@belongsTo(account, via=\"parentId\")\nconcept folder {\n  accountId  string\n  parentId   string\n\n" +
				"  @relationship(type=\"parent\", field=\"parentId\", target=folder, direction=\"outgoing\")\n}\n",
			wantPath: "v1:desk:folder -> v1:desk:folder",
		},
		{
			// Named through parent= rather than a relationship, and entered from
			// a concept OUTSIDE the loop: the tail is not part of the cycle and
			// the refusal must not say it is.
			name: "three concepts, entered from a fourth",
			src: "@belongsTo(account, via=\"nextId\", parent=\"v1:desk:y\")\nconcept x {\n  accountId  string\n  nextId     string\n}\n\n" +
				"@belongsTo(account, via=\"nextId\", parent=\"v1:desk:z\")\nconcept y {\n  accountId  string\n  nextId     string\n}\n\n" +
				"@belongsTo(account, via=\"nextId\", parent=\"v1:desk:x\")\nconcept z {\n  accountId  string\n  nextId     string\n}\n\n" +
				"@belongsTo(account, via=\"nextId\", parent=\"v1:desk:x\")\nconcept tail {\n  accountId  string\n  nextId     string\n}\n",
			wantPath: "v1:desk:x -> v1:desk:y -> v1:desk:z -> v1:desk:x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			concepts := belongsToConcepts(t, map[string]string{"desk/concepts.memql": tc.src})
			problems := validateBelongsToAcrossConcepts(concepts)
			if codes := belongsToProblemCodes(t, problems); !reflect.DeepEqual(codes, []string{langparser.CodeBelongsToViaCycle}) {
				t.Fatalf("codes = %v, want one %s\n%v", codes, langparser.CodeBelongsToViaCycle, problems)
			}
			if msg := problems[0].Error(); !strings.Contains(msg, tc.wantPath) {
				t.Errorf("the refusal must walk the cycle as %q: %s", tc.wantPath, msg)
			}
			if strings.Contains(problems[0].Error(), "v1:desk:tail") {
				t.Errorf("the refusal names a concept that only leads INTO the cycle: %s", problems[0])
			}

			if order, err := engineManagedOrder(concepts); err == nil {
				t.Fatalf("engineManagedOrder ordered a cycle: %v", order)
			} else if codes := belongsToProblemCodes(t, []error{err}); codes[0] != langparser.CodeBelongsToViaCycle {
				t.Fatalf("engineManagedOrder error = %v, want %s", err, langparser.CodeBelongsToViaCycle)
			}
		})
	}
}

// What is NOT a problem. An undeclared parent is the row to read twice: its
// rows count as cluster-wide until it declares, but "nobody has classified this
// concept yet" is not "its author said it is never inside an account", and
// refusing the child for it would make a bundle unloadable the moment the
// concept it follows fell behind on a declaration.
func TestBelongsToViaChainsThatAreSound(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "a parent that declares nothing, through a relationship",
			files: map[string]string{
				"desk/concepts.memql": "concept goal {\n  title  string\n}\n\n" + belongsToRun,
			},
		},
		{
			name: "a parent that declares nothing, named with parent=",
			files: map[string]string{
				"desk/concepts.memql": "concept store {\n  shop  string\n}\n\n" +
					"@belongsTo(account, via=\"storeId\", parent=\"v1:desk:store\")\nconcept review {\n  accountId  string\n  storeId    string\n}\n",
			},
		},
		{
			name:  "a three-level chain",
			files: map[string]string{"desk/concepts.memql": belongsToGoal + "\n" + belongsToRun + "\n" + belongsToStep},
		},
		{
			name: "a parent in another domain, imported",
			files: map[string]string{
				"plans/concepts.memql": belongsToGoal,
				"desk/concepts.memql":  "use plans.concepts.{ goal }\n\n" + belongsToRun,
			},
		},
		{
			name: "a field with no relationship and no parent",
			files: map[string]string{
				"desk/concepts.memql": "@belongsTo(account, via=\"sourceRef\")\nconcept indexRow {\n  accountId  string\n  sourceRef  string\n}\n",
			},
		},
		{
			name: "a parent whose own writers state the account",
			files: map[string]string{
				"desk/concepts.memql": "@belongsTo(account, stated)\nconcept goal {\n  accountId  string\n}\n\n" + belongsToRun,
			},
		},
		{
			name: "a parent that holds cluster-provided rows",
			files: map[string]string{
				"desk/concepts.memql": "@belongsTo(account, shared)\nconcept goal {\n  accountId  string\n}\n\n" + belongsToRun,
			},
		},
		{
			name: "nothing follows anything",
			files: map[string]string{
				"desk/concepts.memql": belongsToGoal + "\n@belongsTo(cluster)\nconcept node {\n  hostname  string\n}\n\nconcept plain {\n  title  string\n}\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			concepts := belongsToConcepts(t, tc.files)
			if problems := validateBelongsToAcrossConcepts(concepts); len(problems) != 0 {
				t.Fatalf("a sound tree drew %d problem(s): %v", len(problems), problems)
			}
			if _, err := engineManagedOrder(concepts); err != nil {
				t.Fatalf("engineManagedOrder: %v", err)
			}
		})
	}
	if problems := validateBelongsToAcrossConcepts(nil); len(problems) != 0 {
		t.Fatalf("no concepts drew problems: %v", problems)
	}
}

// Every problem in a tree is reported, in an order that does not depend on the
// order the concepts arrive in -- a registry lists them from a map, and one
// broken declaration naming a different concept on each boot reads as a flaky
// load rather than as one mistake.
func TestBelongsToProblemsAreAllReportedInAStableOrder(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": "@belongsTo(cluster)\nconcept goal {\n  title  string\n}\n\n" + belongsToRun + "\n" +
			"@belongsTo(account, via=\"storeId\", parent=\"v1:nowhere:store\")\nconcept review {\n  accountId  string\n  storeId    string\n}\n\n" +
			"@belongsTo(account, via=\"parentId\", parent=\"v1:desk:folder\")\nconcept folder {\n  accountId  string\n  parentId   string\n}\n",
	})
	want := []string{langparser.CodeBelongsToViaCycle, langparser.CodeBelongsToParentUnknown, langparser.CodeBelongsToViaCluster}
	sort.Strings(want)

	first := validateBelongsToAcrossConcepts(concepts)
	codes := belongsToProblemCodes(t, first)
	sorted := append([]string(nil), codes...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(sorted, want) {
		t.Fatalf("codes = %v, want each of %v once\n%v", codes, want, first)
	}

	reversed := make([]*memorynodes.Concept, len(concepts))
	for i, c := range concepts {
		reversed[len(concepts)-1-i] = c
	}
	second := validateBelongsToAcrossConcepts(reversed)
	if len(second) != len(first) {
		t.Fatalf("the reversed registry drew %d problem(s), want %d", len(second), len(first))
	}
	for i := range first {
		if first[i].Error() != second[i].Error() {
			t.Fatalf("problem %d depends on the order the concepts arrive in:\n %s\n %s", i, first[i], second[i])
		}
	}
}

// ---------------------------------------------------------------------
// engineManagedOrder

func TestBelongsToEngineManagedOrderPutsParentsBeforeChildren(t *testing.T) {
	concepts := belongsToConcepts(t, map[string]string{
		"desk/concepts.memql": belongsToStep + "\n" + belongsToRun + "\n" + belongsToGoal,
	})
	order, err := engineManagedOrder(concepts)
	if err != nil {
		t.Fatalf("engineManagedOrder: %v", err)
	}
	if want := []string{"v1:desk:goal", "v1:desk:run", "v1:desk:step"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// The whole rule in one tree: only the concepts whose account the ENGINE
// decides are listed; a concept comes after the concept its rows follow; a
// concept whose parent can be any concept -- the field carries no relationship
// and names no parent, so the id decides at write time -- comes after every
// concept it could be following, and what follows IT comes after it; and
// concepts the rule does not separate are in name order.
func TestBelongsToEngineManagedOrderIsTheAssignmentOrder(t *testing.T) {
	src := belongsToGoal + "\n" + belongsToRun + "\n" + belongsToStep + "\n" +
		// Roots, to be ordered by name around `goal`.
		"@belongsTo(account)\nconcept alpha {\n  accountId  string\n}\n\n" +
		"@belongsTo(account, shared)\nconcept zeta {\n  accountId  string\n}\n\n" +
		// Not the engine's to assign: stated, cluster-wide, undeclared.
		"@belongsTo(account, stated)\nconcept campaign {\n  accountId  string\n}\n\n" +
		"@belongsTo(account, shared, stated)\nconcept role {\n  accountId  string\n}\n\n" +
		"@belongsTo(cluster)\nconcept node {\n  hostname  string\n}\n\n" +
		"concept undeclared {\n  title  string\n}\n\n" +
		// Follows a STATED concept: the parent's rows are not assigned by the
		// engine, so nothing has to come first and this is a root.
		"@belongsTo(account, via=\"campaignId\", parent=\"v1:desk:campaign\")\nconcept delivery {\n  accountId   string\n  campaignId  string\n}\n\n" +
		// Follows an UNDECLARED concept: a root for the same reason.
		"@belongsTo(account, via=\"plainId\", parent=\"v1:desk:undeclared\")\nconcept follower {\n  accountId  string\n  plainId    string\n}\n\n" +
		// An index row: its parent is whatever concept the id names.
		"@belongsTo(account, via=\"sourceRef\")\nconcept artifact {\n  accountId  string\n  sourceRef  string\n}\n\n" +
		// Follows the index row, so it comes after it -- and therefore last.
		"@belongsTo(account, via=\"artifactId\", parent=\"v1:desk:artifact\")\nconcept comment {\n  accountId   string\n  artifactId  string\n}\n\n" +
		// A second concept with an unresolved parent, to be ordered by name.
		"@belongsTo(account, via=\"rowRef\")\nconcept outboxEntry {\n  accountId  string\n  rowRef     string\n}\n"

	concepts := belongsToConcepts(t, map[string]string{"desk/concepts.memql": src})
	order, err := engineManagedOrder(concepts)
	if err != nil {
		t.Fatalf("engineManagedOrder: %v", err)
	}
	want := []string{
		// Depth 0, by name.
		"v1:desk:alpha", "v1:desk:delivery", "v1:desk:follower", "v1:desk:goal", "v1:desk:zeta",
		// Depth 1, then 2.
		"v1:desk:run",
		"v1:desk:step",
		// An unresolved parent: after everything they could follow, by name.
		"v1:desk:artifact", "v1:desk:outboxEntry",
		// What follows one of those.
		"v1:desk:comment",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order =\n  %v\nwant\n  %v", order, want)
	}

	// The same answer whatever order the registry lists the concepts in.
	reversed := make([]*memorynodes.Concept, len(concepts))
	for i, c := range concepts {
		reversed[len(concepts)-1-i] = c
	}
	again, err := engineManagedOrder(reversed)
	if err != nil {
		t.Fatalf("engineManagedOrder (reversed): %v", err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("the order depends on the order the concepts arrive in:\n  %v", again)
	}

	if order, err := engineManagedOrder(nil); err != nil || len(order) != 0 {
		t.Fatalf("engineManagedOrder(nil) = %v, %v; want nothing and no error", order, err)
	}
}

// ---------------------------------------------------------------------
// belongsToDeclFor

func TestBelongsToDeclForReadsTheRegisteredDeclaration(t *testing.T) {
	const declared, plain = "v1:belongstofixture:declared", "v1:belongstofixture:plain"
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		declared: {Name: declared, NodeType: "fixture", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Via: "goalId"}},
		plain:    {Name: plain, NodeType: "fixture"},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	got := belongsToDeclFor(" " + declared + " ")
	if got == nil || got.Scope != langparser.BelongsToAccount || got.Via != "goalId" {
		t.Fatalf("belongsToDeclFor(%s) = %+v, want the registered declaration", declared, got)
	}
	// Undeclared, unregistered and unnamed all answer nil: none of them has
	// said which account a row belongs to, and nil is what every caller reads
	// as "nothing to stamp".
	for _, name := range []string{plain, "v1:belongstofixture:missing", "", "   "} {
		if got := belongsToDeclFor(name); got != nil {
			t.Errorf("belongsToDeclFor(%q) = %+v, want nil", name, got)
		}
	}
}

// ---------------------------------------------------------------------
// The report of concepts that declared nothing

func TestUndeclaredBelongsToNamesOnlyMountedDomains(t *testing.T) {
	concepts := []*memorynodes.Concept{
		// Mounted (not core) and undeclared: reported.
		{Name: "v1:acmebundle:zebra"},
		{Name: "v1:acmebundle:ant"},
		// A nested namespace is its top-level domain's.
		{Name: "v1:acmebundle/sub:widget"},
		// Mounted and declared, every way there is: not reported.
		{Name: "v1:acmebundle:node", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToCluster}},
		{Name: "v1:acmebundle:note", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount}},
		{Name: "v1:acmebundle:role", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Shared: true, Stated: true}},
		// Core and undeclared: the build's own gate answers for the engine's
		// tree, so the load does not repeat it on every boot of every node.
		{Name: "v1:cluster:belongsToProbe"},
		{Name: "v1:library:belongsToProbe"},
		nil,
	}
	want := []string{"v1:acmebundle/sub:widget", "v1:acmebundle:ant", "v1:acmebundle:zebra"}
	if got := undeclaredBelongsTo(concepts); !reflect.DeepEqual(got, want) {
		t.Fatalf("undeclaredBelongsTo = %v, want %v", got, want)
	}
	if got := undeclaredBelongsTo(nil); len(got) != 0 {
		t.Fatalf("undeclaredBelongsTo(nil) = %v, want nothing", got)
	}
}

// One WARN line naming every undeclared concept and the annotation to add, and
// the gauge set to their count. One line rather than one per concept for the
// reason the row-authz report gives: the list is the actionable part, and a
// line per concept at every boot of every node is how a signal gets filtered
// out of a log and stops being read.
func TestWarnUndeclaredBelongsToLogsOnceAndSetsTheGauge(t *testing.T) {
	concepts := []*memorynodes.Concept{
		{Name: "v1:acmebundle:zebra"},
		{Name: "v1:acmebundle:ant"},
		{Name: "v1:acmebundle:note", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount}},
		{Name: "v1:cluster:belongsToProbe"},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	metrics.BelongsToUndeclared(99) // a stale value from an earlier load
	warnUndeclaredBelongsTo(logger, concepts, true)

	if got := metrics.BelongsToUndeclaredValue(); got != 2 {
		t.Fatalf("memql_dsl_belongs_to_undeclared = %v, want 2", got)
	}
	out := buf.String()
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", n, out)
	}
	for _, want := range []string{
		`"level":"WARN"`, `"count":2`, "v1:acmebundle:ant, v1:acmebundle:zebra",
		"@belongsTo(account)", "@belongsTo(cluster)", "cluster-wide until",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the line must carry %s: %s", want, out)
		}
	}
	for _, absent := range []string{"v1:acmebundle:note", "v1:cluster:belongsToProbe"} {
		if strings.Contains(out, absent) {
			t.Errorf("the line names %s, which is declared or the engine's own: %s", absent, out)
		}
	}

	// A tree with nothing undeclared says nothing and takes the gauge to zero:
	// a gauge that only ever rose would keep reporting a bundle that has since
	// declared.
	buf.Reset()
	warnUndeclaredBelongsTo(logger, concepts[2:], true)
	if got := metrics.BelongsToUndeclaredValue(); got != 0 {
		t.Fatalf("memql_dsl_belongs_to_undeclared = %v after a clean load, want 0", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("a clean tree logged: %s", buf.String())
	}
}

// A candidate validation (a package deploy's analysis, the linter) loads a tree
// that is not the node's, so it must not move the node's gauge: the series says
// what THIS node loaded. It still logs, to the logger it was handed.
func TestWarnUndeclaredBelongsToLeavesTheGaugeAloneWhenNotPublishing(t *testing.T) {
	metrics.BelongsToUndeclared(7)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	warnUndeclaredBelongsTo(logger, []*memorynodes.Concept{{Name: "v1:acmebundle:zebra"}}, false)
	if got := metrics.BelongsToUndeclaredValue(); got != 7 {
		t.Fatalf("a candidate load moved the gauge to %v", got)
	}
	if !strings.Contains(buf.String(), "v1:acmebundle:zebra") {
		t.Fatalf("the candidate load did not log its undeclared concept: %s", buf.String())
	}
	// And a nil logger is a caller with nowhere to log, not a panic.
	warnUndeclaredBelongsTo(nil, []*memorynodes.Concept{{Name: "v1:acmebundle:zebra"}}, true)
	if got := metrics.BelongsToUndeclaredValue(); got != 1 {
		t.Fatalf("memql_dsl_belongs_to_undeclared = %v, want 1", got)
	}
}

// ---------------------------------------------------------------------
// The scan the linter runs

func TestConceptsDeclaringNoBelongsToFindsEachByNameAndLine(t *testing.T) {
	src := "use identity.concepts.{ user }\n\n" + // lines 1-2
		"/// Declared.\n@belongsTo(account)\nconcept note {\n  accountId  string\n}\n\n" + // 3-8
		"@description(\"A plain row.\")\nconcept plain {\n  title  string\n}\n\n" + // 9-13
		"@belongsTo(cluster)\nconcept node {\n  hostname  string\n}\n\n" + // 14-18
		"concept bare {\n  title  string\n}\n\n" + // 19-22
		// A query is not a concept, whatever it is called.
		"query note belongsToNothing {\n  filter row => row.id != \"\"\n}\n"
	got := ConceptsDeclaringNoBelongsTo(src)
	// The line is the line of the `concept` keyword, which is where an author
	// goes to add the annotation -- not the first line of its preamble.
	want := []UndeclaredBelongsToConcept{{Name: "plain", Line: 10}, {Name: "bare", Line: 19}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConceptsDeclaringNoBelongsTo = %+v, want %+v", got, want)
	}

	// A concept that does not parse declares nothing this scan can read; the
	// load refuses it by name (concept_unparsed), and a second finding about
	// the same concept would send its author to fix the wrong thing first.
	broken := "concept broken {\n  title  string  @@description(\"x\")\n}\n\nconcept fine {\n  title  string\n}\n"
	if got, want := ConceptsDeclaringNoBelongsTo(broken), []UndeclaredBelongsToConcept{{Name: "fine", Line: 5}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConceptsDeclaringNoBelongsTo(broken) = %+v, want %+v", got, want)
	}
	if got := ConceptsDeclaringNoBelongsTo("trait isOpen = row => row.status == \"open\"\n"); len(got) != 0 {
		t.Fatalf("a file with no concept drew %+v", got)
	}
}

// ---------------------------------------------------------------------
// Through the load

// The three cross-concept refusals are recorded on the load report as coded
// skips, which is what makes strict boot refuse the tree and what a package
// deploy's analysis reads. AnalyzePackageDSL runs the engine's own Init over
// the candidate and returns the report, publishing nothing.
func TestInitRefusesAnUnsoundBelongsToChain(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantCode string
		wantText string
	}{
		{
			name:     "via a cluster-wide parent",
			src:      "@belongsTo(cluster)\nconcept goal {\n  title  string\n}\n\n" + belongsToRun,
			wantCode: langparser.CodeBelongsToViaCluster,
			wantText: `concept "v1:belongstoload:run"`,
		},
		{
			name: "a parent naming no concept",
			src: "@belongsTo(account, via=\"storeId\", parent=\"v1:nowhere:store\")\n" +
				"concept review {\n  accountId  string\n  storeId    string\n}\n",
			wantCode: langparser.CodeBelongsToParentUnknown,
			wantText: `concept "v1:belongstoload:review"`,
		},
		{
			name: "a cycle",
			src: "@belongsTo(account, via=\"bId\", parent=\"v1:belongstoload:b\")\nconcept a {\n  accountId  string\n  bId        string\n}\n\n" +
				"@belongsTo(account, via=\"aId\", parent=\"v1:belongstoload:a\")\nconcept b {\n  accountId  string\n  aId        string\n}\n",
			wantCode: langparser.CodeBelongsToViaCycle,
			wantText: "v1:belongstoload:a -> v1:belongstoload:b -> v1:belongstoload:a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := AnalyzePackageDSL(discardLogger(), withLanguageLines(fstest.MapFS{
				"belongstoload/concepts.memql": &fstest.MapFile{Data: []byte(tc.src)},
			}))
			if err != nil {
				t.Fatalf("AnalyzePackageDSL: %v", err)
			}
			var matched []string
			for _, d := range result.Diagnostics {
				if strings.Contains(d.Message, "["+tc.wantCode+"]") {
					matched = append(matched, d.Message)
				}
			}
			if len(matched) != 1 {
				t.Fatalf("want exactly one %s diagnostic, got %d of %d:\n%+v", tc.wantCode, len(matched), len(result.Diagnostics), result.Diagnostics)
			}
			if !strings.Contains(matched[0], tc.wantText) || !strings.Contains(matched[0], "contract-gate:belongsTo") {
				t.Errorf("the diagnostic must carry %q and its phase: %s", tc.wantText, matched[0])
			}
			if !strings.HasSuffix(matched[0], " ["+tc.wantCode+"]") {
				t.Errorf("the diagnostic does not end with its code: %s", matched[0])
			}
		})
	}
}

// The decision itself: a tree whose chain is unsound REFUSES TO BOOT, the
// refusal names the rule code, and the operator break-glass is the only way
// past it. The test above reads the report a package deploy reads; this one
// reads the error a node's boot returns, built the way AnalyzePackageDSL builds
// its engine -- a registry of its own over a private view of the tree, so the
// process's live registry is never touched.
func TestStrictBootRefusesAnUnsoundBelongsToChain(t *testing.T) {
	root := withLanguageLines(fstest.MapFS{
		"belongstostrict/concepts.memql": &fstest.MapFile{Data: []byte(
			"@belongsTo(cluster)\nconcept goal {\n  title  string\n}\n\n" + belongsToRun)},
	})
	boot := func() error {
		tree, _, _, err := memqldsl.CandidateTree(root)
		if err != nil {
			t.Fatalf("CandidateTree: %v", err)
		}
		concepts, skips, err := BuildUnifiedConcepts(discardLogger(), tree)
		if err != nil || len(skips) > 0 {
			t.Fatalf("the fixture's concepts do not build on their own (err=%v, skips=%v); "+
				"the refusal under test is the one only the whole tree can make", err, skips)
		}
		eng, err := newEngine(nil, false, (&component.Component{}).WithLoggerWriter(io.Discard))
		if err != nil {
			t.Fatalf("newEngine: %v", err)
		}
		eng.Logger = discardLogger()
		return eng.initFromTree(memorynodes.NewRegistry(concepts), tree, false)
	}

	t.Setenv(AllowSkipsEnvVar, "")
	err := boot()
	if err == nil {
		t.Fatal("a tree whose row follows a cluster-wide parent booted")
	}
	for _, want := range []string{
		"strict DSL boot refused",
		`concept "v1:belongstostrict:run"`,
		"contract-gate:belongsTo",
		"[" + langparser.CodeBelongsToViaCluster + "]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the boot refusal must carry %q:\n%v", want, err)
		}
	}

	// The break-glass boots it, as it boots past any other skip. That is the
	// operator's decision to take; the check has still said what is wrong.
	t.Setenv(AllowSkipsEnvVar, "1")
	if err := boot(); err != nil {
		t.Fatalf("%s=1 did not boot past the refusal: %v", AllowSkipsEnvVar, err)
	}
}

// Positive control for the test above, and the property the corpus depends on:
// a sound chain, an undeclared parent and an undeclared concept all load with
// NOTHING on the report -- no refusal and no warning. The undeclared concept is
// the one that matters: it is reported in the log and on the gauge only,
// because a warning on the report would turn every load that is clean today
// into one that warns.
func TestInitLoadsSoundAndUndeclaredBelongsToWithNothingOnTheReport(t *testing.T) {
	src := belongsToGoal + "\n" + belongsToRun + "\n" + belongsToStep + "\n" +
		"concept plain {\n  title  string\n}\n\n" +
		"@belongsTo(account, via=\"plainId\", parent=\"v1:belongstoclean:plain\")\nconcept follower {\n  accountId  string\n  plainId    string\n}\n"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	result, err := AnalyzePackageDSL(logger, withLanguageLines(fstest.MapFS{
		"belongstoclean/concepts.memql": &fstest.MapFile{Data: []byte(src)},
	}))
	if err != nil {
		t.Fatalf("AnalyzePackageDSL: %v", err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("a sound tree drew diagnostics: %+v", result.Diagnostics)
	}
	// The undeclared concept reached the log the load was handed.
	if !strings.Contains(buf.String(), "v1:belongstoclean:plain") || !strings.Contains(buf.String(), "@belongsTo") {
		t.Fatalf("the load did not report the undeclared concept in its log:\n%s", buf.String())
	}
}
