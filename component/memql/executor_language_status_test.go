package memql

// executor_language_status_test.go -- the languageStatus builtin (memql#5390):
// the MemQL line this cluster speaks, the forms the language deprecates, and
// where the DSL the answering node loaded still spells one. The read behind
// MemQL OS's Settings -> Language.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/deprecation"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// languageStatusRow is the reply as a CLIENT decodes it. Decoded from the
// node's JSON rather than read off the Go struct, so a renamed json tag fails
// here exactly as it would in the OS.
type languageStatusRow struct {
	Language                string                  `json:"language"`
	Edition                 string                  `json:"edition"`
	Status                  string                  `json:"status"`
	GrammarVersion          string                  `json:"grammarVersion"`
	EditorRelease           string                  `json:"editorRelease"`
	DeprecationWindowMinors int                     `json:"deprecationWindowMinors"`
	Forms                   []languageStatusFormRow `json:"forms"`
}

type languageStatusFormRow struct {
	Rule         string                 `json:"rule"`
	Spelling     string                 `json:"spelling"`
	Replacement  string                 `json:"replacement"`
	Migrator     string                 `json:"migrator"`
	DeprecatedIn string                 `json:"deprecatedIn"`
	RefusedFrom  string                 `json:"refusedFrom"`
	State        string                 `json:"state"`
	Uses         []languageStatusUseRow `json:"uses"`
}

type languageStatusUseRow struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// oneLanguageStatusNode unwraps a top-level builtin's reply -- the handler's
// id-keyed node map -- and insists on exactly the one row the builtin answers.
func oneLanguageStatusNode(t *testing.T, res *ExecuteResult) memorynodes.MemoryNode {
	t.Helper()
	nodes, ok := res.OutputPayload().(map[string]memorynodes.MemoryNode)
	if !ok {
		t.Fatalf("languageStatus answered %T, want the builtin reply's node map", res.OutputPayload())
	}
	if len(nodes) != 1 {
		t.Fatalf("languageStatus answered %d rows, want exactly one", len(nodes))
	}
	for _, n := range nodes {
		return n
	}
	panic("unreachable")
}

func decodeLanguageStatus(t *testing.T, node memorynodes.MemoryNode) languageStatusRow {
	t.Helper()
	var row languageStatusRow
	if err := json.Unmarshal(node.Payload, &row); err != nil {
		t.Fatalf("payload is not the languageStatus row: %v\n%s", err, node.Payload)
	}
	return row
}

// The whole reply, over a tree that spells the deprecated form exactly once:
// the line the engine speaks, the edition and the status that qualifies every
// promise made about it, and the table's forms each carrying the place this
// node's own load found them.
func TestLanguageStatusAnswersTheLineTheFormsAndWhereTheLoadedTreeUsesThem(t *testing.T) {
	installSeededCatalog(t)
	eng, initErr, _ := bootOverlayEngineForTest(t, fstest.MapFS{
		"langstatus/memql.toml":     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"langstatus/concepts.memql": {Data: []byte(deprecatedTicket)},
	})
	if initErr != nil {
		t.Fatalf("a tree whose only finding is a deprecated form must boot: %v", initErr)
	}

	// The call string the generated SDK method sends, through the parse Execute
	// runs, to the builtin branch it dispatches. (Execute itself wants a
	// database before it parses; nothing this builtin does touches one.)
	plan, err := eng.Parse("builtin languageStatus()")
	if err != nil {
		t.Fatalf("the SDK's call string does not parse: %v", err)
	}
	call, ok := plan.Root.(*BuiltinFunctionExpression)
	if !ok || call.Executor != BuiltinExecutorLanguageStatus {
		t.Fatalf("the SDK's call string planned %T %+v, want the languageStatus builtin", plan.Root, plan.Root)
	}
	nodes, err := eng.evaluateBuiltinFunctionExpression(asCaller("owner"), call, 0)
	if err != nil {
		t.Fatalf("languageStatus refused an owner: %v", err)
	}
	node := oneLanguageStatusNode(t, NewResultWithOutput(nodesToMap(nodes)))
	if node.ID != LanguageStatusConcept || node.Concept != LanguageStatusConcept {
		t.Errorf("node id %q concept %q, want both %q", node.ID, node.Concept, LanguageStatusConcept)
	}
	row := decodeLanguageStatus(t, node)

	// Against the CONSTANTS the values come from, rather than their literals
	// today (1.0, 2026, frozen, a window of 2): a line, a status or a window
	// that moves on purpose must not fail a test about where the reply reads
	// it. The status in particular is designed to flip once.
	if row.Language != languageParser.LanguageVersion {
		t.Errorf("language = %q, want %q", row.Language, languageParser.LanguageVersion)
	}
	if row.Edition != languageParser.Edition {
		t.Errorf("edition = %q, want %q", row.Edition, languageParser.Edition)
	}
	if row.Status != languageParser.EditionStatus {
		t.Errorf("status = %q, want parser.EditionStatus %q", row.Status, languageParser.EditionStatus)
	}
	if row.GrammarVersion != languageParser.GrammarVersion {
		t.Errorf("grammarVersion = %q, want %q", row.GrammarVersion, languageParser.GrammarVersion)
	}
	if row.EditorRelease != languageParser.EditorRelease {
		t.Errorf("editorRelease = %q, want %q", row.EditorRelease, languageParser.EditorRelease)
	}
	if row.DeprecationWindowMinors != deprecation.MinimumMinorReleases {
		t.Errorf("deprecationWindowMinors = %d, want %d", row.DeprecationWindowMinors, deprecation.MinimumMinorReleases)
	}

	// Every registered form, in the table's order, each carrying its window.
	registered := deprecation.Forms()
	if len(row.Forms) != len(registered) {
		t.Fatalf("forms = %+v, want one per registered form (%d)", row.Forms, len(registered))
	}
	for i, want := range registered {
		got := row.Forms[i]
		if got.Rule != want.Rule || got.Spelling != want.Spelling || got.Replacement != want.Replacement ||
			got.Migrator != want.Migrator || got.DeprecatedIn != want.DeprecatedIn ||
			got.RefusedFrom != want.RefusedFrom() {
			t.Errorf("forms[%d] = %+v, want the registered form %+v", i, got, want)
		}
	}

	arrayType := row.Forms[0]
	if arrayType.Rule != deprecation.ArrayType {
		t.Fatalf("forms[0] is %q, want %q", arrayType.Rule, deprecation.ArrayType)
	}
	if arrayType.Spelling != "array(T)" || arrayType.Replacement != "[]T" {
		t.Errorf("the array form reads %+v", arrayType)
	}
	// A test binary is not cut from a release, so the window cannot be read and
	// the form still loads -- which is what this node does with it, and so what
	// it must report. TestLanguageStatusReadsTheStateAtTheReleaseTheNodeWasCutFrom
	// moves the release and watches it flip.
	if arrayType.State != languageFormDeprecated {
		t.Errorf("state = %q on an unstamped build, want %q", arrayType.State, languageFormDeprecated)
	}
	// EXACTLY the overlay's one use, carrying the text as written -- the
	// concrete `array(string)` the generic `array(T)` cannot show. The embedded
	// tree spells none, so a second use here is the core tree regressing, and a
	// zero is the scan not reaching the reply.
	wantUses := []languageStatusUseRow{{File: "langstatus/concepts.memql", Line: 4, Column: 8, Text: "array(string)"}}
	if !reflect.DeepEqual(arrayType.Uses, wantUses) {
		t.Errorf("uses = %+v, want %+v", arrayType.Uses, wantUses)
	}
}

// THE STATE IS DERIVED, at the release the node was cut from, and this is the
// test that says so: nothing in the table is flipped, only the release moves.
// A cluster on 0.25 reports `refused` for a form deprecated in 0.23 with a
// two-minor window, and the same binary on 0.24 reports `deprecated` -- which
// is the promise the window makes, kept by arithmetic rather than by somebody
// remembering to edit a field.
func TestLanguageStatusReadsTheStateAtTheReleaseTheNodeWasCutFrom(t *testing.T) {
	form := deprecation.Forms()[0]
	for _, tc := range []struct {
		release string
		want    string
	}{
		{"", languageFormDeprecated}, // a dev build: unreadable, so fail-open
		{form.DeprecatedIn, languageFormDeprecated},
		{"0.24.0", languageFormDeprecated},
		{form.RefusedFrom() + ".0", languageFormRefused},
		{"1.0.0", languageFormRefused},
	} {
		restore := deprecation.SetCurrent(tc.release)
		nodes, err := (&MemQLEngine{}).evaluateLanguageStatusExpression(asCaller("owner"))
		restore()
		if err != nil {
			t.Fatalf("release %q: %v", tc.release, err)
		}
		row := decodeLanguageStatus(t, nodes[0])
		if row.Forms[0].State != tc.want {
			t.Errorf("at release %q the form reads %q, want %q", tc.release, row.Forms[0].State, tc.want)
		}
		// The window itself never moves with the asking release: the floor is a
		// fact about the form.
		if row.Forms[0].RefusedFrom != form.RefusedFrom() {
			t.Errorf("at release %q refusedFrom = %q, want the form's own %q",
				tc.release, row.Forms[0].RefusedFrom, form.RefusedFrom())
		}
	}
}

// A form is listed from the TABLE, not from the uses: an engine that loaded
// nothing still reports every form and its window, because that is the version
// of the page a person reads BEFORE they write the form. `uses` is then an
// EMPTY LIST rather than a null -- the OS reads it as an array, and "no use" is
// an answer rather than a missing field.
func TestLanguageStatusListsAFormNothingUsesWithAnEmptyUseList(t *testing.T) {
	nodes, err := (&MemQLEngine{}).evaluateLanguageStatusExpression(asCaller("owner"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}

	var raw struct {
		Forms []map[string]any `json:"forms"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Forms) != len(deprecation.Forms()) {
		t.Fatalf("forms = %v, want one per registered form", raw.Forms)
	}
	var rules []string
	for _, f := range raw.Forms {
		rule, _ := f["rule"].(string)
		rules = append(rules, rule)
		uses, isList := f["uses"].([]any)
		if !isList || len(uses) != 0 {
			t.Errorf("a form with no use carries uses = %#v, want an empty list", f["uses"])
		}
	}
	// The table's own rule order, which Forms sorts -- so two nodes answering
	// the same table list it the same way.
	want := make([]string, 0, len(deprecation.Forms()))
	for _, f := range deprecation.Forms() {
		want = append(want, f.Rule)
	}
	if strings.Join(rules, ",") != strings.Join(want, ",") {
		t.Errorf("forms in order %v, want the table's rule order %v", rules, want)
	}
}

// The capability is the wall: nothing here reads a row, so no @rowAuthz tier
// can gate it, and the builtin must refuse every caller the seeds do not grant
// it.
func TestLanguageStatusRequiresReadOnTheLanguageSection(t *testing.T) {
	installSeededCatalog(t)
	e := treeBuiltinEngine(t)

	fn, err := e.functions.Get(BuiltinExecutorLanguageStatus)
	if err != nil || fn == nil {
		t.Fatalf("languageStatus is not in the loaded tree: %v", err)
	}
	// THE CONTROL COMES FROM THE TREE: a builtin that lost its annotation would
	// admit everybody below, and this test would call that a pass.
	if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbRead, Resource: "app:settings/language"}) {
		t.Fatalf("languageStatus declares %+v, want read on app:settings/language", fn.RequiresCapability)
	}

	for _, role := range []string{"user", "viewer"} {
		nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), builtinCall(BuiltinExecutorLanguageStatus), 0)
		if err == nil {
			t.Fatalf("languageStatus answered a %s with %d rows; the section is seeded for owner, developer and admin", role, len(nodes))
		}
		if !strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": ") || !strings.Contains(err.Error(), "app:settings/language") {
			t.Fatalf("languageStatus refused a %s with %q, want the capability refusal naming app:settings/language", role, err)
		}
	}

	// THE REACHABLE POSITIVE: a gate that refused everybody would pass the half
	// above.
	for _, role := range []string{"owner", "developer", "admin"} {
		nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), builtinCall(BuiltinExecutorLanguageStatus), 0)
		if err != nil {
			t.Fatalf("languageStatus refused a %s, whom the seeds grant it: %v", role, err)
		}
		if len(nodes) != 1 {
			t.Fatalf("languageStatus answered a %s with %d rows, want 1", role, len(nodes))
		}
	}
}
