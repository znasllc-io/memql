package main

import (
	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

const lsName = "memql-lsp"

// server holds the language server's state: the workspace root (the folder of
// .memql files later work packages build offline Sense from) and the store of
// open documents. This WP2 skeleton records the root and maintains buffers; it
// wires no language features yet.
type server struct {
	root string
	log  commonlog.Logger
	docs *documentStore
}

func newServer(root string, log commonlog.Logger) *server {
	return &server{root: root, log: log, docs: newDocumentStore()}
}

// handler wires the LSP methods this skeleton implements: lifecycle
// (initialize / initialized / shutdown / setTrace) and incremental text sync
// (didOpen / didChange / didClose).
func (s *server) handler() *protocol.Handler {
	return &protocol.Handler{
		Initialize:            s.initialize,
		Initialized:           s.initialized,
		Shutdown:              s.shutdown,
		SetTrace:              s.setTrace,
		TextDocumentDidOpen:   s.didOpen,
		TextDocumentDidChange: s.didChange,
		TextDocumentDidClose:  s.didClose,
	}
}

func (s *server) initialize(_ *glsp.Context, _ *protocol.InitializeParams) (any, error) {
	// Advertise open/close notifications and INCREMENTAL text sync. The document
	// store folds each didChange edit into the full buffer (Sense needs full
	// source). No feature providers (semantic tokens, diagnostics, completion,
	// hover, signature help) are advertised yet -- those are WP3/WP4.
	syncKind := protocol.TextDocumentSyncKindIncremental
	openClose := true
	capabilities := protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &syncKind,
		},
	}
	s.log.Infof("initialize: workspace root=%s", s.root)
	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo:   &protocol.InitializeResultServerInfo{Name: lsName},
	}, nil
}

func (s *server) initialized(_ *glsp.Context, _ *protocol.InitializedParams) error {
	return nil
}

func (s *server) shutdown(_ *glsp.Context) error {
	// The glsp handler flips its initialized flag before calling this; nothing
	// else to tear down for the offline server.
	return nil
}

func (s *server) setTrace(_ *glsp.Context, params *protocol.SetTraceParams) error {
	protocol.SetTraceValue(params.Value)
	return nil
}

func (s *server) didOpen(_ *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	s.docs.open(params.TextDocument.URI, params.TextDocument.Text)
	return nil
}

func (s *server) didChange(_ *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	if _, ok := s.docs.applyChanges(params.TextDocument.URI, params.ContentChanges); !ok {
		s.log.Warningf("didChange for a document that was never opened: %s", params.TextDocument.URI)
	}
	return nil
}

func (s *server) didClose(_ *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.docs.closeDoc(params.TextDocument.URI)
	return nil
}
