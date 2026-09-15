package main

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// The "Rewrite to edition 2026" code actions (memql#5364), driven the way an
// editor drives them: initialize over a workspace on disk, open a legacy
// document, take the diagnostic the server publishes, ask for code actions at
// it, apply the edit. The expected text is the CLI's: memqlmigrate
// --rewrite=expressions is the tree's .memql files resolved over the core tree
// and RewriteExpressions over the file (cmd/memqlmigrate/expressions.go), which
// cliRewrite repeats.
//
// Every MemQL document here is legacy on purpose -- the rewrite of a legacy
// document is the subject -- so the Go-fixture migration leaves the file as it
// is: memqlmigrate:keep-file.

// codemodWorkspace is a bundle domain whose query names a spec over an @actor
// shape declared in two OTHER files: `isAdmin` becomes `isAdmin(actor)` only
// when the predicate set is the tree's, which is what makes the fix a
// tree-level answer rather than a file-level guess.
var codemodWorkspace = map[string]string{
	"fylo/concepts.memql": "concept order {\n  status  string\n  ownerUserId  string\n}\n",
	"fylo/shapes.memql":   "@actor\nshape actorEnvelope {\n  actor.role\n}\n",
	"fylo/specs.memql":    "use fylo.shapes.{ actorEnvelope }\n\nspec actorEnvelope isAdmin {\n  return role == \"admin\"\n}\n",
	"fylo/queries.memql": "use fylo.concepts.{ order }\nuse fylo.specs.{ isAdmin }\n\n" +
		"query order orders {\n" +
		"  args {\n" +
		"    status string\n" +
		"  }\n" +
		"  filter  status == args.status || isAdmin\n" +
		"  paginate 20\n" +
		"}\n",
}

func writeWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func initializedServer(t *testing.T, root string) *server {
	t.Helper()
	commonlog.Configure(-4, nil)
	s := newServer(root, commonlog.GetLogger(lsName))
	if _, err := s.initialize(nil, &protocol.InitializeParams{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return s
}

// workspaceServer is initializedServer without the offline engine build, for
// the tests that need only what a code action reads: a registry-less Sense
// publishes the same syntax diagnostics, and the rewrite state loads from root
// exactly as buildSense loads it.
func workspaceServer(t *testing.T, root string) *server {
	t.Helper()
	commonlog.Configure(-4, nil)
	s := newServer(root, commonlog.GetLogger(lsName))
	s.setBuild(sense.New(nil), memql.WorkspaceLanguageLines{})
	s.rewrite.load(root)
	return s
}

// openWorkspaceDoc opens the workspace file rel as the editor would and returns its
// uri, its text and the diagnostics the server published for it.
func openWorkspaceDoc(t *testing.T, s *server, root, rel string) (protocol.DocumentUri, string, []protocol.Diagnostic) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	uri := pathToURI(filepath.Join(root, filepath.FromSlash(rel)))
	notify, got := capturingNotify()
	if err := s.didOpen(&glsp.Context{Notify: notify}, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: "memql", Text: string(data)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("didOpen published %d diagnostic sets, want 1", len(*got))
	}
	return uri, string(data), (*got)[0].Diagnostics
}

func codeActions(t *testing.T, s *server, uri protocol.DocumentUri, rng protocol.Range, diags []protocol.Diagnostic, only ...protocol.CodeActionKind) []protocol.CodeAction {
	t.Helper()
	res, err := s.codeAction(nil, &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Range:        rng,
		Context:      protocol.CodeActionContext{Diagnostics: diags, Only: only},
	})
	if err != nil {
		t.Fatalf("codeAction: %v", err)
	}
	if res == nil {
		return nil
	}
	actions, ok := res.([]protocol.CodeAction)
	if !ok {
		t.Fatalf("codeAction returned %T, want []protocol.CodeAction", res)
	}
	return actions
}

func actionsOfKind(actions []protocol.CodeAction, kind protocol.CodeActionKind) []protocol.CodeAction {
	var out []protocol.CodeAction
	for _, a := range actions {
		if a.Kind != nil && *a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func diagnosticWithCode(diags []protocol.Diagnostic, code string) (protocol.Diagnostic, bool) {
	for _, d := range diags {
		if diagnosticCode(d) == code {
			return d, true
		}
	}
	return protocol.Diagnostic{}, false
}

// applyEdits applies one action's edits for uri the way an editor does: every
// range is read against the original text.
func applyEdits(t *testing.T, text string, a protocol.CodeAction, uri protocol.DocumentUri) string {
	t.Helper()
	if a.Edit == nil {
		t.Fatalf("action %q carries no edit", a.Title)
	}
	edits := append([]protocol.TextEdit(nil), a.Edit.Changes[uri]...)
	if len(edits) == 0 {
		t.Fatalf("action %q edits nothing in %s", a.Title, uri)
	}
	sort.Slice(edits, func(i, j int) bool {
		return position.ByteOffsetForLSP(text, edits[i].Range.Start) > position.ByteOffsetForLSP(text, edits[j].Range.Start)
	})
	out := text
	prev := len(text) + 1
	for _, e := range edits {
		start := position.ByteOffsetForLSP(text, e.Range.Start)
		end := position.ByteOffsetForLSP(text, e.Range.End)
		if end > prev {
			t.Fatalf("action %q has overlapping edits", a.Title)
		}
		out = out[:start] + e.NewText + out[end:]
		prev = start
	}
	return out
}

// cliRewrite is what memqlmigrate --rewrite=expressions writes for rel when
// run over root.
func cliRewrite(t *testing.T, root, rel string) string {
	t.Helper()
	files := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".memql" {
			return err
		}
		files[p], err = fs.ReadFile(os.DirFS(root), p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	local := make([]langparser.PredicateDeclarations, 0, len(paths))
	for _, p := range paths {
		local = append(local, langparser.ScanPredicateDeclarations(p, files[p]))
	}
	core, err := corePredicates()
	if err != nil {
		t.Fatal(err)
	}
	preds, err := langparser.ResolvePredicatesOver(core, local)
	if err != nil {
		t.Fatal(err)
	}
	out, err := langparser.RewriteExpressions(files[rel], preds)
	if err != nil {
		t.Fatalf("the CLI refuses %s: %v", rel, err)
	}
	return string(out)
}

// syntaxErrors is what the squiggles of text would be: the error-severity
// syntax diagnostics Diagnose reports.
func syntaxErrors(text string) []sense.Diagnostic {
	var out []sense.Diagnostic
	for _, d := range sense.New(nil).Diagnose(text, "") {
		if d.Severity == sense.SeverityError && syntaxCode(d.Code) {
			out = append(out, d)
		}
	}
	return out
}

func TestCodeAction_Advertised(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := res.(protocol.InitializeResult).Capabilities.CodeActionProvider.(protocol.CodeActionOptions)
	if !ok {
		t.Fatalf("codeActionProvider = %T, want CodeActionOptions", res.(protocol.InitializeResult).Capabilities.CodeActionProvider)
	}
	want := []protocol.CodeActionKind{"quickfix", "source.fixAll"}
	if strings.Join(opts.CodeActionKinds, ",") != strings.Join(want, ",") {
		t.Errorf("kinds = %v, want %v", opts.CodeActionKinds, want)
	}
	if s.handler().TextDocumentCodeAction == nil {
		t.Error("codeActionProvider is advertised and textDocument/codeAction has no handler")
	}
}

// The quick fix on a retired form: the published diagnostic carries the rule
// id, the action keys on it, and the edit writes the CLI's text -- one
// changed line, touched alone -- which then parses clean.
func TestCodeAction_QuickFixWritesTheCLIRewrite(t *testing.T) {
	root := writeWorkspace(t, codemodWorkspace)
	s := initializedServer(t, root)
	const rel = "fylo/queries.memql"
	uri, text, diags := openWorkspaceDoc(t, s, root, rel)

	d, ok := diagnosticWithCode(diags, "retired_filter_without_lambda")
	if !ok {
		t.Fatalf("no retired_filter_without_lambda diagnostic was published: %+v", diags)
	}
	quick := actionsOfKind(codeActions(t, s, uri, d.Range, []protocol.Diagnostic{d}, protocol.CodeActionKindQuickFix), protocol.CodeActionKindQuickFix)
	if len(quick) != 1 {
		t.Fatalf("want one quick fix, got %d", len(quick))
	}
	a := quick[0]
	if a.Title != "Rewrite to edition 2026" || a.IsPreferred == nil || !*a.IsPreferred || len(a.Diagnostics) != 1 {
		t.Errorf("quick fix = %q preferred=%v diagnostics=%d", a.Title, a.IsPreferred, len(a.Diagnostics))
	}

	got := applyEdits(t, text, a, uri)
	if want := cliRewrite(t, root, rel); got != want {
		t.Fatalf("the quick fix wrote:\n%s\nmemqlmigrate writes:\n%s", got, want)
	}
	if !strings.Contains(got, "  filter  row => row.status == args.status || isAdmin(actor)\n") {
		t.Errorf("isAdmin is not applied to actor -- the predicate set is not the tree's:\n%s", got)
	}
	if errs := syntaxErrors(got); len(errs) != 0 {
		t.Errorf("the rewritten document does not parse: %+v", errs)
	}

	// Minimal: the one changed line is the one edit, a whole-line range.
	edits := a.Edit.Changes[uri]
	if len(edits) != 1 {
		t.Fatalf("want one edit, got %d: %+v", len(edits), edits)
	}
	filterLine := protocol.UInteger(strings.Count(text[:strings.Index(text, "  filter")], "\n"))
	if r := edits[0].Range; r.Start != (protocol.Position{Line: filterLine}) || r.End != (protocol.Position{Line: filterLine + 1}) {
		t.Errorf("the edit spans %+v, want exactly line %d", r, filterLine)
	}
}

// The whole file at once, across every kind of clause the codemod converts
// -- a filter, a trait body, a trigger @filter over a bare field, a logic body
// with retired calls, null and the key-less map entry of onDelegationCreated --
// equals the CLI's output and parses clean. Only changed lines are edited.
func TestCodeAction_FixAllWritesTheCLIRewrite(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	const rel = "fylo/logic.memql"
	files[rel] = "use fylo.concepts.{ order }\n" +
		"\n" +
		"// Orders that are still open.\n" +
		"query order openOrders {\n" +
		"  filter  status == \"open\" && isAdmin\n" +
		"}\n" +
		"\n" +
		"trait isShipped {\n" +
		"  return status == \"shipped\"\n" +
		"}\n" +
		"\n" +
		"@trigger(event=\"node.updated\", concept=\"v1:fylo:order\", partition=\"*\")\n" +
		"@filter(status == \"archived\")\n" +
		"automation onArchived {\n" +
		"  step s {\n" +
		"    logic recordArchive ( event )\n" +
		"  }\n" +
		"}\n" +
		"\n" +
		"logic recordArchive {\n" +
		"  args {\n" +
		"    event object @required\n" +
		"  }\n" +
		"  body {\n" +
		"    note := cond(args.event.payload.note == null, \"none\", concat(\"note: \", args.event.payload.note))\n" +
		"    emitEvent := publishEvent(\n" +
		"      topic: \"order.archived\",\n" +
		"      payload: {\n" +
		"        orderId: args.event.payload.id,\n" +
		"        args.event.payload.ownerUserId,\n" +
		"        note: note\n" +
		"      }\n" +
		"    )\n" +
		"    return emitEvent\n" +
		"  }\n" +
		"}\n"
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, _ := openWorkspaceDoc(t, s, root, rel)

	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatalf("want one source.fixAll.memql action, got %d", len(all))
	}
	got := applyEdits(t, text, all[0], uri)
	want := cliRewrite(t, root, rel)
	if got != want {
		t.Fatalf("fix-all wrote:\n%s\nmemqlmigrate writes:\n%s", got, want)
	}
	if errs := syntaxErrors(got); len(errs) != 0 {
		t.Errorf("the rewritten document does not parse: %+v", errs)
	}
	for _, frag := range []string{
		"@filter(row => row.status == \"archived\")",
		"ownerUserId: args.event.payload.ownerUserId",
		"trait isShipped = row => row.status == \"shipped\"",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("the rewrite lacks %s:\n%s", frag, got)
		}
	}

	// Every edited line is a changed line: no edit replaces a line with
	// itself, and the lines no edit touches are the lines the rewrite kept.
	oldLines := splitLines(text)
	for _, e := range all[0].Edit.Changes[uri] {
		replaced := strings.Join(oldLines[e.Range.Start.Line:min(int(e.Range.End.Line), len(oldLines))], "")
		if replaced == e.NewText {
			t.Errorf("an edit rewrites lines %d-%d to themselves", e.Range.Start.Line, e.Range.End.Line)
		}
	}
	if hunks := len(all[0].Edit.Changes[uri]); hunks < 4 {
		t.Errorf("want one edit per changed run of lines, got %d", hunks)
	}
}

// A clause the codemod refuses gets no action and keeps its diagnostic; the
// region beside it is still rewritable, and fix-all rewrites only that one.
func TestCodeAction_RefusedClauseGetsNoAction(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	const rel = "fylo/folders.memql"
	refused := "query order folders {\n  filter  childOf(kind == \"folder\")\n}\n"
	files[rel] = refused + "\nquery order open {\n  filter  status == \"open\"\n}\n"
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, diags := openWorkspaceDoc(t, s, root, rel)

	if plan, _ := langparser.PlanExpressions([]byte(text), nil); len(plan.Refused) != 1 {
		t.Fatalf("the codemod does not refuse exactly the traversal: %+v", plan.Refused)
	}
	d, ok := diagnosticWithCode(diags, "retired_filter_without_lambda")
	if !ok || d.Range.Start.Line > 2 {
		t.Fatalf("want the refused query's diagnostic first, got %+v", diags)
	}
	if quick := actionsOfKind(codeActions(t, s, uri, d.Range, []protocol.Diagnostic{d}), protocol.CodeActionKindQuickFix); len(quick) != 0 {
		t.Fatalf("a refused clause was offered %d quick fixes: %+v", len(quick), quick)
	}

	// The same diagnostic on the second query is fixable.
	second := d
	second.Range.Start.Line, second.Range.End.Line = 5, 5
	if quick := actionsOfKind(codeActions(t, s, uri, second.Range, []protocol.Diagnostic{second}), protocol.CodeActionKindQuickFix); len(quick) != 1 {
		t.Errorf("the rewritable query beside a refused one was offered %d quick fixes", len(quick))
	}

	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatalf("want one fix-all action, got %d", len(all))
	}
	got := applyEdits(t, text, all[0], uri)
	if !strings.HasPrefix(got, refused) {
		t.Errorf("fix-all touched the refused query:\n%s", got)
	}
	if !strings.Contains(got, "  filter  row => row.status == \"open\"\n") {
		t.Errorf("fix-all left the rewritable query alone:\n%s", got)
	}
	if errs := syntaxErrors(got); len(errs) != 1 || errs[0].Code != "retired_filter_without_lambda" || errs[0].Range.Start.Line > 3 {
		t.Errorf("the refused query's diagnostic must stay exactly as it was, got %+v", errs)
	}
}

// A region holding a refused clause offers nothing even where what is left
// would parse. The handler's placeholder sits inside a string of its query,
// which no parse reads -- the codemod refuses it because rewriting it would
// silently turn it into literal text -- and the @filter above the terse
// automation shares its region (no blank line, no closing brace between), so
// the @filter's fix is withheld too.
func TestCodeAction_RegionWithARefusalOffersNothing(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	const rel = "fylo/tools.memql"
	files[rel] = "@filter(payload.kind == \"file\")\n" +
		"automation onFile @trigger(event=\"node.created\", concept=\"v1:fylo:order\", partition=\"*\") => logic record\n" +
		"@description(\"Search orders by name\")\n" +
		"@handler(type=\"query\", query=\"concept==v1:fylo:order && name==\\\"order-$args.q\\\"\")\n" +
		"tool searchOrders {\n" +
		"  q string @description(\"name\")\n" +
		"}\n"
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, _ := openWorkspaceDoc(t, s, root, rel)

	plan, err := langparser.PlanExpressions([]byte(text), nil)
	if err != nil || len(plan.Edits) != 1 || len(plan.Refused) != 1 || !strings.Contains(plan.Refused[0].Err.Error(), "inside a string literal") {
		t.Fatalf("want the @filter converted and the handler refused, got %+v (%v)", plan, err)
	}
	diag := protocol.Diagnostic{Code: &protocol.IntegerOrString{Value: "retired_filter_annotation"}, Range: protocol.Range{Start: protocol.Position{Line: 0}}}
	if actions := codeActions(t, s, uri, diag.Range, []protocol.Diagnostic{diag}); len(actions) != 0 {
		t.Errorf("a region holding a refused clause offered %d actions", len(actions))
	}
}

// Nothing is offered that would not parse. The codemod converts the cond(...)
// in this logic body, but the body also passes an object literal as a call's
// arguments -- a form refused at parse whatever the edition -- so the region's
// rewrite still reports a syntax error and offers nothing, while the query
// beside it is fixed.
func TestCodeAction_NoEditThatWouldNotParse(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	const rel = "fylo/broken.memql"
	broken := "logic recordArchive {\n" +
		"  args {\n" +
		"    event object @required\n" +
		"  }\n" +
		"  note := cond(args.event.payload.note == null, \"none\", \"some\")\n" +
		"  emitted := builtin emitRecord({ note: note })\n" +
		"  return emitted\n" +
		"}\n"
	files[rel] = broken + "\nquery order open {\n  filter  status == \"open\"\n}\n"
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, diags := openWorkspaceDoc(t, s, root, rel)

	if plan, err := langparser.PlanExpressions([]byte(text), nil); err != nil || len(plan.Edits) != 2 || len(plan.Refused) != 0 {
		t.Fatalf("want the codemod to convert both regions, got %+v (%v)", plan, err)
	}
	d, ok := diagnosticWithCode(diags, "retired_cond_call")
	if !ok {
		t.Fatalf("no retired_cond_call diagnostic was published: %+v", diags)
	}
	if quick := actionsOfKind(codeActions(t, s, uri, d.Range, []protocol.Diagnostic{d}), protocol.CodeActionKindQuickFix); len(quick) != 0 {
		t.Fatalf("offered a rewrite that does not parse: %+v", quick)
	}
	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatalf("want one fix-all action for the query, got %d", len(all))
	}
	if got := applyEdits(t, text, all[0], uri); !strings.HasPrefix(got, broken) || !strings.Contains(got, "filter  row => row.status == \"open\"") {
		t.Errorf("fix-all must rewrite the query and leave the logic alone:\n%s", got)
	}
}

// The predicate set reads the open buffers, not the saved files: a spec typed
// into another open file, not yet saved, decides the rewrite.
func TestCodeAction_PredicatesReadOpenBuffers(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	files["fylo/queries.memql"] = strings.Replace(files["fylo/queries.memql"], "|| isAdmin", "|| isOwnerRole", 1)
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, _ := openWorkspaceDoc(t, s, root, "fylo/queries.memql")

	// On disk, isOwnerRole is declared nowhere: the codemod refuses the
	// clause, and so the fix is not offered.
	if all := codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"); len(all) != 0 {
		t.Fatalf("offered a rewrite naming an undeclared predicate: %+v", all)
	}

	specsURI, _, _ := openWorkspaceDoc(t, s, root, "fylo/specs.memql")
	unsaved := files["fylo/specs.memql"] + "\nspec actorEnvelope isOwnerRole {\n  return role == \"owner\"\n}\n"
	if err := s.didChange(noopCtx(), &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: specsURI}},
		ContentChanges: []any{protocol.TextDocumentContentChangeEventWhole{Text: unsaved}},
	}); err != nil {
		t.Fatal(err)
	}
	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatalf("the unsaved spec did not reach the predicate set")
	}
	if got := applyEdits(t, text, all[0], uri); !strings.Contains(got, "isOwnerRole(actor)") {
		t.Errorf("isOwnerRole is not applied to actor:\n%s", got)
	}
}

// A bundle's filter naming a core trait is rewritable: the predicate set
// holds the core domains the workspace does not carry, as the engine's flat
// registry does when the bundle loads over the core tree.
func TestCodeAction_CorePredicatesResolve(t *testing.T) {
	files := map[string]string{}
	for k, v := range codemodWorkspace {
		files[k] = v
	}
	const rel = "fylo/active.memql"
	files[rel] = "query order activeOrders {\n  filter  isActiveRecord && status == \"open\"\n}\n"
	root := writeWorkspace(t, files)
	s := workspaceServer(t, root)
	uri, text, _ := openWorkspaceDoc(t, s, root, rel)
	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatal("a filter naming the core trait isActiveRecord is not rewritable")
	}
	if got := applyEdits(t, text, all[0], uri); !strings.Contains(got, "filter  row => isActiveRecord(row) && row.status == \"open\"") {
		t.Errorf("rewrote:\n%s", got)
	}
}

// A bundle's spec over the core @actor shape actorEnvelope, which the bundle
// does not declare, is rewritten as an actor predicate -- the workspace
// resolves over the core tree, as memqlmigrate does.
func TestCodeAction_SpecOverACoreActorShape(t *testing.T) {
	const rel = "fylo/specs.memql"
	root := writeWorkspace(t, map[string]string{
		"fylo/concepts.memql": "concept order {\n  status  string\n}\n",
		rel:                   "use common.shapes.{ actorEnvelope }\n\nspec actorEnvelope isAdmin {\n  return role == \"admin\"\n}\n\nquery order adminOrders {\n  filter  isAdmin\n}\n",
	})
	s := workspaceServer(t, root)
	uri, text, _ := openWorkspaceDoc(t, s, root, rel)
	all := actionsOfKind(codeActions(t, s, uri, protocol.Range{}, nil, "source.fixAll.memql"), "source.fixAll.memql")
	if len(all) != 1 {
		t.Fatal("no fix-all for a spec over the core actorEnvelope")
	}
	got := applyEdits(t, text, all[0], uri)
	if want := cliRewrite(t, root, rel); got != want {
		t.Errorf("fix-all wrote:\n%s\nmemqlmigrate writes:\n%s", got, want)
	}
	for _, line := range []string{"spec actorEnvelope isAdmin = actor => actor.role == \"admin\"\n", "  filter  row => isAdmin(actor)\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("the rewrite lacks %q:\n%s", line, got)
		}
	}
}

func TestTopLevelRegions(t *testing.T) {
	text := "use a.b.{ c }\n" + //                  0
		"use a.d.{ e }\n" + //                           1
		"\n" + //                                         2
		"// doc\n" + //                                   3
		"@description(\"x\",\n" + //                    4  an annotation over two lines
		"\n" + //                                         5  blank, but inside its parentheses
		"  more)\n" + //                                 6
		"query q {\n" + //                               7
		"\n" + //                                         8  blank, but inside the braces
		"  filter \"}\" == x // }\n" + //               9  a brace in a string and a comment
		"}\n" + //                                        10
		"query r { }\n" + //                             11 back to back: its own region
		"\n" + //                                         12
		"automation t @trigger(schedule=\"x\") => logic l" // 13 no newline at the end
	var got []string
	for _, rg := range topLevelRegions(text) {
		first := strings.Count(text[:rg.start], "\n")
		last := strings.Count(text[:rg.end-1], "\n")
		got = append(got, strconv.Itoa(first)+"-"+strconv.Itoa(last))
	}
	want := []string{"0-0", "1-1", "3-10", "11-11", "13-13"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("regions = %v, want %v", got, want)
	}
}

func TestLineEdits(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		want           []protocol.TextEdit
	}{
		{"one line", "a\nb\nc\n", "a\nB\nc\n", []protocol.TextEdit{
			{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 2}}, NewText: "B\n"},
		}},
		{"two runs", "a\nb\nc\nd\ne\n", "a\nB\nc\nD\nE\ne\n", []protocol.TextEdit{
			{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 2}}, NewText: "B\n"},
			{Range: protocol.Range{Start: protocol.Position{Line: 3}, End: protocol.Position{Line: 4}}, NewText: "D\nE\n"},
		}},
		{"lines removed", "a\nb\nc\nd\n", "a\nd\n", []protocol.TextEdit{
			{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 3}}, NewText: ""},
		}},
		{"last line without a newline", "a\nb", "a\nbé", []protocol.TextEdit{
			{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1, Character: 1}}, NewText: "bé"},
		}},
		{"unchanged", "a\nb\n", "a\nb\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lineEdits(tc.old, 0, tc.old, tc.new)
			if len(got) != len(tc.want) {
				t.Fatalf("edits = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("edit %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			a := protocol.CodeAction{Title: tc.name, Edit: &protocol.WorkspaceEdit{Changes: map[protocol.DocumentUri][]protocol.TextEdit{"u": got}}}
			if len(got) > 0 {
				if applied := applyEdits(t, tc.old, a, "u"); applied != tc.new {
					t.Errorf("applied = %q, want %q", applied, tc.new)
				}
			}
		})
	}
}
