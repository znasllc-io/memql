package memql

// sense_load_pass_test.go -- the editor's load pass (memql#5434): Lower's
// refusals of one document, resolved as the load resolves them.
//
// The fixture is two product domains that both declare a concept named
// `widget`, with different fields: alpha's shines, beta's weighs. Every bare
// `widget` in the workspace is therefore a name two domains share, and which
// one it means is decided by the file it is written in -- the case the issue
// names, where a pass that does not know the document's place in the tree
// shows errors the load never makes.

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql/sense"
)

var twoWidgetDomains = fstest.MapFS{
	"alpha/memql.toml": languageLineFile(),
	"alpha/concepts.memql": {Data: []byte(`/// An alpha widget.
concept widget {
  /// Label.
  label  string  @required
  /// Whether it shines.
  shine  bool
}

/// An alpha gadget. gamma declares one too, in a file not yet saved.
concept gadget {
  /// Label.
  label  string  @required
}
`)},
	"alpha/specs.memql": {Data: []byte(`/// Shiny alpha widgets.
spec widget isShiny = row => row.shine == true
`)},
	"alpha/queries.memql": {Data: []byte(`/// Shiny alpha widgets.
@unbounded("fixture")
query widget shinyWidgets {
  filter row => isShiny(row) && row.label != ""
}
`)},
	"beta/memql.toml": languageLineFile(),
	"beta/concepts.memql": {Data: []byte(`/// A beta widget.
concept widget {
  /// Label.
  label   string  @required
  /// Weight.
  weight  float
}
`)},
	"beta/specs.memql": {Data: []byte(`/// Heavy beta widgets.
spec widget isHeavy = row => row.weight > 1
`)},
	"beta/queries.memql": {Data: []byte(`/// Heavy beta widgets.
@unbounded("fixture")
query widget heavyWidgets {
  filter row => row.weight > 2 && isHeavy(row)
}
`)},
	"gamma/memql.toml": languageLineFile(),
	"gamma/concepts.memql": {Data: []byte(`/// A gamma gear.
concept gear {
  /// Teeth.
  teeth  int
}
`)},
}

// The offline build is the expensive part (seconds), so this file shares one;
// no test here mutates it -- the pass is read-only, which is part of what
// TestLowerRefusals_LeavesTheEngineAsItWas pins.
var (
	twoWidgetOnce    sync.Once
	twoWidgetAdapter *SenseAdapter
	twoWidgetErr     error
)

func twoWidgetEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	twoWidgetOnce.Do(func() {
		twoWidgetAdapter, twoWidgetErr = buildOfflineSenseAdapter(nil, twoWidgetDomains)
	})
	if twoWidgetErr != nil {
		t.Fatalf("the two-domain fixture must boot clean -- a fixture that does not load proves nothing about the pass: %v", twoWidgetErr)
	}
	return twoWidgetAdapter.engine
}

// Both domains' files, as saved, load clean -- so the pass over each, placed
// where it lives, must say nothing. A pass that resolved `widget` by trailing
// segment, or through the wrong domain, would refuse `row.shine` or
// `row.weight` in one of them.
func TestLowerRefusals_TwoDomainsOneName_NoFalseError(t *testing.T) {
	e := twoWidgetEngine(t)
	for path, file := range twoWidgetDomains {
		if !strings.HasSuffix(path, ".memql") {
			continue
		}
		if got := e.LowerRefusals(string(file.Data), path); len(got) != 0 {
			t.Errorf("%s loads clean, yet the pass refused it: %+v", path, got)
		}
	}
}

// The true error is shown, in the file that has it, on the token. beta's
// widget has no `shine`, so a beta query reading it is refused -- and the SAME
// text placed in alpha is not, because alpha's widget declares the field. That
// is the path deciding which `widget` a bare name means.
func TestLowerRefusals_TwoDomainsOneName_TrueErrorInTheRightFile(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `/// Beta widgets that shine -- which a beta widget cannot.
@unbounded("fixture")
query widget shiningBetaWidgets {
  filter row => row.shine == true
}
`
	got := e.LowerRefusals(src, "beta/queries.memql")
	if len(got) != 1 {
		t.Fatalf("want exactly one refusal of `row.shine` in beta, got %d: %+v", len(got), got)
	}
	d := got[0]
	if d.Code != LowerCodeUnknownField {
		t.Errorf("code = %q, want %q", d.Code, LowerCodeUnknownField)
	}
	if !strings.Contains(d.Error, "v1:beta:widget") {
		t.Errorf("the refusal must name the concept the document binds, v1:beta:widget: %q", d.Error)
	}
	if strings.Contains(d.Error, "[lower_") {
		t.Errorf("the message carries the code a second time; it rides in its own field: %q", d.Error)
	}
	// `row.shine` is line 4, columns 17-26 (the end is exclusive).
	if d.Line != 4 || d.Column != 17 || d.EndLine != 4 || d.EndColumn != 26 {
		t.Errorf("range = %d:%d-%d:%d, want 4:17-4:26 -- exactly the refused node", d.Line, d.Column, d.EndLine, d.EndColumn)
	}
	if line := strings.Split(src, "\n")[d.Line-1]; string([]rune(line)[d.Column-1:d.EndColumn-1]) != "row.shine" {
		t.Errorf("the range covers %q, not the refused node", string([]rune(line)[d.Column-1:d.EndColumn-1]))
	}

	if got := e.LowerRefusals(src, "alpha/queries.memql"); len(got) != 0 {
		t.Errorf("in alpha the same text reads a field alpha's widget declares, so nothing is refused: %+v", got)
	}
}

// A new, unsaved file declares a `gadget` in gamma while alpha's saved one is
// the only `gadget` the engine holds. Placed in gamma, the document's concept
// is its own domain's and its spec binds it; a pass that did not know where the
// document lives would bind the spec to alpha's gadget -- the one name the
// engine can find -- and refuse `row.size` as a field that gadget lacks: an
// error the load, which reads the file in gamma, never makes.
func TestLowerRefusals_TwoDomainsOneName_NewFileInItsOwnDomain(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `/// A gamma gadget.
concept gadget {
  /// Size.
  size  int
}

/// Big gadgets.
spec gadget isBig = row => row.size > 1
`
	if got := e.LowerRefusals(src, "gamma/gadgets.memql"); len(got) != 0 {
		t.Errorf("gamma's gadget declares size, so nothing is refused; a refusal here is the false error of a document placed in the wrong domain: %+v", got)
	}
	// Control: the same spec in alpha binds alpha's gadget, which has no size.
	spec := "/// Big gadgets.\nspec gadget isBig = row => row.size > 1\n"
	got := e.LowerRefusals(spec, "alpha/specs.memql")
	if len(got) != 1 || got[0].Code != LowerCodeUnknownField || !strings.Contains(got[0].Error, "v1:alpha:gadget") {
		t.Errorf("control: in alpha the spec binds alpha's gadget and `row.size` is refused, got %+v", got)
	}
}

// The pass needs the document's place in the tree; without it there is no
// pass rather than a guess.
func TestLowerRefusals_NeedsTheTreePath(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `@unbounded("fixture")
query widget shiningBetaWidgets {
  filter row => row.shine == true
}
`
	for _, path := range []string{
		"",                         // an untitled buffer
		"queries.memql",            // no domain directory
		"zeta/queries.memql",       // a directory no build mounted
		"_reference/queries.memql", // a directory no loader reads
		"beta/notes.txt",           // not a .memql file
	} {
		if got := e.LowerRefusals(src, path); got != nil {
			t.Errorf("path %q places the document in no loaded domain, yet the pass answered: %+v", path, got)
		}
	}
	if got := e.LowerRefusals(src, "beta/queries.memql"); len(got) != 1 {
		t.Errorf("control: placed in beta, the same text must be refused once, got %+v", got)
	}
}

// The document replaces its file: a spec the buffer redefines is the buffer's
// version, not the one the engine loaded from disk. Here isShiny becomes a
// context spec, so a spec in the same buffer applying it to the row is
// refused -- which only the buffer's version can say; the saved one is a row
// spec and applies cleanly.
func TestLowerRefusals_TheBufferReplacesItsFile(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `use common.shapes.{ actorEnvelope }

/// Now a question about the caller.
spec actorEnvelope isShiny = actor => actor.role == "admin"

/// Widgets that shine.
spec widget isShinyWidget = row => isShiny(row)
`
	got := e.LowerRefusals(src, "alpha/specs.memql")
	if len(got) != 1 || got[0].Code != LowerCodeContextSpecOnRow {
		t.Fatalf("want one %s on `isShiny(row)`, got %+v", LowerCodeContextSpecOnRow, got)
	}
	if got[0].Name != "isShinyWidget" || got[0].Line != 7 {
		t.Errorf("the refusal belongs to isShinyWidget on line 7, got %s line %d", got[0].Name, got[0].Line)
	}

	// A spec the buffer no longer declares is gone, as it would be once saved.
	removed := `/// Widgets that shine.
spec widget isShinyWidget = row => isShiny(row)
`
	got = e.LowerRefusals(removed, "alpha/specs.memql")
	if len(got) != 1 || got[0].Code != LowerCodeUnknownName {
		t.Fatalf("isShiny was removed from its file, so applying it is %s; got %+v", LowerCodeUnknownName, got)
	}
}

// The text validated is the text given -- an unsaved edit, not the file on
// disk -- and a fix in the buffer clears the refusal before any save.
func TestLowerRefusals_ValidatesTheTextGiven(t *testing.T) {
	e := twoWidgetEngine(t)
	broken := strings.Replace(string(twoWidgetDomains["beta/queries.memql"].Data), "row.weight > 2", "row.weigth > 2", 1)
	got := e.LowerRefusals(broken, "beta/queries.memql")
	if len(got) != 1 || got[0].Code != LowerCodeUnknownField {
		t.Fatalf("the typo in the buffer must be refused, got %+v", got)
	}
	if got := e.LowerRefusals(string(twoWidgetDomains["beta/queries.memql"].Data), "beta/queries.memql"); len(got) != 0 {
		t.Errorf("the fixed buffer must be clean: %+v", got)
	}
}

// A @disabled spec is never registered, and its body is lowered all the same:
// re-enabling one that does not lower would refuse boot, so the editor says so
// now, in the load's words.
func TestLowerRefusals_DisabledSpecBody(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `/// Not yet.
@disabled
spec widget isLabelled = row => row.labell != ""
`
	got := e.LowerRefusals(src, "alpha/specs.memql")
	if len(got) != 1 || got[0].Code != LowerCodeUnknownField {
		t.Fatalf("want the disabled body's %s, got %+v", LowerCodeUnknownField, got)
	}
	if !strings.HasPrefix(got[0].Error, "@disabled, and its body does not lower") {
		t.Errorf("the refusal must say why a disabled body matters: %q", got[0].Error)
	}
}

// One document, several faults: each is reported once, on its own node, and a
// construct that fails before Lower sees it (a concept that does not resolve)
// hides nothing else.
func TestLowerRefusals_EachFaultOnce(t *testing.T) {
	e := twoWidgetEngine(t)
	src := `/// Two faults in one filter.
@unbounded("fixture")
query widget twoFaults {
  filter row => row.weigth > 1 && row.colour == "red"
}

/// Binds a concept nobody declares.
@unbounded("fixture")
query gadget noSuchConcept {
  filter row => row.label != ""
}
`
	got := e.LowerRefusals(src, "beta/queries.memql")
	if len(got) != 1 {
		t.Fatalf("Lower stops at a filter's first refusal, and the unresolved concept is not Lower's to report: want 1, got %+v", got)
	}
	if got[0].Name != "twoFaults" || got[0].Code != LowerCodeUnknownField || got[0].Column != 17 {
		t.Errorf("want twoFaults' `row.weigth` at column 17, got %+v", got[0])
	}
}

// The pass registers nothing: the engine's registries are the same before and
// after a pass that redefines a spec and a concept.
func TestLowerRefusals_LeavesTheEngineAsItWas(t *testing.T) {
	e := twoWidgetEngine(t)
	spec, err := e.specs.Get("alpha.isShiny")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	src := `use common.shapes.{ actorEnvelope }

/// Now a question about the caller.
spec actorEnvelope isShiny = actor => actor.role == "admin"
`
	_ = e.LowerRefusals(src, "alpha/specs.memql")
	_ = e.LowerRefusals("/// Changed.\nconcept widget {\n  /// Label.\n  label string @required\n}\n", "alpha/concepts.memql")
	after, err := e.specs.Get("alpha.isShiny")
	if err != nil || after.Kind != spec.Kind || after.BoundName != spec.BoundName {
		t.Errorf("the pass changed the engine's isShiny: before %s/%s, after %+v (%v)", spec.Kind, spec.BoundName, after, err)
	}
	c, err := e.concepts.Get("v1:alpha:widget")
	if err != nil || !slices.Contains(c.ProjectableFields(), "shine") {
		t.Errorf("the pass changed the engine's alpha widget: %v", err)
	}
}

// The adapter hands Sense Errors with the code in its field, and a refusal the
// load could anchor only at its construct draws its signature line.
func TestSenseAdapter_LoadDiagnostics(t *testing.T) {
	e := twoWidgetEngine(t)
	a := NewSenseAdapter(e)

	src := `@unbounded("fixture")
query widget shiningBetaWidgets {
  filter row => row.shine == true
}
`
	got := a.LoadDiagnostics(src, "beta/queries.memql")
	if len(got) != 1 {
		t.Fatalf("want one diagnostic, got %+v", got)
	}
	want := sense.Range{Start: sense.Position{Line: 3, Column: 17}, End: sense.Position{Line: 3, Column: 26}}
	if got[0].Range != want || got[0].Severity != sense.SeverityError || got[0].Code != LowerCodeUnknownField {
		t.Errorf("got %+v, want an Error %s over %+v", got[0], LowerCodeUnknownField, want)
	}

	// A spec over an @actor shape names its parameter `actor`; the refusal of
	// `row` carries no node position, so it is anchored at the signature.
	anchored := `use common.shapes.{ actorEnvelope }

/// Wrong parameter.
spec actorEnvelope isAdminCaller = row => row.role == "admin"
`
	got = a.LoadDiagnostics(anchored, "alpha/specs.memql")
	if len(got) != 1 {
		t.Fatalf("want one diagnostic, got %+v", got)
	}
	sig := "spec actorEnvelope isAdminCaller = row => row.role == \"admin\""
	wantLine := sense.Range{Start: sense.Position{Line: 4, Column: 1}, End: sense.Position{Line: 4, Column: len([]rune(sig)) + 1}}
	if got[0].Range != wantLine {
		t.Errorf("an anchored refusal draws its signature line %+v, got %+v", wantLine, got[0].Range)
	}
	if got[0].Code == "" || got[0].Message == "" {
		t.Errorf("an anchored refusal still carries its code and sentence: %+v", got[0])
	}
}

// One fault, one squiggle, with the real Diagnose beside the real load. A bare
// `id` in a filter is Diagnose's warning and the load's error, and the merge
// keeps the load's -- the one that says the file will not load. An unknown
// actor member is Diagnose's error alone: the loader's closed-envelope gate
// refuses that construct before Lower sees it, uncoded, so the pass is silent
// and the fault still draws exactly once.
func TestLowerRefusals_MergedWithDiagnoseOneSquigglePerFault(t *testing.T) {
	e := twoWidgetEngine(t)
	svc := sense.New(NewSenseAdapter(e))
	src := `/// The caller's widgets, by a member the envelope does not have.
@actor
@unbounded("fixture")
query widget widgetsByDisplayName {
  filter row => row.label == actor.displayName
}

/// Widgets by id, written bare.
@unbounded("fixture")
query widget widgetById {
  args {
    widgetId  string
  }
  filter row => row.label != ""
    && id == args.widgetId
}
`
	const path = "beta/queries.memql"
	fast := svc.Diagnose(src, path)
	load := svc.DiagnoseLoad(src, path)
	if len(load) != 1 || load[0].Code != LowerCodeUnknownName {
		t.Fatalf("the load refuses the bare `id` and nothing else, got %+v", load)
	}
	if !strings.Contains(load[0].Message, "write `row.id`") {
		t.Errorf("the refusal that replaces the warning must name the same fix the warning did: %q", load[0].Message)
	}
	var warned bool
	for _, d := range fast {
		if d.Code == "bare-row-intrinsic" && rangesMeet(d.Range, load[0].Range) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("control: Diagnose must warn on the same bare `id` for the merge to have a duplicate to drop; fast=%+v", fast)
	}
	merged := sense.MergeLoadDiagnostics(fast, load)

	byCode := map[string]int{}
	for _, d := range merged {
		byCode[d.Code]++
	}
	if byCode["actor-unknown-property"] != 1 {
		t.Errorf("the unknown actor member draws once, as Diagnose's error: %+v", merged)
	}
	if byCode[LowerCodeUnknownName] != 1 || byCode["bare-row-intrinsic"] != 0 {
		t.Errorf("the bare `id` draws once, as the load's error in place of the warning: %+v", merged)
	}
}

// rangesMeet reports whether two Sense ranges share a line and overlap on it.
func rangesMeet(a, b sense.Range) bool {
	return a.Start.Line == b.Start.Line && a.Start.Column < b.End.Column && b.Start.Column < a.End.Column
}
