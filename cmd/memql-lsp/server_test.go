package main

import (
	"testing"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func newTestServer() *server {
	commonlog.Configure(-4, nil) // verbosity None: keep test output quiet
	return newServer("/tmp/workspace", commonlog.GetLogger(lsName))
}

// noopCtx is a glsp.Context whose Notify discards -- the sync handlers call
// ctx.Notify to push diagnostics, so tests must pass a non-nil context.
func noopCtx() *glsp.Context {
	return &glsp.Context{Notify: func(string, any) {}}
}

func TestServer_InitializeAdvertisesIncrementalSync(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ir, ok := res.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize returned %T; want protocol.InitializeResult", res)
	}
	if ir.ServerInfo == nil || ir.ServerInfo.Name != lsName {
		t.Errorf("server info = %+v; want name %q", ir.ServerInfo, lsName)
	}
	sync, ok := ir.Capabilities.TextDocumentSync.(protocol.TextDocumentSyncOptions)
	if !ok {
		t.Fatalf("TextDocumentSync = %T; want TextDocumentSyncOptions", ir.Capabilities.TextDocumentSync)
	}
	if sync.Change == nil || *sync.Change != protocol.TextDocumentSyncKindIncremental {
		t.Errorf("sync change kind = %v; want Incremental", sync.Change)
	}
	if sync.OpenClose == nil || !*sync.OpenClose {
		t.Error("expected OpenClose sync to be advertised")
	}
}

func TestServer_DocumentLifecycle(t *testing.T) {
	s := newTestServer()
	const uri = "file:///x.memql"

	if err := s.didOpen(noopCtx(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, Text: "hello world"},
	}); err != nil {
		t.Fatalf("didOpen: %v", err)
	}

	if err := s.didChange(noopCtx(), &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri}},
		ContentChanges: []any{rangeChange(0, 6, 0, 11, "there")},
	}); err != nil {
		t.Fatalf("didChange: %v", err)
	}
	if got, _ := s.docs.get(uri); got != "hello there" {
		t.Errorf("buffer after didChange = %q; want %q", got, "hello there")
	}

	if err := s.didClose(noopCtx(), &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	}); err != nil {
		t.Fatalf("didClose: %v", err)
	}
	if _, ok := s.docs.get(uri); ok {
		t.Error("expected document removed after didClose")
	}
}
