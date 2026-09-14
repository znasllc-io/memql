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

	// publishMu serializes publishing a document's diagnostics with closing it,
	// so a publish computed before a close can never land after the close's
	// clear (didClose states the rule).
	publishMu sync.Mutex

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

	// rewrite is the workspace state behind the "Rewrite to edition 2026"
	// code actions -- see codeaction.go, which owns it.
	rewrite rewriteState
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

// buildSense builds the offline Sense service over the workspace root, with the
// language lines the build's Init resolved. On engine failure (a workspace
// construct trips the strict-boot gate) the build still returns a service
// carrying the workspace symbol graph, so import and reference diagnostics
// keep working on exactly the broken workspace an author is iterating on --
// only hover and the registry-backed vocabulary in completion are absent until
// boot is clean.
//
// That absence is said, not just logged (languageline.go): a refused language
// line is published on its domain's files, and the failure is announced once
// through notify. A nil notify announces nothing.
func (s *server) buildSense(notify glsp.NotifyFunc) {
	// The lines are the build's own, as its Init resolved them, so they and
	// the build's error are one answer even when a memql.toml changes while
	// the build runs.
	svc, lines, err := memql.BuildOfflineSenseWithLanguageLines(os.DirFS(s.root))
	if err != nil {
		s.log.Warningf("offline Sense engine build failed for %s (%s); serving workspace-graph reference analysis + syntax diagnostics without the full registry", s.root, err)
	}
	if svc == nil {
		// Defensive: BuildOfflineSense returns a non-nil service even on error.
		svc = sense.New(nil)
	}
	s.setBuild(svc, lines)
	// The predicate sources the rewrite code actions resolve against change
	// with the workspace, on the same schedule.
	s.rewrite.load(s.root)
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
		// Code actions: the quick fix that writes a domain's missing
		// memql.toml (languageline.go), offered only to a client that can
		// create a file through a workspace edit; and "Rewrite to edition
		// 2026", a quickfix on a retired form plus source.fixAll.memql for
		// the whole file (codeaction.go).
		CodeActionProvider: protocol.CodeActionOptions{
			CodeActionKinds: codeActionKinds,
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

// didClose forgets a document and clears its diagnostics.
//
// THE RULE: a closed document carries no diagnostics from this server. The
// server analyzes open buffers only -- a rebuild republishes open documents
// and nothing else -- so anything left on a closed file would stop being kept
// true. A language-line refusal is exactly what goes stale that way: its fix
// is a memql.toml written beside the file, after which a refusal left behind
// would still say the file is missing. The client clears nothing on close for
// pushed diagnostics, so the server publishes the empty set itself.
//
// Under publishMu, and after the buffer is dropped, so the clear is the last
// word: a publish that already read the buffer has finished before it, and one
// that has not finds no buffer and publishes nothing.
func (s *server) didClose(ctx *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.diag.cancel(uri)
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.docs.closeDoc(uri)
	s.rewrite.forget(uri)
	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{},
	})
	return nil
}
