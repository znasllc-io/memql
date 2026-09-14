package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
	"github.com/znasllc-io/memql/core/dslfs"
)

// languageline_test.go -- the editor on a workspace whose DSL has not declared
// its language line (memql#5362; the editor half of epic memql#5356's final
// review). Every product repository is that workspace until it migrates.

// gadgetConcept is a domain's only file: a concept that loads clean, so the
// domain can be refused for nothing but its language line.
const gadgetConcept = `@version("1.0.0")
@namespace("gadgets")
@description("A gadget.")
concept gadget {
  label string @required @description("Label")
}
`

// probeTrait is a construct that loads clean in any domain, for fixtures that
// only resolve language lines.
const probeTrait = "@enabled\ntrait languageLineProbe {\n  return active == true\n}\n"

// vscodeInitialize is the part of VS Code's initialize request this server
// reads: it applies versioned document changes, and it creates files.
const vscodeInitialize = `{"processId":null,"rootUri":null,"capabilities":{"workspace":{"workspaceEdit":` +
	`{"documentChanges":true,"resourceOperations":["create","rename","delete"]}}}}`

// declaration is the text the quick fix writes: the line this engine reads.
func declaration() string {
	return dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
}

// writeFiles writes files under root, creating their directories.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// jsonOf is v as it goes over the wire.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// lspClient drives the server the way glsp's own server does -- a method name
// and raw JSON params through the wrapped handler -- and records what the
// server sends back. Every request is therefore decoded exactly as a real
// client's is, which is what catches glsp dropping a diagnostic's code on the
// way in (languageline.go).
type lspClient struct {
	t *testing.T
	h *customHandler

	mu        sync.Mutex
	published map[protocol.DocumentUri][]protocol.PublishDiagnosticsParams
	messages  []protocol.ShowMessageParams
}

func (c *lspClient) notify(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case protocol.ServerTextDocumentPublishDiagnostics:
		p := params.(protocol.PublishDiagnosticsParams)
		c.published[p.URI] = append(c.published[p.URI], p)
	case protocol.ServerWindowShowMessage:
		c.messages = append(c.messages, params.(protocol.ShowMessageParams))
	}
}

// call sends one request or notification and returns the server's result.
func (c *lspClient) call(method, params string) any {
	c.t.Helper()
	res, validMethod, validParams, err := c.h.Handle(&glsp.Context{
		Method: method,
		Params: json.RawMessage(params),
		Notify: c.notify,
	})
	if err != nil || !validMethod || !validParams {
		c.t.Fatalf("%s: validMethod=%v validParams=%v err=%v", method, validMethod, validParams, err)
	}
	return res
}

// diagnostics is the last set published for a document.
func (c *lspClient) diagnostics(uri protocol.DocumentUri) []protocol.Diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := c.published[uri]
	if len(all) == 0 {
		c.t.Fatalf("nothing was ever published for %s", uri)
	}
	return all[len(all)-1].Diagnostics
}

func (c *lspClient) shown() []protocol.ShowMessageParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.ShowMessageParams(nil), c.messages...)
}

// newLanguageLineClient is a server over root, with its rebuilds on a clock the
// test fires, and a client connected to it.
func newLanguageLineClient(t *testing.T, root string) (*lspClient, *fakeClock) {
	t.Helper()
	commonlog.Configure(-4, nil)
	s := newServer(root, commonlog.GetLogger(lsName))
	clock := &fakeClock{}
	s.rebuild = newRebuildDebouncerWithTimers(rebuildDebounce, clock.afterFunc)
	return &lspClient{
		t:         t,
		h:         newCustomHandler(s),
		published: map[protocol.DocumentUri][]protocol.PublishDiagnosticsParams{},
	}, clock
}

func didOpenParams(t *testing.T, uri, text string) string {
	return jsonOf(t, map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "memql", "version": 1, "text": text,
	}})
}

// watchedParams is a workspace/didChangeWatchedFiles notification for one
// file: 1 created, 2 changed, 3 deleted.
func watchedParams(t *testing.T, uri string, kind int) string {
	return jsonOf(t, map[string]any{"changes": []any{map[string]any{"uri": uri, "type": kind}}})
}

// codeActionParams is the request VS Code sends from the lightbulb on a
// diagnostic: the diagnostic echoed back in the context, code included.
func codeActionParams(t *testing.T, uri string, d protocol.Diagnostic, only ...string) string {
	ctx := map[string]any{"diagnostics": []any{d}}
	if len(only) > 0 {
		ctx["only"] = only
	}
	return jsonOf(t, map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"range":        d.Range,
		"context":      ctx,
	})
}

func completionParams(t *testing.T, uri string, line, char int) string {
	return jsonOf(t, map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	})
}

// withCode is the diagnostic carrying code, if any.
func withCode(diags []protocol.Diagnostic, code string) (protocol.Diagnostic, bool) {
	for _, d := range diags {
		if d.Code != nil && d.Code.Value == code {
			return d, true
		}
	}
	return protocol.Diagnostic{}, false
}

// offersConcept reports whether completion offers the concept named name.
func offersConcept(items []protocol.CompletionItem, name string) bool {
	for _, it := range items {
		if it.Label == name && it.Kind != nil && *it.Kind == protocol.CompletionItemKindClass {
			return true
		}
	}
	return false
}

// TestLanguageLine_AnUnmigratedWorkspaceSaysWhyAndClearsWhenFixed is the
// author's whole path through memql#5362, over the protocol, on a workspace
// holding one domain with no memql.toml:
//
//  1. an open file of the domain carries the refusal on its first line;
//  2. the quick fix for it writes the file, with exactly these documentChanges;
//  3. one notification says completion and hover are off, and why;
//  4. a rebuild that fails the same way says nothing new;
//  5. once the file exists and the watcher reports it, the rebuild clears the
//     refusal and completion offers the registry again;
//  6. a later failure, after that success, is announced again.
//
// The brief numbers "the same cause says nothing new" last; it runs before the
// fix here, because after a successful build the next failure is news by
// design, which is what step 6 asserts.
func TestLanguageLine_AnUnmigratedWorkspaceSaysWhyAndClearsWhenFixed(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"gadgets/concepts.memql": gadgetConcept})
	c, clock := newLanguageLineClient(t, root)

	conceptsURI := pathToURI(filepath.Join(root, "gadgets", "concepts.memql"))
	// A new file of the domain, still unsaved: the one completion is asked in.
	queriesURI := pathToURI(filepath.Join(root, "gadgets", "queries.memql"))
	manifestFile := filepath.Join(root, "gadgets", dslfs.ManifestFile)
	manifestURI := pathToURI(manifestFile)

	// The refusal as the engine words it, read from the engine rather than
	// restated: what is under test is what the server does with the message,
	// not its text.
	engine := memql.ResolveWorkspaceLanguageLines(os.DirFS(root)).Problems
	if len(engine) != 1 || engine[0].Code != langparser.CodeLanguageLineMissing {
		t.Fatalf("the engine's refusals = %+v; want one %s", engine, langparser.CodeLanguageLineMissing)
	}
	refusal := engine[0]

	c.call(protocol.MethodInitialize, vscodeInitialize)
	c.call(protocol.MethodInitialized, `{}`)
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, conceptsURI, gadgetConcept))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, queriesURI, "query "))

	// 1. The refusal, on the open file's first line.
	diag, ok := withCode(c.diagnostics(conceptsURI), langparser.CodeLanguageLineMissing)
	if !ok {
		t.Fatalf("step 1: no %s diagnostic on gadgets/concepts.memql; published %s",
			langparser.CodeLanguageLineMissing, jsonOf(t, c.diagnostics(conceptsURI)))
	}
	firstLine := len(`@version("1.0.0")`)
	if diag.Range.Start != (protocol.Position{}) || diag.Range.End != (protocol.Position{Line: 0, Character: protocol.UInteger(firstLine)}) {
		t.Errorf("step 1: range = %+v; want the whole first line, 0:0-0:%d", diag.Range, firstLine)
	}
	if diag.Severity == nil || *diag.Severity != protocol.DiagnosticSeverityError {
		t.Errorf("step 1: severity = %v; want Error", diag.Severity)
	}
	if diag.Source == nil || *diag.Source != lsName {
		t.Errorf("step 1: source = %v; want %q, the source of every diagnostic this server publishes", diag.Source, lsName)
	}
	if diag.Message != refusal.Message || !strings.Contains(diag.Message, `"gadgets"`) {
		t.Errorf("step 1: message = %q; want the engine's refusal of domain \"gadgets\", %q", diag.Message, refusal.Message)
	}
	if _, ok := withCode(c.diagnostics(queriesURI), langparser.CodeLanguageLineMissing); !ok {
		t.Error("step 1: the domain's other open file carries no refusal; every file of the domain should")
	}

	// 2. The quick fix, requested the way VS Code's lightbulb requests it.
	res := c.call(protocol.MethodTextDocumentCodeAction, codeActionParams(t, conceptsURI, diag))
	wantChanges := `[{"kind":"create","uri":` + jsonOf(t, manifestURI) + `,"options":{"ignoreIfExists":true}},` +
		`{"textDocument":{"uri":` + jsonOf(t, manifestURI) + `,"version":null},` +
		`"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":` +
		jsonOf(t, declaration()) + `}]}]`
	wantActions := `[{"title":"Create gadgets/memql.toml","kind":"quickfix","diagnostics":[` + jsonOf(t, diag) + `],` +
		`"isPreferred":true,"edit":{"documentChanges":` + wantChanges + `}}]`
	if got := jsonOf(t, res); got != wantActions {
		t.Errorf("step 2: code actions\n got: %s\nwant: %s", got, wantActions)
	}

	// 3. One notification, naming the domain and the fix.
	const wantNotice = `MemQL completion and hover are off: domain "gadgets" has no memql.toml. ` +
		`Use the quick fix on any of its files, or add gadgets/memql.toml.`
	shown := c.shown()
	if len(shown) != 1 {
		t.Fatalf("step 3: %d notifications; want exactly one: %+v", len(shown), shown)
	}
	if shown[0].Type != protocol.MessageTypeWarning || shown[0].Message != wantNotice {
		t.Errorf("step 3: notification = {%v %q}; want {Warning %q}", shown[0].Type, shown[0].Message, wantNotice)
	}
	// What the notification says is off is off: the concept slot, which only
	// the registry fills, offers nothing.
	if items, _ := c.call(protocol.MethodTextDocumentCompletion, completionParams(t, queriesURI, 0, 6)).([]protocol.CompletionItem); len(items) != 0 {
		t.Errorf("step 3: completion offers %d items at the concept slot; the build that failed has no registry to offer them from", len(items))
	}

	// 4. An edit elsewhere rebuilds, and the build fails the same way: the
	// refusal is published again and nothing new is announced.
	c.call(protocol.MethodWorkspaceDidChangeWatchedFiles, watchedParams(t, conceptsURI, 2))
	if n := clock.fire(); n != 1 {
		t.Fatalf("step 4: %d rebuilds ran; want the one the watcher scheduled", n)
	}
	if _, ok := withCode(c.diagnostics(conceptsURI), langparser.CodeLanguageLineMissing); !ok {
		t.Error("step 4: the refusal vanished on a rebuild that failed the same way")
	}
	if n := len(c.shown()); n != 1 {
		t.Errorf("step 4: %d notifications after a second failure with the same cause; want still 1", n)
	}

	// 5. Write the file the quick fix writes. The client's watcher reports it
	// (editors/vscode/src/extension.ts watches **/memql.toml), and the rebuild
	// clears the refusal and brings the registry back -- no reload.
	writeFiles(t, root, map[string]string{"gadgets/" + dslfs.ManifestFile: declaration()})
	c.call(protocol.MethodWorkspaceDidChangeWatchedFiles, watchedParams(t, manifestURI, 1))
	if n := clock.fire(); n != 1 {
		t.Fatalf("step 5: %d rebuilds ran; want the one the watcher scheduled", n)
	}
	for _, uri := range []string{conceptsURI, queriesURI} {
		if d, stale := withCode(c.diagnostics(uri), langparser.CodeLanguageLineMissing); stale {
			t.Errorf("step 5: %s still carries the refusal after the fix: %q", uri, d.Message)
		}
	}
	// The registry is back, and it now holds the domain it refused: the
	// domain's own concept is on offer in the domain's file.
	items, _ := c.call(protocol.MethodTextDocumentCompletion, completionParams(t, queriesURI, 0, 6)).([]protocol.CompletionItem)
	if !offersConcept(items, "gadget") {
		t.Errorf("step 5: completion does not offer the domain's concept gadget after the fix; got %d items", len(items))
	}
	if n := len(c.shown()); n != 1 {
		t.Errorf("step 5: %d notifications after a build that succeeded; want still 1", n)
	}

	// 6. The line goes missing again after a success: that is news.
	if err := os.Remove(manifestFile); err != nil {
		t.Fatal(err)
	}
	c.call(protocol.MethodWorkspaceDidChangeWatchedFiles, watchedParams(t, manifestURI, 3))
	clock.fire()
	if shown := c.shown(); len(shown) != 2 || shown[1].Message != wantNotice {
		t.Errorf("step 6: notifications = %+v; want a second one, %q", shown, wantNotice)
	}
}

// languageLineServer is a server over root whose build is the workspace's
// language lines and a registry-less Sense service -- what a failed build
// leaves -- without a full build's cost.
func languageLineServer(t *testing.T, root string, createsFiles bool) (*server, *lspClient) {
	t.Helper()
	commonlog.Configure(-4, nil)
	s := newServer(root, commonlog.GetLogger(lsName))
	s.setBuild(sense.New(nil), memql.ResolveWorkspaceLanguageLines(os.DirFS(root)))
	s.createsFiles.Store(createsFiles)
	h := newCustomHandler(s)
	h.Handler.SetInitialized(true)
	return s, &lspClient{t: t, h: h, published: map[protocol.DocumentUri][]protocol.PublishDiagnosticsParams{}}
}

// TestLanguageLine_OnlyARefusedMountedDomainsFilesCarryTheRefusal: of the
// files a workspace can hold, exactly the ones in a mounted domain whose line
// is refused get the diagnostic. A core domain speaks the engine's line, and a
// directory the build does not mount is none of the build's business.
func TestLanguageLine_OnlyARefusedMountedDomainsFilesCarryTheRefusal(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"gadgets/traits.memql":          probeTrait,
		"widgets/traits.memql":          probeTrait,
		"widgets/" + dslfs.ManifestFile: declaration(),
		"identity/traits.memql":         probeTrait,
		"fixtures/deep/traits.memql":    probeTrait,
		"loose.memql":                   probeTrait,
	})
	_, c := languageLineServer(t, root, true)

	cases := []struct {
		name, uri string
		want      bool
	}{
		{"a file of the refused domain", pathToURI(filepath.Join(root, "gadgets", "traits.memql")), true},
		{"a file below the refused domain's top level", pathToURI(filepath.Join(root, "gadgets", "more", "x.memql")), true},
		{"a file of a domain that declares its line", pathToURI(filepath.Join(root, "widgets", "traits.memql")), false},
		{"a file of a core domain", pathToURI(filepath.Join(root, "identity", "traits.memql")), false},
		{"a file of a directory the build does not mount", pathToURI(filepath.Join(root, "fixtures", "deep", "traits.memql")), false},
		{"a file at the workspace root", pathToURI(filepath.Join(root, "loose.memql")), false},
		{"a file outside the workspace", pathToURI(filepath.Join(t.TempDir(), "gadgets", "traits.memql")), false},
		{"a buffer that is not a file", "untitled:Untitled-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.t = t
			c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, tc.uri, probeTrait))
			_, got := withCode(c.diagnostics(tc.uri), langparser.CodeLanguageLineMissing)
			if got != tc.want {
				t.Errorf("carries the refusal = %v; want %v (published %s)", got, tc.want, jsonOf(t, c.diagnostics(tc.uri)))
			}
		})
	}
}

// TestLanguageLine_AClosingFileGivesUpItsRefusalAndKeepsTheRest: a rebuild
// republishes open files only, so the refusal comes off a file when it closes
// -- left there, it would still say "no memql.toml" after the fix -- while the
// file's own diagnostics stay, as they always did.
func TestLanguageLine_AClosingFileGivesUpItsRefusalAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"gadgets/traits.memql":          probeTrait,
		"widgets/traits.memql":          probeTrait,
		"widgets/" + dslfs.ManifestFile: declaration(),
	})
	_, c := languageLineServer(t, root, true)
	closeParams := func(uri string) string {
		return jsonOf(t, map[string]any{"textDocument": map[string]any{"uri": uri}})
	}

	// A buffer with an error of its own: a logic missing its body.
	const broken = "logic oops {\n"
	refused := pathToURI(filepath.Join(root, "gadgets", "logic.memql"))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, refused, broken))
	open := c.diagnostics(refused)
	if _, ok := withCode(open, langparser.CodeLanguageLineMissing); !ok || len(open) < 2 {
		t.Fatalf("open: published %s; want the refusal beside the buffer's own error", jsonOf(t, open))
	}
	c.call(protocol.MethodTextDocumentDidClose, closeParams(refused))
	closed := c.diagnostics(refused)
	if _, ok := withCode(closed, langparser.CodeLanguageLineMissing); ok {
		t.Errorf("closed: the refusal is still published on a file the server no longer keeps current")
	}
	if len(closed) != len(open)-1 {
		t.Errorf("closed: published %s; want the buffer's own %d diagnostic(s) kept", jsonOf(t, closed), len(open)-1)
	}

	// A file with no refusal to withdraw is left as it was published.
	accepted := pathToURI(filepath.Join(root, "widgets", "logic.memql"))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, accepted, broken))
	before := len(c.published[accepted])
	c.call(protocol.MethodTextDocumentDidClose, closeParams(accepted))
	if after := len(c.published[accepted]); after != before {
		t.Errorf("closing a file of an accepted domain published %d more time(s); want none", after-before)
	}
}

// TestLanguageLine_ARepositoryRootNamesTheDomainsRealPath: from a repository
// whose domains sit under dsl/, the refusal still lands on the domain's files,
// and the quick fix and the notification name the file from the workspace
// root -- the path the author will find it at.
func TestLanguageLine_ARepositoryRootNamesTheDomainsRealPath(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"dsl/gadgets/traits.memql":          probeTrait,
		"dsl/widgets/traits.memql":          probeTrait,
		"dsl/widgets/" + dslfs.ManifestFile: declaration(),
		"README.md":                         "# a product\n",
	})
	s, c := languageLineServer(t, root, true)

	uri := pathToURI(filepath.Join(root, "dsl", "gadgets", "traits.memql"))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, uri, probeTrait))
	diag, ok := withCode(c.diagnostics(uri), langparser.CodeLanguageLineMissing)
	if !ok {
		t.Fatalf("no refusal on dsl/gadgets/traits.memql; published %s", jsonOf(t, c.diagnostics(uri)))
	}
	actions, _ := c.call(protocol.MethodTextDocumentCodeAction, codeActionParams(t, uri, diag)).([]protocol.CodeAction)
	if len(actions) != 1 || actions[0].Title != "Create dsl/gadgets/memql.toml" {
		t.Fatalf("actions = %s; want one, \"Create dsl/gadgets/memql.toml\"", jsonOf(t, actions))
	}
	wantURI := pathToURI(filepath.Join(root, "dsl", "gadgets", dslfs.ManifestFile))
	if create, ok := actions[0].Edit.DocumentChanges[0].(protocol.CreateFile); !ok || create.URI != wantURI {
		t.Errorf("the quick fix creates %s; want %s", jsonOf(t, actions[0].Edit.DocumentChanges[0]), wantURI)
	}
	_, lines := s.getBuild()
	const want = `MemQL completion and hover are off: domain "gadgets" has no memql.toml. ` +
		`Use the quick fix on any of its files, or add dsl/gadgets/memql.toml.`
	if got := buildFailureNotice(lines, true); got != want {
		t.Errorf("notice = %q; want %q", got, want)
	}
}

// TestLanguageLine_TheQuickFixIsOnlyForItsOwnDiagnostic: the action is offered
// for the missing-line refusal this server published, to a client that can
// create the file, while the file is still absent -- and in no other case.
func TestLanguageLine_TheQuickFixIsOnlyForItsOwnDiagnostic(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"gadgets/traits.memql":   probeTrait,
		"sprockets/traits.memql": probeTrait,
		// Newer than this engine: refused, but not for a missing file.
		"sprockets/" + dslfs.ManifestFile: "memql = \"99.0\"\nedition = \"" + langparser.Edition + "\"\n",
	})
	gadgets := pathToURI(filepath.Join(root, "gadgets", "traits.memql"))
	sprockets := pathToURI(filepath.Join(root, "sprockets", "traits.memql"))

	s, c := languageLineServer(t, root, true)
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, gadgets, probeTrait))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, sprockets, probeTrait))
	missing, ok := withCode(c.diagnostics(gadgets), langparser.CodeLanguageLineMissing)
	if !ok {
		t.Fatal("no missing-line refusal on gadgets/traits.memql")
	}
	newer, ok := withCode(c.diagnostics(sprockets), langparser.CodeLanguageVersionNewer)
	if !ok {
		t.Fatalf("no %s refusal on sprockets/traits.memql; published %s", langparser.CodeLanguageVersionNewer, jsonOf(t, c.diagnostics(sprockets)))
	}

	actionsFor := func(t *testing.T, c *lspClient, uri string, d protocol.Diagnostic, only ...string) []protocol.CodeAction {
		t.Helper()
		c.t = t
		actions, _ := c.call(protocol.MethodTextDocumentCodeAction, codeActionParams(t, uri, d, only...)).([]protocol.CodeAction)
		return actions
	}

	if got := actionsFor(t, c, gadgets, missing); len(got) != 1 {
		t.Fatalf("the refusal's own diagnostic gets %d actions; want 1 -- every case below is measured against it", len(got))
	}

	other := missing
	other.Message = "some other problem on the first line"
	foreign := missing
	foreign.Source = ptr("another-server")

	for _, tc := range []struct {
		name string
		uri  string
		diag protocol.Diagnostic
		only []string
		want int
	}{
		{"a filter that asks for quick fixes", gadgets, missing, []string{"quickfix"}, 1},
		{"a filter that asks for every kind", gadgets, missing, []string{""}, 1},
		{"a filter that asks for source actions", gadgets, missing, []string{"source.organizeImports"}, 0},
		{"a filter that asks for refactorings", gadgets, missing, []string{"refactor"}, 0},
		{"a diagnostic with another message", gadgets, other, nil, 0},
		{"another server's diagnostic", gadgets, foreign, nil, 0},
		{"a refusal for a line newer than the engine's", sprockets, newer, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionsFor(t, c, tc.uri, tc.diag, tc.only...); len(got) != tc.want {
				t.Errorf("%d actions; want %d: %s", len(got), tc.want, jsonOf(t, got))
			}
		})
	}

	t.Run("a client that cannot create files", func(t *testing.T) {
		s.createsFiles.Store(false)
		defer s.createsFiles.Store(true)
		if got := actionsFor(t, c, gadgets, missing); len(got) != 0 {
			t.Errorf("%d actions; want none -- the edit would fail when chosen", len(got))
		}
	})

	t.Run("a memql.toml written since the last build", func(t *testing.T) {
		writeFiles(t, root, map[string]string{"gadgets/" + dslfs.ManifestFile: declaration()})
		defer os.Remove(filepath.Join(root, "gadgets", dslfs.ManifestFile))
		if got := actionsFor(t, c, gadgets, missing); len(got) != 0 {
			t.Errorf("%d actions; want none -- the file exists, and inserting the line again would duplicate it", len(got))
		}
	})
}

// TestLanguageLine_OneActionWritesEveryMissingLine: in a repository that has
// not migrated at all, the preferred action writes this domain's file and a
// second one writes every domain's, in one edit -- rather than one quick fix
// and one rebuild per domain.
func TestLanguageLine_OneActionWritesEveryMissingLine(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"gadgets/traits.memql":            probeTrait,
		"widgets/traits.memql":            probeTrait,
		"sprockets/traits.memql":          probeTrait,
		"sprockets/" + dslfs.ManifestFile: declaration(),
	})
	_, c := languageLineServer(t, root, true)
	uri := pathToURI(filepath.Join(root, "widgets", "traits.memql"))
	c.call(protocol.MethodTextDocumentDidOpen, didOpenParams(t, uri, probeTrait))
	diag, ok := withCode(c.diagnostics(uri), langparser.CodeLanguageLineMissing)
	if !ok {
		t.Fatal("no missing-line refusal on widgets/traits.memql")
	}

	actions, _ := c.call(protocol.MethodTextDocumentCodeAction, codeActionParams(t, uri, diag)).([]protocol.CodeAction)
	if len(actions) != 2 {
		t.Fatalf("actions = %s; want two", jsonOf(t, actions))
	}
	if actions[0].Title != "Create widgets/memql.toml" || actions[0].IsPreferred == nil || !*actions[0].IsPreferred {
		t.Errorf("first action = %q (preferred %v); want the preferred \"Create widgets/memql.toml\"", actions[0].Title, actions[0].IsPreferred)
	}
	all := actions[1]
	if all.Title != "Create memql.toml in all 2 domains without one" {
		t.Errorf("second action title = %q", all.Title)
	}
	if all.IsPreferred != nil {
		t.Errorf("the all-domains action is marked preferred; only one action may be, and it is this domain's")
	}
	var created []string
	for _, change := range all.Edit.DocumentChanges {
		if cf, ok := change.(protocol.CreateFile); ok {
			created = append(created, cf.URI)
		}
	}
	want := []string{
		pathToURI(filepath.Join(root, "gadgets", dslfs.ManifestFile)),
		pathToURI(filepath.Join(root, "widgets", dslfs.ManifestFile)),
	}
	if len(all.Edit.DocumentChanges) != 4 || strings.Join(created, " ") != strings.Join(want, " ") {
		t.Errorf("the all-domains edit creates %v in %d changes; want %v, each with its insert, in domain order",
			created, len(all.Edit.DocumentChanges), want)
	}

	// Once one of the two is on disk, one remains: no all-domains action.
	writeFiles(t, root, map[string]string{"gadgets/" + dslfs.ManifestFile: declaration()})
	actions, _ = c.call(protocol.MethodTextDocumentCodeAction, codeActionParams(t, uri, diag)).([]protocol.CodeAction)
	if len(actions) != 1 {
		t.Errorf("with one domain left, actions = %s; want the one", jsonOf(t, actions))
	}
}

// TestLanguageLine_TheNoticeIsWrittenFromItsCause pins every sentence the
// failure notification can say: the domains and the fix for refused lines,
// and where the list is for any other failure -- never the engine's report.
func TestLanguageLine_TheNoticeIsWrittenFromItsCause(t *testing.T) {
	refused := func(root string, problems ...string) memql.WorkspaceLanguageLines {
		lines := memql.WorkspaceLanguageLines{Root: root}
		for i := 0; i < len(problems); i += 2 {
			lines.Problems = append(lines.Problems, memql.LanguageLineProblem{Domain: problems[i], Code: problems[i+1]})
		}
		return lines
	}
	const (
		missing = langparser.CodeLanguageLineMissing
		newer   = langparser.CodeLanguageVersionNewer
		edition = langparser.CodeEditionUnknown
	)
	for _, tc := range []struct {
		name         string
		lines        memql.WorkspaceLanguageLines
		createsFiles bool
		want         string
	}{
		{"one domain without a line", refused("", "gadgets", missing), true,
			`MemQL completion and hover are off: domain "gadgets" has no memql.toml. Use the quick fix on any of its files, or add gadgets/memql.toml.`},
		{"one domain without a line, for a client with no quick fix", refused("dsl", "gadgets", missing), false,
			`MemQL completion and hover are off: domain "gadgets" has no memql.toml. Add dsl/gadgets/memql.toml; the error on any of its files says what it declares.`},
		{"two domains without a line", refused("", "gadgets", missing, "widgets", missing), true,
			`MemQL completion and hover are off: domains "gadgets" and "widgets" have no memql.toml. Use the quick fix on any of their files, or add a memql.toml to each.`},
		{"two domains without a line, for a client with no quick fix", refused("", "gadgets", missing, "widgets", missing), false,
			`MemQL completion and hover are off: domains "gadgets" and "widgets" have no memql.toml. Add a memql.toml to each; the error on any of their files says what it declares.`},
		{"six domains without a line", refused("", "a", missing, "b", missing, "c", missing, "d", missing, "e", missing, "f", missing), true,
			`MemQL completion and hover are off: domains "a", "b", "c" and 3 more have no memql.toml. Use the quick fix on any of their files, or add a memql.toml to each.`},
		{"a line this engine cannot use", refused("", "gadgets", newer), true,
			`MemQL completion and hover are off: this version of MemQL cannot use the memql.toml of domain "gadgets". Open any of its files for the reason and the fix.`},
		{"one domain wrong in two ways", refused("", "gadgets", newer, "gadgets", edition), true,
			`MemQL completion and hover are off: this version of MemQL cannot use the memql.toml of domain "gadgets". Open any of its files for the reason and the fix.`},
		{"lines missing and lines unusable", refused("", "gadgets", missing, "sprockets", newer, "widgets", missing), true,
			`MemQL completion and hover are off: domains "gadgets" and "widgets" have no memql.toml, and this version of MemQL cannot use the one in domain "sprockets". Open any of their files for the reason and the fix.`},
		{"a failure with no line refused", refused("dsl"), true,
			`MemQL completion and hover are off: this workspace would not boot. The Problems panel lists the errors the editor can see in open files; run "memqllint dsl" for the full report.`},
		{"a failure with no line refused, domains at the top", refused(""), true,
			`MemQL completion and hover are off: this workspace would not boot. The Problems panel lists the errors the editor can see in open files; run "memqllint ." for the full report.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildFailureNotice(tc.lines, tc.createsFiles); got != tc.want {
				t.Errorf("notice\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestLanguageLine_EachCauseIsAnnouncedOnce walks announceBuild through the
// builds an author causes, counting notifications: news is announced, the
// same failure is not, fewer refusals than were announced are not, and a
// build that succeeds makes the next failure news again.
func TestLanguageLine_EachCauseIsAnnouncedOnce(t *testing.T) {
	commonlog.Configure(-4, nil)
	s := newServer(t.TempDir(), commonlog.GetLogger(lsName))
	var shown []string
	notify := func(method string, params any) {
		if method == protocol.ServerWindowShowMessage {
			shown = append(shown, params.(protocol.ShowMessageParams).Message)
		}
	}
	failed := errors.New("strict DSL boot refused")
	lines := func(domains ...string) memql.WorkspaceLanguageLines {
		var l memql.WorkspaceLanguageLines
		for _, d := range domains {
			l.Problems = append(l.Problems, memql.LanguageLineProblem{Domain: d, Code: langparser.CodeLanguageLineMissing})
		}
		return l
	}

	for i, step := range []struct {
		what  string
		err   error
		lines memql.WorkspaceLanguageLines
		want  int
	}{
		{"the first failure", failed, lines("gadgets"), 1},
		{"the same failure", failed, lines("gadgets"), 1},
		{"a second domain refused", failed, lines("gadgets", "widgets"), 2},
		{"one of the two fixed", failed, lines("widgets"), 2},
		{"a build that succeeds", nil, lines(), 2},
		{"a failure after the success", failed, lines("widgets"), 3},
		{"lines fixed, but something else refuses the tree", failed, lines(), 4},
		{"that failure again", failed, lines(), 4},
		{"a refused line on top of it", failed, lines("gadgets"), 5},
	} {
		s.announceBuild(notify, step.err, step.lines)
		if len(shown) != step.want {
			t.Fatalf("step %d (%s): %d notifications; want %d: %q", i+1, step.what, len(shown), step.want, shown)
		}
	}
}

// TestServer_InitializeAdvertisesQuickFixes: capabilities are hand-built, so a
// registered handler with no advertised capability is never called
// (TestServer_InitializeAdvertisesDefinitionProvider says why). And the
// client's own capabilities decide whether the quick fix is offered at all.
func TestServer_InitializeAdvertisesQuickFixes(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ir := res.(protocol.InitializeResult)
	opts, ok := ir.Capabilities.CodeActionProvider.(protocol.CodeActionOptions)
	if !ok || len(opts.CodeActionKinds) != 1 || opts.CodeActionKinds[0] != protocol.CodeActionKindQuickFix {
		t.Errorf("CodeActionProvider = %#v; want CodeActionOptions advertising the quickfix kind", ir.Capabilities.CodeActionProvider)
	}
	if s.handler().TextDocumentCodeAction == nil {
		t.Error("TextDocumentCodeAction handler not registered")
	}

	for _, tc := range []struct {
		name, params string
		want         bool
	}{
		{"VS Code", vscodeInitialize, true},
		{"a client that says nothing", `{"processId":null,"rootUri":null,"capabilities":{}}`, false},
		{"a client without the create operation", `{"processId":null,"rootUri":null,"capabilities":{"workspace":{"workspaceEdit":{"documentChanges":true,"resourceOperations":["rename"]}}}}`, false},
		{"a client without document changes", `{"processId":null,"rootUri":null,"capabilities":{"workspace":{"workspaceEdit":{"resourceOperations":["create"]}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var params protocol.InitializeParams
			if err := json.Unmarshal([]byte(tc.params), &params); err != nil {
				t.Fatal(err)
			}
			if got := clientCreatesFiles(params.Capabilities); got != tc.want {
				t.Errorf("clientCreatesFiles = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestRebuildDebouncer coalesces two saves into one rebuild, on the fake clock
// the diagnostics debouncer's test uses.
func TestRebuildDebouncer(t *testing.T) {
	clock := &fakeClock{}
	r := newRebuildDebouncerWithTimers(rebuildDebounce, clock.afterFunc)
	rebuilds := 0
	r.schedule(func() { rebuilds++ })
	r.schedule(func() { rebuilds++ })
	if n := clock.fire(); n != 1 || rebuilds != 1 {
		t.Fatalf("live timers = %d, rebuilds = %d; want 1 and 1 -- the second schedule supersedes the first", n, rebuilds)
	}
	r.schedule(func() { rebuilds++ })
	r.stop()
	if n := clock.fire(); n != 0 || rebuilds != 1 {
		t.Fatalf("after stop: live timers = %d, rebuilds = %d; want 0 and still 1", n, rebuilds)
	}
}
