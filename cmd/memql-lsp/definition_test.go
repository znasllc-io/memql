package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tliron/commonlog"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// TestServer_InitializeAdvertisesDefinitionProvider is the gate that makes F12
// reachable at all (memql#2754). Capabilities here are hand-built rather than
// derived from the handler map, so a registered handler with no advertised
// capability is silently dead -- the client never sends the request.
func TestServer_InitializeAdvertisesDefinitionProvider(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ir, ok := res.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize returned %T; want protocol.InitializeResult", res)
	}
	if ir.Capabilities.DefinitionProvider == nil {
		t.Fatal("DefinitionProvider not advertised; VS Code will never send textDocument/definition")
	}
	if enabled, isBool := ir.Capabilities.DefinitionProvider.(bool); !isBool || !enabled {
		t.Errorf("DefinitionProvider = %v, want true", ir.Capabilities.DefinitionProvider)
	}
	if s.handler().TextDocumentDefinition == nil {
		t.Error("TextDocumentDefinition handler not registered")
	}
}

// TestPathToURI_RoundTripsWithUriToPath: the definition handler converts a
// workspace-relative target into a URI, and the two helpers must agree or F12
// lands nowhere.
func TestPathToURI_RoundTripsWithUriToPath(t *testing.T) {
	cases := []string{
		"/home/user/repo/dsl/actions/concepts.memql",
		"/path with spaces/dsl/actions/concepts.memql",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if got := uriToPath(pathToURI(p)); got != p {
				t.Errorf("round trip = %q, want %q (uri was %q)", got, p, pathToURI(p))
			}
		})
	}
}

// TestDefinition_JumpsToDeclarationInAnotherFile drives the handler end to end
// over a real two-file workspace: the reference is ambient (no `use` import,
// authoring rule 25), so this is exactly the case F12 exists for.
func TestDefinition_JumpsToDeclarationInAnotherFile(t *testing.T) {
	root := t.TempDir()
	domain := filepath.Join(root, "actions")
	if err := os.MkdirAll(domain, 0o755); err != nil {
		t.Fatal(err)
	}
	concepts := "@version(\"1.0.0\")\n@namespace(\"actions\")\nconcept candidate {\n  id  string  @required\n}\n"
	shapes := "@row\nshape candidate candidateFull {\n  id\n}\n"
	if err := os.WriteFile(filepath.Join(domain, "concepts.memql"), []byte(concepts), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(domain, "shapes.memql"), []byte(shapes), 0o644); err != nil {
		t.Fatal(err)
	}

	commonlog.Configure(-4, nil)
	s := newServer(root, commonlog.GetLogger(lsName))
	s.buildSense()

	shapesURI := pathToURI(filepath.Join(domain, "shapes.memql"))
	s.docs.open(shapesURI, shapes)

	// Line 2 `shape candidate candidateFull {`; LSP positions are 0-based, so
	// line 1, character 8 sits inside `candidate`.
	res, err := s.definition(noopCtx(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: shapesURI},
			Position:     protocol.Position{Line: 1, Character: 8},
		},
	})
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	locations, ok := res.([]protocol.Location)
	if !ok || len(locations) != 1 {
		t.Fatalf("definition returned %#v; want exactly one Location", res)
	}
	wantURI := pathToURI(filepath.Join(domain, "concepts.memql"))
	if locations[0].URI != wantURI {
		t.Errorf("target URI = %q, want %q", locations[0].URI, wantURI)
	}
	// `concept candidate {` is source line 3 -> LSP line 2; the name starts at
	// column 9 -> character 8.
	if locations[0].Range.Start.Line != 2 || locations[0].Range.Start.Character != 8 {
		t.Errorf("target start = %+v, want line 2 character 8", locations[0].Range.Start)
	}
}
