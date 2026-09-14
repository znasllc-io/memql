package main

// languageline.go -- a refused language line, shown where the author is
// looking (epic memql#5356; the editor half of its final review, memql#5362).
//
// A domain that declares no language line -- every product repository before
// it migrates -- is refused by the engine's strict Init, and with it the
// offline build behind completion and hover: the service falls back to the
// workspace graph alone, for the whole workspace. The editor used to go dark
// with no visible reason; the one message saying why was a log line in the
// output channel. Three things here say it where the author is looking:
//
//   - every open file of a refused domain carries the refusal on its first
//     line, beside Sense's own diagnostics (languageLineDiagnostics);
//   - a missing line has a quick fix that writes the file (codeAction);
//   - a failed build says once, in a notification, that completion and hover
//     are off and why (announceBuild).
//
// The client watches memql.toml as well as .memql files
// (editors/vscode/src/extension.ts), so writing the file -- through the quick
// fix or by hand -- rebuilds, and all three clear with no reload. A rebuild
// republishes OPEN files only, so a file that closes has its refusal withdrawn
// then (withdrawRefusal): left behind, it would outlive the fix.
//
// WHICH DOMAINS. The lines come from memql.ResolveWorkspaceLanguageLines,
// which mounts exactly what BuildOfflineSense mounts and resolves them with
// the resolver Init runs. A file in no mounted domain -- a core domain, or a
// directory the build does not read -- carries nothing.
//
// WHICH DIAGNOSTIC IS OURS, in a code-action request, is decided by its source
// and message and never by its code. glsp v0.2.2 decodes IntegerOrString with
// a VALUE-receiver UnmarshalJSON, so the code a client echoes back in the
// request's context arrives as nil: a handler keyed on it never fires against
// a real client, while a test that builds the params in Go passes. The code
// that decides the action is the refusal the server itself resolved.

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
)

// bootCause is the one cause of a build that failed with no language line
// refused (buildFailureCauses).
const bootCause = "boot"

// languageLineDiagnostics is one Error per refusal of the line governing the
// document, as lines resolved it. None for a document in no mounted domain, or
// in one whose line is accepted.
func (s *server) languageLineDiagnostics(uri protocol.DocumentUri, text string, lines memql.WorkspaceLanguageLines) []protocol.Diagnostic {
	problems, ok := s.documentRefusal(uri, lines)
	if !ok {
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(problems))
	for _, p := range problems {
		out = append(out, languageLineDiagnostic(text, p))
	}
	return out
}

// languageLineDiagnostic is a refusal as the editor shows it: an Error over the
// document's first line, carrying the refusal's code and message. The first
// line because the refusal is about the whole file -- nothing in it is at
// fault -- and it is the line an author reads first.
func languageLineDiagnostic(text string, p memql.LanguageLineProblem) protocol.Diagnostic {
	severity := protocol.DiagnosticSeverityError
	return protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{},
			// A column past the line's end clamps to it.
			End: position.ToLSPPosition(text, 1, math.MaxInt32),
		},
		Severity: &severity,
		Code:     &protocol.IntegerOrString{Value: p.Code},
		Source:   ptr(lsName),
		Message:  p.Message,
	}
}

// withdrawRefusal takes a refused line's diagnostic off a document that is
// closing, republishing the rest of what the document carries from its
// buffer.
//
// A refusal is about the domain, and its fix happens elsewhere: a memql.toml
// written beside the file. A rebuild republishes open files only, so a refusal
// left on a closed file would still say "no memql.toml" after the file exists.
// The client clears nothing on close itself, for pushed diagnostics. The
// refusal stays in view where it can be kept true: on every open file of the
// domain, and in the notification. The document's own diagnostics are kept,
// as they always were.
func (s *server) withdrawRefusal(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	text, ok := s.docs.get(uri)
	if !ok {
		return
	}
	svc, lines := s.getBuild()
	if svc == nil {
		return
	}
	if _, refused := s.documentRefusal(uri, lines); !refused {
		return
	}
	notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: senseDiagnostics(svc, uri, text),
	})
}

// documentRefusal is the refusal of the line governing a document: every
// problem naming its domain. ok is false for a document that is not a file
// under the workspace root, is in no mounted domain, or is in a domain whose
// line is accepted.
func (s *server) documentRefusal(uri protocol.DocumentUri, lines memql.WorkspaceLanguageLines) ([]memql.LanguageLineProblem, bool) {
	rel, ok := s.workspacePath(uri)
	if !ok {
		return nil, false
	}
	// The lines were resolved from the tree the domains sit in, which is
	// lines.Root below the workspace root.
	if lines.Root != "" {
		if rel, ok = strings.CutPrefix(rel, lines.Root+"/"); !ok {
			return nil, false
		}
	}
	line, ok := lines.Lines.For(rel)
	if !ok || !line.Refused {
		return nil, false
	}
	var problems []memql.LanguageLineProblem
	for _, p := range lines.Problems {
		if p.Domain == line.Domain {
			problems = append(problems, p)
		}
	}
	return problems, true
}

// workspacePath is a document's path relative to the workspace root,
// slash-separated. ok is false for a document that is not a file, or is not
// under the root.
func (s *server) workspacePath(uri protocol.DocumentUri) (string, bool) {
	if u, err := url.Parse(uri); err != nil || u.Scheme != "file" {
		return "", false
	}
	rel, err := filepath.Rel(s.root, filepath.FromSlash(uriToPath(uri)))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// manifestPath is where a domain's language line lives, relative to the
// workspace root and slash-separated: the path the quick fix names and writes.
func manifestPath(root, domain string) string {
	return path.Join(root, domain, dslfs.ManifestFile)
}

// manifestOnDisk reports whether a domain's memql.toml exists now, whatever
// the last build saw.
func (s *server) manifestOnDisk(root, domain string) bool {
	_, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(manifestPath(root, domain))))
	return err == nil
}

// clientCreatesFiles reports whether a client can apply a workspace edit that
// creates a file: it takes versioned document changes, and the create resource
// operation. VS Code says both. A client that does not is offered no quick fix,
// rather than one that fails when chosen.
func clientCreatesFiles(caps protocol.ClientCapabilities) bool {
	if caps.Workspace == nil || caps.Workspace.WorkspaceEdit == nil {
		return false
	}
	edit := caps.Workspace.WorkspaceEdit
	if edit.DocumentChanges == nil || !*edit.DocumentChanges {
		return false
	}
	return slices.Contains(edit.ResourceOperations, protocol.ResourceOperationKindCreate)
}

// codeAction answers textDocument/codeAction with the quick fix for a missing
// language line: one preferred action writing the document's own domain's
// memql.toml and, when more than one mounted domain is missing its line -- a
// repository that has not migrated at all -- a second writing all of them.
//
// Both are offered only for the refusal's own diagnostic, only while the file
// is still absent on disk, and only to a client that can create a file. A
// refusal for any other reason gets no action: the fix for a newer line or an
// edition this engine does not read is a decision, not a file.
func (s *server) codeAction(_ *glsp.Context, params *protocol.CodeActionParams) (any, error) {
	if !s.createsFiles.Load() || !wantsQuickFix(params.Context.Only) {
		return nil, nil
	}
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	_, lines := s.getBuild()
	problems, ok := s.documentRefusal(params.TextDocument.URI, lines)
	if !ok {
		return nil, nil
	}
	i := slices.IndexFunc(problems, func(p memql.LanguageLineProblem) bool {
		return p.Code == langparser.CodeLanguageLineMissing
	})
	if i < 0 {
		return nil, nil
	}
	missing := problems[i]
	if !mentionsRefusal(params.Context.Diagnostics, missing) || s.manifestOnDisk(lines.Root, missing.Domain) {
		return nil, nil
	}

	// The diagnostic the actions resolve is rebuilt from the refusal rather
	// than echoed from the request, whose code glsp has dropped (see the file
	// comment): the client matches an action to the diagnostic it published.
	diagnostic := languageLineDiagnostic(text, missing)
	quickFix := protocol.CodeActionKindQuickFix
	preferred := true
	actions := []protocol.CodeAction{{
		Title:       "Create " + manifestPath(lines.Root, missing.Domain),
		Kind:        &quickFix,
		Diagnostics: []protocol.Diagnostic{diagnostic},
		IsPreferred: &preferred,
		Edit:        s.createManifests(lines.Root, []string{missing.Domain}),
	}}
	if all := s.missingManifests(lines); len(all) > 1 {
		actions = append(actions, protocol.CodeAction{
			Title:       fmt.Sprintf("Create %s in all %d domains without one", dslfs.ManifestFile, len(all)),
			Kind:        &quickFix,
			Diagnostics: []protocol.Diagnostic{diagnostic},
			Edit:        s.createManifests(lines.Root, all),
		})
	}
	return actions, nil
}

// wantsQuickFix reports whether a request's `only` filter admits a quickfix.
// Kinds are hierarchical and a filter admits its children, so no filter, the
// empty kind and "quickfix" itself all do.
func wantsQuickFix(only []protocol.CodeActionKind) bool {
	if len(only) == 0 {
		return true
	}
	return slices.ContainsFunc(only, func(k protocol.CodeActionKind) bool {
		return k == protocol.CodeActionKindEmpty || k == protocol.CodeActionKindQuickFix
	})
}

// mentionsRefusal reports whether a code-action request's context carries the
// diagnostic this server published for a refusal: its source and its message.
func mentionsRefusal(diagnostics []protocol.Diagnostic, p memql.LanguageLineProblem) bool {
	return slices.ContainsFunc(diagnostics, func(d protocol.Diagnostic) bool {
		return d.Source != nil && *d.Source == lsName && d.Message == p.Message
	})
}

// missingManifests is every mounted domain missing its line whose memql.toml
// is still absent on disk, in domain order.
func (s *server) missingManifests(lines memql.WorkspaceLanguageLines) []string {
	var domains []string
	for _, p := range lines.Problems {
		if p.Code == langparser.CodeLanguageLineMissing && !s.manifestOnDisk(lines.Root, p.Domain) {
			domains = append(domains, p.Domain)
		}
	}
	return domains
}

// createManifests is the workspace edit that writes each domain's memql.toml,
// declaring the line this engine reads: the file is created -- left alone if
// it appears first -- and the declaration inserted at its start.
func (s *server) createManifests(root string, domains []string) *protocol.WorkspaceEdit {
	declaration := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
	ignoreIfExists := true
	changes := make([]any, 0, 2*len(domains))
	for _, d := range domains {
		uri := pathToURI(filepath.Join(s.root, filepath.FromSlash(manifestPath(root, d))))
		changes = append(changes,
			protocol.CreateFile{
				Kind:    string(protocol.ResourceOperationKindCreate),
				URI:     uri,
				Options: &protocol.CreateFileOptions{IgnoreIfExists: &ignoreIfExists},
			},
			protocol.TextDocumentEdit{
				// No version: the document does not exist yet, so the content
				// on disk -- none -- is what the edit applies to.
				TextDocument: protocol.OptionalVersionedTextDocumentIdentifier{
					TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri},
				},
				Edits: []any{protocol.TextEdit{NewText: declaration}},
			},
		)
	}
	return &protocol.WorkspaceEdit{DocumentChanges: changes}
}

// announceBuild tells the author, once per cause, that completion and hover are
// off and why, as a Warning notification.
//
// A failed build is announced when it has a cause the last announcement did
// not name. The same failure again says nothing, and neither does the same
// failure with fewer refusals -- the author fixing domains one by one has
// already been told about the rest. A build that succeeds clears the record,
// so the next failure is news. buildSense keeps its log line, which carries
// the engine's full report.
func (s *server) announceBuild(notify glsp.NotifyFunc, err error, lines memql.WorkspaceLanguageLines) {
	if notify == nil {
		return
	}
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	if err == nil {
		s.announced = nil
		return
	}
	causes := buildFailureCauses(lines)
	news := false
	for c := range causes {
		if !s.announced[c] {
			news = true
			break
		}
	}
	s.announced = causes
	if !news {
		return
	}
	notify(protocol.ServerWindowShowMessage, protocol.ShowMessageParams{
		Type:    protocol.MessageTypeWarning,
		Message: buildFailureNotice(lines, s.createsFiles.Load()),
	})
}

// buildFailureCauses names a failed build's causes: one per refusal of a
// mounted domain's line, or bootCause alone when no line is refused and the
// build failed on something else.
func buildFailureCauses(lines memql.WorkspaceLanguageLines) map[string]bool {
	causes := map[string]bool{}
	for _, p := range lines.Problems {
		causes["line "+p.Domain+" "+p.Code] = true
	}
	if len(causes) == 0 {
		causes[bootCause] = true
	}
	return causes
}

// buildFailureNotice is the notification for a failed build, written from its
// cause. Refused language lines are named with their fix. Any other failure
// says the workspace would not boot and where its errors are listed, and does
// not paste the engine's multi-line report -- that is the log line's.
//
// createsFiles is whether the client can take the quick fix: the notice
// offers it only then.
func buildFailureNotice(lines memql.WorkspaceLanguageLines, createsFiles bool) string {
	const off = "MemQL completion and hover are off: "
	manifest := dslfs.ManifestFile

	var missing, unusable []string
	for _, p := range lines.Problems {
		switch {
		case p.Code == langparser.CodeLanguageLineMissing:
			missing = append(missing, p.Domain)
		case !slices.Contains(unusable, p.Domain):
			// A domain whose line is wrong in two ways is named once.
			unusable = append(unusable, p.Domain)
		}
	}

	switch {
	case len(missing) == 0 && len(unusable) == 0:
		lintRoot := lines.Root
		if lintRoot == "" {
			lintRoot = "."
		}
		return off + "this workspace would not boot. The Problems panel lists the errors the editor can see in open files; " +
			"run \"memqllint " + lintRoot + "\" for the full report."

	case len(unusable) == 0 && len(missing) == 1:
		d := missing[0]
		if createsFiles {
			return fmt.Sprintf("%sdomain %q has no %s. Use the quick fix on any of its files, or add %s.",
				off, d, manifest, manifestPath(lines.Root, d))
		}
		return fmt.Sprintf("%sdomain %q has no %s. Add %s; the error on any of its files says what it declares.",
			off, d, manifest, manifestPath(lines.Root, d))

	case len(unusable) == 0:
		if createsFiles {
			return fmt.Sprintf("%s%s have no %s. Use the quick fix on any of their files, or add a %s to each.",
				off, domainsPhrase(missing), manifest, manifest)
		}
		return fmt.Sprintf("%s%s have no %s. Add a %s to each; the error on any of their files says what it declares.",
			off, domainsPhrase(missing), manifest, manifest)

	case len(missing) == 0:
		return fmt.Sprintf("%sthis version of MemQL cannot use the %s of %s. Open any of %s files for the reason and the fix.",
			off, manifest, domainsPhrase(unusable), possessive(len(unusable)))

	default:
		has, ones := "has", "the one"
		if len(missing) > 1 {
			has = "have"
		}
		if len(unusable) > 1 {
			ones = "the ones"
		}
		return fmt.Sprintf("%s%s %s no %s, and this version of MemQL cannot use %s in %s. Open any of their files for the reason and the fix.",
			off, domainsPhrase(missing), has, manifest, ones, domainsPhrase(unusable))
	}
}

// domainsPhrase names domains the way a sentence does: domain "a"; domains
// "a" and "b"; domains "a", "b" and "c"; past four, the first three and how
// many more.
func domainsPhrase(domains []string) string {
	quoted := make([]string, len(domains))
	for i, d := range domains {
		quoted[i] = strconv.Quote(d)
	}
	switch n := len(quoted); {
	case n == 1:
		return "domain " + quoted[0]
	case n <= 4:
		return "domains " + strings.Join(quoted[:n-1], ", ") + " and " + quoted[n-1]
	default:
		return fmt.Sprintf("domains %s and %d more", strings.Join(quoted[:3], ", "), n-3)
	}
}

// possessive is "its" for one domain and "their" for several.
func possessive(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}
