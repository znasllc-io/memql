package main

import (
	"os"
	"sync"
	"sync/atomic"

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
	root    string
	log     commonlog.Logger
	docs    *documentStore
	diag    *diagnosticsDebouncer
	rebuild *rebuildDebouncer

	// mu guards sense and lines, which one build produces together and
	// setBuild swaps together: lines is the workspace's language lines as that
	// build resolved them (languageline.go).
	mu    sync.RWMutex
	sense *sense.Service
	lines memql.WorkspaceLanguageLines

	// createsFiles records whether the client can apply a workspace edit that
	// creates a file, which the language-line quick fix is (languageline.go).
	// Set once, at initialize.
	createsFiles atomic.Bool

	// announceMu guards announced: the causes the last notification about a
	// failed build named, so the same failure is not announced twice
	// (announceBuild, languageline.go). Empty after a build that succeeded.
	announceMu sync.Mutex
	announced  map[string]bool

	// catalogMu guards catalog, the connected cluster's construct catalog as
	// last pushed by the client over `memql/clusterCatalog`. Its ZERO VALUE IS
	// DISCONNECTED, which is what makes "no client has pushed anything yet" and
	// "there is no cluster" the same fact -- see training.go, which owns
	// everything about this field except its declaration.
	//
	// A lock of its own rather than mu: mu is held across the offline Sense
	// rebuild, and a catalog push arriving mid-rebuild has no reason to wait for
	// a workspace compile it has nothing to do with.
	catalogMu sync.RWMutex
	catalog   clusterCatalog
}

func newServer(root string, log commonlog.Logger) *server {
	return &server{
		root:    root,
		log:     log,
		docs:    newDocumentStore(),
		diag:    newDiagnosticsDebouncer(diagnosticsDebounce),
		rebuild: newRebuildDebouncer(rebuildDebounce),
	}
}

// scheduleRebuild debounces an offline-Sense rebuild after a save / watched-file
// change so a concept or shape added in one file becomes visible to
// completion/hover in the others. After the swap it republishes diagnostics for
// every open document against the new registry.
func (s *server) scheduleRebuild(notify glsp.NotifyFunc) {
	s.rebuild.schedule(func() {
		s.buildSense(notify)
		for _, uri := range s.docs.uris() {
			s.publishDiagnostics(notify, uri)
		}
	})
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

// getBuild returns the current Sense service and the language lines resolved
// with it, from one build.
func (s *server) getBuild() (*sense.Service, memql.WorkspaceLanguageLines) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sense, s.lines
}

// setBuild swaps in one build's Sense service and language lines together, so
// no reader pairs one build's service with another's lines.
func (s *server) setBuild(svc *sense.Service, lines memql.WorkspaceLanguageLines) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sense = svc
	s.lines = lines
}

// buildSense builds the offline Sense service over the workspace root, and
// resolves the workspace's language lines beside it. On engine failure (a
// workspace construct trips the strict-boot gate) BuildOfflineSense still
// returns a service carrying the workspace symbol graph, so import and
// reference diagnostics keep working on exactly the broken workspace an author
// is iterating on -- only the registry-backed vocabulary (completion, hover) is
// absent until boot is clean.
//
// That absence is said, not just logged (languageline.go): a refused language
// line is published on its domain's files, and the failure is announced once
// through notify. A nil notify announces nothing.
func (s *server) buildSense(notify glsp.NotifyFunc) {
	root := os.DirFS(s.root)
	// Resolved BEFORE the build, for the one race that is common: a memql.toml
	// written while the build runs (by the quick fix). Resolved first, the
	// lines at worst still name a refusal the build no longer has, which the
	// rebuild that same write triggers clears. Resolved after, they could miss
	// the refusal the build failed on, and the notification would blame the
	// wrong thing.
	lines := memql.ResolveWorkspaceLanguageLines(root)
	svc, err := memql.BuildOfflineSense(root)
	if err != nil {
		s.log.Warningf("offline Sense engine build failed for %s (%s); serving workspace-graph reference analysis + syntax diagnostics without the full registry", s.root, err)
	}
	if svc == nil {
		// Defensive: BuildOfflineSense returns a non-nil service even on error.
		svc = sense.New(nil)
	}
	s.setBuild(svc, lines)
	s.announceBuild(notify, err, lines)
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
		TextDocumentCompletion:         s.completion,
		TextDocumentHover:              s.hover,
		TextDocumentDefinition:         s.definition,
		TextDocumentSignatureHelp:      s.signatureHelp,
		TextDocumentCodeAction:         s.codeAction,
		WorkspaceDidChangeWatchedFiles: s.didChangeWatchedFiles,
	}
}

func (s *server) initialize(ctx *glsp.Context, params *protocol.InitializeParams) (any, error) {
	if params != nil {
		s.createsFiles.Store(clientCreatesFiles(params.Capabilities))
	}
	// Build offline Sense from the workspace before serving any request. A
	// failed build is announced from here: window/showMessage is one of the
	// notifications LSP lets a server send while initialize is in flight.
	var notify glsp.NotifyFunc
	if ctx != nil {
		notify = ctx.Notify
	}
	s.buildSense(notify)

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
		CompletionProvider: &protocol.CompletionOptions{
			TriggerCharacters: completionTriggerChars,
		},
		HoverProvider: true,
		// glsp's auto-advertise path is not used here (capabilities are
		// hand-built), so without this the client never sends
		// textDocument/definition at all.
		DefinitionProvider: true,
		SignatureHelpProvider: &protocol.SignatureHelpOptions{
			TriggerCharacters: signatureHelpTriggerChars,
		},
		// The quick fix that writes a domain's missing memql.toml
		// (languageline.go). Advertised to every client; offered only to one
		// that can create a file through a workspace edit.
		CodeActionProvider: protocol.CodeActionOptions{
			CodeActionKinds: []protocol.CodeActionKind{protocol.CodeActionKindQuickFix},
		},
		// Custom (non-LSP) requests this server answers, advertised so a client
		// can feature-detect rather than call blind and handle MethodNotFound.
		// See runnable.go, imports.go and training.go.
		Experimental: map[string]any{
			capabilityRunnableConstructs: true,
			capabilityImports:            true,
			capabilityTrainingState:      true,
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
	s.rebuild.stop()
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
	// A save may have added/edited a concept or shape; rebuild the registry so
	// it becomes visible to completion/hover in other files.
	s.scheduleRebuild(ctx.Notify)
	return nil
}

func (s *server) didChangeWatchedFiles(ctx *glsp.Context, _ *protocol.DidChangeWatchedFilesParams) error {
	s.scheduleRebuild(ctx.Notify)
	return nil
}

func (s *server) didClose(ctx *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.diag.cancel(params.TextDocument.URI)
	// Before the buffer goes: its own diagnostics are republished from it.
	s.withdrawRefusal(ctx.Notify, params.TextDocument.URI)
	s.docs.closeDoc(params.TextDocument.URI)
	return nil
}
