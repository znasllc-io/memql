package main

import (
	"os"
	"sync"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

const lsName = "memql-lsp"

// server holds the language server's state: the workspace root, the store of
// open documents, and the MemQL Sense service (built offline from the workspace
// at initialize, rebuilt as the workspace changes -- WP4). It serves push
// diagnostics and semantic tokens from Sense.
type server struct {
	root string
	log  commonlog.Logger
	docs *documentStore
	diag *diagnosticsDebouncer

	mu    sync.RWMutex
	sense *sense.Service
}

func newServer(root string, log commonlog.Logger) *server {
	return &server{
		root: root,
		log:  log,
		docs: newDocumentStore(),
		diag: newDiagnosticsDebouncer(diagnosticsDebounce),
	}
}

// getSense returns the current Sense service (nil before initialize).
func (s *server) getSense() *sense.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sense
}

// setSense atomically swaps the Sense service.
func (s *server) setSense(svc *sense.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sense = svc
}

// buildSense builds the offline Sense service over the workspace root. On
// failure (a workspace construct trips the strict-boot gate) it falls back to a
// registry-less service so syntax diagnostics still work -- an author iterating
// on broken DSL must still see their errors.
func (s *server) buildSense() {
	svc, err := memql.BuildOfflineSense(os.DirFS(s.root))
	if err != nil {
		s.log.Warningf("offline Sense build failed for %s (%s); falling back to syntax-only analysis", s.root, err)
		svc = sense.New(nil)
	}
	s.setSense(svc)
}

// handler wires the LSP methods: lifecycle, incremental text sync, push
// diagnostics (via the sync notifications), and semantic tokens.
func (s *server) handler() *protocol.Handler {
	return &protocol.Handler{
		Initialize:                     s.initialize,
		Initialized:                    s.initialized,
		Shutdown:                       s.shutdown,
		SetTrace:                       s.setTrace,
		TextDocumentDidOpen:            s.didOpen,
		TextDocumentDidChange:          s.didChange,
		TextDocumentDidSave:            s.didSave,
		TextDocumentDidClose:           s.didClose,
		TextDocumentSemanticTokensFull: s.semanticTokensFull,
	}
}

func (s *server) initialize(_ *glsp.Context, _ *protocol.InitializeParams) (any, error) {
	// Build offline Sense from the workspace before serving any request.
	s.buildSense()

	syncKind := protocol.TextDocumentSyncKindIncremental
	openClose := true
	fullTokens := true
	capabilities := protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &syncKind,
		},
		// Diagnostics are pushed via textDocument/publishDiagnostics (no
		// capability field needed). Semantic tokens are advertised with the
		// legend the encoder maps Sense token types onto.
		SemanticTokensProvider: protocol.SemanticTokensOptions{
			Legend: protocol.SemanticTokensLegend{
				TokenTypes:     semanticTokenTypes,
				TokenModifiers: []string{},
			},
			Full: &fullTokens,
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
	s.diag.stopAll()
	return nil
}

func (s *server) setTrace(_ *glsp.Context, params *protocol.SetTraceParams) error {
	protocol.SetTraceValue(params.Value)
	return nil
}

func (s *server) didOpen(ctx *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	s.docs.open(params.TextDocument.URI, params.TextDocument.Text)
	// Publish immediately on open so a freshly-opened file lights up at once.
	s.publishDiagnostics(ctx.Notify, params.TextDocument.URI)
	return nil
}

func (s *server) didChange(ctx *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	if _, ok := s.docs.applyChanges(params.TextDocument.URI, params.ContentChanges); !ok {
		s.log.Warningf("didChange for a document that was never opened: %s", params.TextDocument.URI)
		return nil
	}
	// Debounce diagnostics on the hot edit path.
	uri := params.TextDocument.URI
	notify := ctx.Notify
	s.diag.schedule(uri, func() { s.publishDiagnostics(notify, uri) })
	return nil
}

func (s *server) didSave(ctx *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
	s.publishDiagnostics(ctx.Notify, params.TextDocument.URI)
	return nil
}

func (s *server) didClose(_ *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.diag.cancel(params.TextDocument.URI)
	s.docs.closeDoc(params.TextDocument.URI)
	return nil
}
