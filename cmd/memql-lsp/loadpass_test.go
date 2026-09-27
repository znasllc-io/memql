package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// loadpass_test.go -- Lower's load refusals in the editor (memql#5434): the
// debounced publish sends Diagnose's diagnostics at once and the load's
// refusals follow, for the buffer's text and the document's place in the tree.

const lspLanguageLine = "memql = \"1.0\"\nedition = \"2026\"\n"

// twoWidgetWorkspace is a product repository whose two domains, under dsl/,
// both declare a `widget`: alpha's shines, beta's weighs. Which one a bare
// `widget` means is decided by the file it is written in.
func twoWidgetWorkspace(t *testing.T) string {
	t.Helper()
	return writeWorkspace(t, map[string]string{
		"dsl/alpha/memql.toml": lspLanguageLine,
		"dsl/alpha/concepts.memql": `/// An alpha widget.
concept widget {
  /// Label.
  label  string  @required
  /// Whether it shines.
  shine  bool
}
`,
		"dsl/beta/memql.toml": lspLanguageLine,
		"dsl/beta/concepts.memql": `/// A beta widget.
concept widget {
  /// Label.
  label   string  @required
  /// Weight.
  weight  float
}
`,
	})
}

// shiningWidgets reads `shine`, which alpha's widget declares and beta's does
// not.
const shiningWidgets = `/// Widgets that shine.
@unbounded("fixture")
query widget shiningWidgets {
  filter row => row.shine == true
}
`

// The build is seconds; the tests that need one share it, each with its own
// server over it.
var (
	widgetBuildOnce  sync.Once
	widgetBuildRoot  string
	widgetBuildSvc   *sense.Service
	widgetBuildLines memql.WorkspaceLanguageLines
)

// widgetServer is a server over the two-domain workspace whose loads run where
// the test can see them finish: on the publishing goroutine.
func widgetServer(t *testing.T) (*server, string) {
	t.Helper()
	widgetBuildOnce.Do(func() {
		// The first test's temp directory holds the files the build reads; the
		// built service keeps everything it needs, and a later test uses the
		// root only as the prefix a document path is placed against.
		widgetBuildRoot = twoWidgetWorkspace(t)
		s := initializedServer(t, widgetBuildRoot)
		widgetBuildSvc, widgetBuildLines = s.getBuild()
	})
	if !widgetBuildSvc.CanLoad() {
		t.Fatal("the two-domain workspace must build clean, or its service has no engine to load with")
	}
	s := newTestServerWithSense(t, widgetBuildSvc)
	s.root = widgetBuildRoot
	s.setBuild(widgetBuildSvc, widgetBuildLines)
	s.loads.start = func(fn func()) { fn() }
	return s, widgetBuildRoot
}

func docURI(root, rel string) protocol.DocumentUri {
	return pathToURI(filepath.Join(root, filepath.FromSlash(rel)))
}

func loadCoded(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if strings.HasPrefix(diagnosticCode(d), "lower_") {
			out = append(out, d)
		}
	}
	return out
}

// The fast publish comes first and does not wait on the load; the load's
// refusal follows in a second publish, on the token, with its code, in the
// file whose domain refuses it.
func TestLoadPass_RefusalFollowsDiagnoseOnTheToken(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)

	if len(*got) != 2 {
		t.Fatalf("want Diagnose's publish and then the load's, got %d publishes", len(*got))
	}
	if n := len(loadCoded((*got)[0].Diagnostics)); n != 0 {
		t.Errorf("the first publish is Diagnose's alone -- it must not wait on the load -- yet it carries %d load refusals", n)
	}
	refusals := loadCoded((*got)[1].Diagnostics)
	if len(refusals) != 1 {
		t.Fatalf("want one load refusal in the second publish, got %+v", (*got)[1].Diagnostics)
	}
	d := refusals[0]
	if diagnosticCode(d) != "lower_unknown_field" || d.Severity == nil || *d.Severity != protocol.DiagnosticSeverityError {
		t.Errorf("want an Error coded lower_unknown_field, got %+v", d)
	}
	want := protocol.Range{Start: protocol.Position{Line: 3, Character: 16}, End: protocol.Position{Line: 3, Character: 25}}
	if d.Range != want {
		t.Errorf("range %+v, want %+v -- exactly `row.shine`", d.Range, want)
	}
	if strings.Contains(d.Message, "[lower_") || !strings.Contains(d.Message, "v1:beta:widget") {
		t.Errorf("the message names the concept and leaves the code to its field: %q", d.Message)
	}
}

// Two domains, one name: the same text in alpha reads a field alpha's widget
// declares. No refusal -- and no second publish, since the load found nothing
// to add.
func TestLoadPass_TwoDomainsOneNameNoFalseError(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/alpha/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if len(*got) != 1 {
		t.Errorf("a load that finds nothing publishes nothing more; got %d publishes", len(*got))
	}
	for _, p := range *got {
		if n := loadCoded(p.Diagnostics); len(n) != 0 {
			t.Errorf("alpha's widget declares `shine`, yet the editor refused it: %+v", n)
		}
	}
}

// A document outside the DSL tree cannot be placed, so it gets no load.
func TestLoadPass_NoPassOutsideTheTree(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "notes/queries.memql")
	s.docs.open(uri, shiningWidgets)
	ran := false
	s.loads.start = func(fn func()) { ran = true; fn() }
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if ran || len(*got) != 1 {
		t.Errorf("no load for a document in no domain: ran=%v publishes=%d", ran, len(*got))
	}
}

// Between a keystroke and the next load, the last refusal is carried across
// the edit: moved with its line when the edit is above it, dropped when the
// edit is on it -- never blinking off at every pause, never drawn on the wrong
// text.
func TestLoadPass_CarriesTheLastRefusalAcrossAnEdit(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)

	// Hold the next load back, to see what the fast publish alone draws.
	var held []func()
	s.loads.start = func(fn func()) { held = append(held, fn) }

	s.docs.open(uri, "// a note above\n"+shiningWidgets)
	s.publishDiagnostics(notify, uri)
	carried := loadCoded((*got)[len(*got)-1].Diagnostics)
	if len(carried) != 1 || carried[0].Range.Start.Line != 4 {
		t.Fatalf("a line added above moves the refusal down one line, got %+v", carried)
	}

	edited := strings.Replace(shiningWidgets, "row.shine == true", "row.shine == false", 1)
	s.docs.open(uri, edited)
	s.publishDiagnostics(notify, uri)
	if carried := loadCoded((*got)[len(*got)-1].Diagnostics); len(carried) != 0 {
		t.Errorf("an edit on the refused line drops the carried refusal until the load answers, got %+v", carried)
	}

	// The held load answers for the buffer as it is now.
	for _, fn := range held {
		fn()
	}
	if refusals := loadCoded((*got)[len(*got)-1].Diagnostics); len(refusals) != 1 || refusals[0].Range.Start.Line != 3 {
		t.Errorf("the load answers for the edited buffer: %+v", refusals)
	}
}

// The buffer is what is loaded: a fix typed and not saved clears the refusal.
func TestLoadPass_AFixInTheBufferClearsTheRefusal(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)

	s.docs.open(uri, strings.Replace(shiningWidgets, "row.shine == true", "row.weight > 1", 1))
	s.publishDiagnostics(notify, uri)
	if last := loadCoded((*got)[len(*got)-1].Diagnostics); len(last) != 0 {
		t.Errorf("the fixed buffer must end with no load refusal, got %+v", last)
	}
	if len(*got) != 4 {
		t.Errorf("want 4 publishes (fast and load, twice): the load that cleared the refusal must publish; got %d", len(*got))
	}
}

// Closing a document forgets its load, so nothing it found outlives it.
func TestLoadPass_CloseForgets(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, _ := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if len(s.loads.carried(uri, shiningWidgets)) != 1 {
		t.Fatal("control: the load's refusal is held for the open document")
	}
	if err := s.didClose(&glsp.Context{Notify: notify}, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.loads.carried(uri, shiningWidgets); got != nil {
		t.Errorf("a closed document keeps no load result: %+v", got)
	}
}

func TestCarryAcrossEdit(t *testing.T) {
	at := func(line int) sense.Diagnostic {
		return sense.Diagnostic{Range: sense.Range{
			Start: sense.Position{Line: line, Column: 3},
			End:   sense.Position{Line: line, Column: 9},
		}, Code: "lower_x"}
	}
	lines := func(d []sense.Diagnostic) []int {
		out := []int{}
		for _, x := range d {
			out = append(out, x.Range.Start.Line)
		}
		return out
	}
	old := "a\nb\nc\nd\ne"
	diags := []sense.Diagnostic{at(1), at(3), at(5)}
	for _, tc := range []struct {
		name, new string
		want      []int
	}{
		{"unchanged", old, []int{1, 3, 5}},
		{"a line inserted above the middle", "a\nb\nNEW\nc\nd\ne", []int{1, 4, 6}},
		{"a line removed above the middle", "a\nc\nd\ne", []int{1, 2, 4}},
		{"the middle line edited", "a\nb\nC\nd\ne", []int{1, 5}},
		{"the first line edited", "A\nb\nc\nd\ne", []int{3, 5}},
		{"the last line edited", "a\nb\nc\nd\nE", []int{1, 3}},
		{"everything replaced", "x\ny", []int{}},
	} {
		if got := lines(carryAcrossEdit(old, tc.new, diags)); !equalInts(got, tc.want) {
			t.Errorf("%s: carried to lines %v, want %v", tc.name, got, tc.want)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadPasses_OneInFlightAndOneQueued(t *testing.T) {
	l := newLoadPasses()
	const uri = "file:///w/dsl/a/q.memql"
	if !l.begin(uri) {
		t.Fatal("the first pass claims the document")
	}
	if l.begin(uri) || l.begin(uri) {
		t.Fatal("while one runs, later requests queue behind it")
	}
	if !l.finish(uri) {
		t.Fatal("a pass that was asked for again runs again")
	}
	if l.finish(uri) {
		t.Fatal("once caught up, the pass ends")
	}
	if !l.begin(uri) {
		t.Fatal("after it ends, the next request starts a pass")
	}
}

func TestTreePath(t *testing.T) {
	s := newServer("/w", nil)
	for _, tc := range []struct {
		uri    string
		root   string
		want   string
		placed bool
	}{
		{"file:///w/dsl/planner/queries.memql", "dsl", "planner/queries.memql", true},
		{"file:///w/dsl/shopify/overlay/queries.memql", "dsl", "shopify/overlay/queries.memql", true},
		{"file:///w/notes/queries.memql", "dsl", "", false},
		{"file:///w/planner/queries.memql", "", "planner/queries.memql", true},
		{"file:///elsewhere/planner/queries.memql", "", "", false},
		{"untitled:Untitled-1", "", "", false},
	} {
		got, placed := s.treePath(tc.uri, memql.WorkspaceLanguageLines{Root: tc.root})
		if got != tc.want || placed != tc.placed {
			t.Errorf("treePath(%s, root %q) = %q, %v; want %q, %v", tc.uri, tc.root, got, placed, tc.want, tc.placed)
		}
	}
}

// The shipped path: the load runs on its own goroutine while the edits
// keep coming, and the last word for the buffer is the load's answer for its
// final text. Run under -race, this is the test that sees the locks.
func TestLoadPass_OffTheRequestPath(t *testing.T) {
	s, root := widgetServer(t)
	s.loads.start = func(fn func()) { go fn() }
	uri := docURI(root, "dsl/beta/queries.memql")

	var mu sync.Mutex
	var last []protocol.Diagnostic
	notify := func(method string, params any) {
		if p, ok := params.(protocol.PublishDiagnosticsParams); ok && method == protocol.ServerTextDocumentPublishDiagnostics {
			mu.Lock()
			last = p.Diagnostics
			mu.Unlock()
		}
	}
	s.docs.open(uri, shiningWidgets)
	s.publishDiagnostics(notify, uri)
	final := strings.Replace(shiningWidgets, "row.shine == true", "row.shine == false && row.label != \"\"", 1)
	for i := 0; i < 5; i++ {
		s.docs.open(uri, final)
		s.publishDiagnostics(notify, uri)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.loads.mu.Lock()
		idle := len(s.loads.running) == 0
		result := s.loads.results[uri]
		s.loads.mu.Unlock()
		if idle && result.text == final {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	refusals := loadCoded(last)
	if len(refusals) != 1 || refusals[0].Range.Start.Line != 3 {
		t.Errorf("the last publish is the load's answer for the final buffer, one refusal on line 3; got %+v", last)
	}
}

// A product domain may relate its concepts to a storefront pack's (epic
// memql#5532), and every published image links those packs. The build models
// that image, so the workspace builds -- with its registry, and with its load
// pass -- instead of refusing every file over an import a node accepts.
func TestBuild_ADomainRelatingToAStorefrontPackConceptBuilds(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"dsl/shop/memql.toml": lspLanguageLine,
		"dsl/shop/concepts.memql": `use wholesale.concepts.{ application }

/// The shop's own detail on a wholesale application.
concept applicationNote {
  /// The application this note is about.
  applicationId  string  @required
  /// The note.
  body           string

  @relationship(type="references", field="applicationId", target=application, direction="outgoing")
}
`,
	})
	s := initializedServer(t, root)
	svc, _ := s.getBuild()
	if !svc.CanLoad() {
		t.Fatal("a workspace relating to a storefront pack's concept must build with its engine, as it boots on a node")
	}
	s.loads.start = func(fn func()) { fn() }
	uri := docURI(root, "dsl/shop/queries.memql")
	s.docs.open(uri, `/// Notes with a body the concept does not have.
@unbounded("fixture")
query applicationNote notesByTitle {
  filter row => row.title != ""
}
`)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if refusals := loadCoded((*got)[len(*got)-1].Diagnostics); len(refusals) != 1 || diagnosticCode(refusals[0]) != "lower_unknown_field" {
		t.Errorf("the load pass runs in the workspace and refuses `row.title`, got %+v", (*got)[len(*got)-1].Diagnostics)
	}
}

// A rebuild that fails leaves a service with no engine, so no load can run --
// and nothing may go on drawing what the last good build found: refusals
// nothing keeps true any more, fixed ones included. The republish after the
// failed rebuild carries none.
func TestLoadPass_AFailedRebuildDropsTheLastBuildsRefusals(t *testing.T) {
	root := twoWidgetWorkspace(t)
	s := initializedServer(t, root)
	s.loads.start = func(fn func()) { fn() }
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if n := len(loadCoded((*got)[len(*got)-1].Diagnostics)); n != 1 {
		t.Fatalf("control: the good build refuses `row.shine` once, got %d", n)
	}

	// The refusal is fixed in another file -- beta's widget gains `shine` --
	// while the workspace breaks elsewhere: alpha loses its language line, so
	// the rebuild fails its strict boot. Rebuild as a save does: build, then
	// republish every open document.
	writeWorkspaceFile(t, root, "dsl/beta/concepts.memql", `/// A beta widget.
concept widget {
  /// Label.
  label   string  @required
  /// Weight.
  weight  float
  /// Whether it shines.
  shine   bool
}
`)
	if err := os.Remove(filepath.Join(root, "dsl", "alpha", "memql.toml")); err != nil {
		t.Fatal(err)
	}
	s.buildSense(nil)
	if svc, _ := s.getBuild(); svc.CanLoad() {
		t.Fatal("control: the broken workspace must fail its build, leaving no engine to load with")
	}
	s.publishDiagnostics(notify, uri)
	if refusals := loadCoded((*got)[len(*got)-1].Diagnostics); len(refusals) != 0 {
		t.Errorf("a failed rebuild must not keep drawing the last build's refusals -- this one was fixed in another file -- got %+v", refusals)
	}
	if carried := s.loads.carried(uri, shiningWidgets); carried != nil {
		t.Errorf("the document's load result must be forgotten, got %+v", carried)
	}
}

// A pass that loaded against a build a rebuild has since replaced does not
// store or publish what it found: the rebuild republished the document under
// its own build, and the old answer would overwrite it.
func TestLoadPass_APassAgainstAReplacedBuildIsDiscarded(t *testing.T) {
	s, root := widgetServer(t)
	uri := docURI(root, "dsl/beta/queries.memql")
	s.docs.open(uri, shiningWidgets)
	_, lines := s.getBuild()
	swapping := &buildSwappingLoads{onLoad: func() { s.setBuild(sense.New(nil), lines) }}
	s.setBuild(sense.New(swapping), lines)

	notify, got := capturingNotify()
	s.publishDiagnostics(notify, uri)
	if !swapping.called {
		t.Fatal("control: the pass must have run and swapped the build")
	}
	for _, p := range *got {
		if refusals := loadCoded(p.Diagnostics); len(refusals) != 0 {
			t.Errorf("a pass against the replaced build published its answer: %+v", refusals)
		}
	}
	if carried := s.loads.carried(uri, shiningWidgets); carried != nil {
		t.Errorf("a pass against the replaced build stored its answer: %+v", carried)
	}
}

// buildSwappingLoads is a registry that can load, and that replaces the
// server's build in the middle of its pass -- as a rebuild finishing under a
// running pass does -- before answering with a refusal.
type buildSwappingLoads struct {
	onLoad func()
	called bool
}

func (b *buildSwappingLoads) LoadDiagnostics(context.Context, string, string) []sense.Diagnostic {
	b.called = true
	b.onLoad()
	return []sense.Diagnostic{{
		Range:    sense.Range{Start: sense.Position{Line: 4, Column: 17}, End: sense.Position{Line: 4, Column: 26}},
		Severity: sense.SeverityError,
		Message:  "`row.shine` does not lower in a query filter",
		Code:     "lower_unknown_field",
	}}
}

func (*buildSwappingLoads) FunctionNames() []string                        { return nil }
func (*buildSwappingLoads) FunctionGet(string) (*sense.FunctionInfo, bool) { return nil, false }
func (*buildSwappingLoads) ConceptNames() []string                         { return nil }
func (*buildSwappingLoads) ConceptGet(string) (*sense.ConceptInfo, bool)   { return nil, false }
func (*buildSwappingLoads) SpecNames() []string                            { return nil }
func (*buildSwappingLoads) SpecGet(string) (*sense.SpecInfo, bool)         { return nil, false }
func (*buildSwappingLoads) ToolNames() []string                            { return nil }
func (*buildSwappingLoads) ToolGet(string) (*sense.ToolInfo, bool)         { return nil, false }
func (*buildSwappingLoads) PromptNames() []string                          { return nil }
func (*buildSwappingLoads) PromptGet(string) (*sense.PromptInfo, bool)     { return nil, false }
func (*buildSwappingLoads) ProviderNames() []string                        { return nil }
func (*buildSwappingLoads) ProviderGet(string) (*sense.ProviderInfo, bool) { return nil, false }
func (*buildSwappingLoads) ShapeNames() []string                           { return nil }
func (*buildSwappingLoads) ShapeGet(string) (*sense.ShapeInfo, bool)       { return nil, false }
func (*buildSwappingLoads) IntegrationCapabilities() []string              { return nil }

func writeWorkspaceFile(t *testing.T, root, rel, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
