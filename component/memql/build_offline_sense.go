package memql

// build_offline_sense.go gives an offline tool (cmd/memql-lsp) a full-fidelity
// MemQL Sense service over a workspace of .memql files, with no database and no
// network. It is the delivery-mechanism seam for the VS Code language server:
// the LSP front end owns stdio + protocol, this owns turning a workspace root
// into a live *sense.Service.
//
// The construction is the DB-free tier LintUnifiedTree already proves out (see
// its header): mount the workspace's product domains as an overlay on the
// embedded core tree, load concepts into a registry, build an engine with a nil
// DB, and Init it -- "Init needs only a concept registry -- no database, no
// network -- so the whole boot-time validation tier runs offline." Where
// LintUnifiedTree collects the diagnostics Init would emit and then discards the
// engine, BuildOfflineSense keeps the engine alive behind the existing
// SenseAdapter and hands back a *sense.Service that projects the workspace's
// real vocabulary (concepts, functions, shapes, specs, tools, ...).
//
// Global state, and why the returned service survives its restoration:
// MountOverlayDomains registers into the process-global plugin tree and
// LoadUnifiedConcepts merges into the process-global concept registry. Both are
// restored to their embedded-only baseline before returning, so a later
// in-process build -- the LSP rebuilds on every .memql change -- never observes
// a previous workspace's overlay. The returned service is unaffected because
// Init binds the engine to an ISOLATED clone of the merged registry
// (CloneDefaultRegistry), and the per-kind function/shape/spec/tool/prompt/
// provider registries Init builds are independent structures parsed from the
// tree while the overlay was still mounted. Restoring the global registry
// therefore strips nothing out from under the live service.
//
// Concurrency: the whole build is serialized on a package mutex because it
// mutates and restores process-global DSL registration state. It must not run
// concurrently with another BuildOfflineSense call or an engine Init in the
// same process; the standalone LSP binary has neither, and serializes its
// debounced rebuilds through this lock.

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sync"

	"github.com/znasllc-io/memql/component"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/sense"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// offlineSenseBuildMu serializes BuildOfflineSense calls. The build mutates and
// restores the process-global concept registry + plugin tree, so two concurrent
// builds would race on that shared state.
var offlineSenseBuildMu sync.Mutex

// BuildOfflineSense builds a full-fidelity MemQL Sense service over the embedded
// core tree plus every product-domain directory found at the top level of root,
// with no database and no network. root is typically os.DirFS over a workspace
// whose top-level entries are product namespace directories; a nil root (or one
// with no product domains) yields a service over the embedded core tree alone.
// Domains that collide with a core embedded domain are skipped -- the embedded
// tree owns them -- so pointing root at the engine's own dsl/ tree builds over
// the embedded tree as-is.
//
// Repeated calls are safe: the build is serialized and the process-global
// registration state is restored to its embedded-only baseline before
// returning, while the returned service retains this workspace's overlay
// vocabulary (see this file's header).
//
// An error from the engine's own init-time validation (a malformed construct in
// the workspace tripping the strict-boot gate) is returned as-is, so the LSP can
// surface it; the embedded core tree loads clean.
func BuildOfflineSense(root fs.FS) (*sense.Service, error) {
	// The workspace graph is built from the file tree (dslimports.Load returns a
	// partial tree even on error), independent of the engine's strict-boot
	// validation. Build it first so a service backed by it survives an Init
	// failure below -- reference diagnostics must keep working on exactly the
	// broken-reference workspace that trips boot.
	graph := buildWorkspaceGraph(root)

	adapter, err := buildOfflineSenseAdapter(nil, root)
	if err != nil {
		// A workspace construct tripped the strict-boot gate. Return a service
		// that still carries the workspace graph (registry-less) and surface the
		// error for logging, rather than dropping to a graph-less fallback.
		return sense.NewWithWorkspace(nil, graph), err
	}
	return sense.NewWithWorkspace(adapter, graph), nil
}

// buildOfflineSenseAdapter performs the DB-free construction and returns the
// SenseAdapter that BuildOfflineSense wraps. It is the internal seam tests use
// to assert directly on the built registries (the *sense.Service does not
// expose them). logger nil discards all build/init logging.
func buildOfflineSenseAdapter(logger *slog.Logger, root fs.FS) (*SenseAdapter, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	offlineSenseBuildMu.Lock()
	defer offlineSenseBuildMu.Unlock()

	// Mount overlays from the resolved DSL root, not the raw workspace root
	// (memql#2770). MountOverlayDomains treats any immediate subdirectory
	// holding a .memql file as a product domain -- and from a repository root
	// a single scratch file in dsl/ makes `dsl` itself qualify, so it mounts
	// as a domain and every concept beneath it fails namespace validation,
	// taking the whole registry down. Resolving first mounts from where the
	// real domains live: in the engine repo they all collide with core
	// embedded domains and are correctly skipped, and a product bundle whose
	// domains sit at the top level resolves to the root unchanged.
	overlayRoot, _ := resolveDSLRoot(root)
	_, _, unmount := memqldsl.MountOverlayDomains(logger, overlayRoot)
	defer func() {
		unmount()
		// LoadUnifiedConcepts is additive (MergeAll). Restore a clean
		// embedded-only global registry so the next in-process build does not
		// observe this workspace's overlay concepts: clear, then reload the
		// now-overlay-free tree. The returned adapter is unaffected -- Init
		// bound the engine to the isolated clone taken below.
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(logger)
	}()

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		return nil, fmt.Errorf("loading concepts from merged tree: %w", err)
	}

	// Snapshot the merged (embedded core + workspace overlay) concept registry
	// into an isolated clone. Init binds e.concepts to this clone, so restoring
	// the global registry in the deferred cleanup never strips the overlay
	// concepts out from under the returned service.
	registry := concept.CloneDefaultRegistry()

	// Nil DB: the whole boot-time validation + vocabulary load runs offline.
	// Route the constructor's own log line to io.Discard; Init-time logs go
	// through eng.Logger, set below.
	eng, err := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		return nil, fmt.Errorf("constructing offline sense engine: %w", err)
	}
	eng.Logger = logger

	if err := eng.Init(registry); err != nil {
		return nil, fmt.Errorf("initializing offline sense engine: %w", err)
	}

	return NewSenseAdapter(eng), nil
}
