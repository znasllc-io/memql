// Package workbench implements the agent-node-side handler for the
// workbenchHost tool. It manages per-Plan isolated working
// directories on the local filesystem and dispatches the six action
// verbs (exec / fs_read / fs_write / fs_list / fs_stat / http_fetch)
// against them.
//
// MVP scope: workspaces live on the agent node's local disk. Concept
// row persistence (v1:workbench:workspace) is defined in the DSL but
// not yet written from Go -- the future distributed version (lifting
// this code into a dedicated workbench node) will wire that. The
// release-on-plan-terminal automation lives in the DSL too and is a
// no-op until the persistence layer lands.
package workbench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// rootEnvVar names the env var that overrides the default workspace
// root directory. Useful for dev (point at a project-local path),
// Docker volumes (point at a mounted /var/lib/...), and tests.
const rootEnvVar = "MEMQL_WORKBENCH_ROOT"

// defaultRoot is the production default. Cloud Run instance-local;
// not durable across revision deploys for MVP. The distributed
// version swaps this for a durable substrate (GCS-FUSE / Filestore).
const defaultRoot = "/var/lib/memql/workbenches"

// workspace is the in-memory handle for a single Plan's workspace.
type workspace struct {
	planId     string
	rootPath   string
	createdAt  time.Time
	lastUsedAt time.Time
	mu         sync.Mutex // serializes per-workspace state changes
}

// Manager owns the workspace lifecycle: lazy provisioning on first
// access, lookup by planId, future teardown hooks.
type Manager struct {
	root  string
	mu    sync.Mutex
	cache map[string]*workspace
	clock func() time.Time
}

// NewManager constructs a Manager with the configured (or default)
// root directory. Does not create the root -- the first
// provisionForPlan call lazily mkdir's it.
func NewManager() *Manager {
	root := strings.TrimSpace(os.Getenv(rootEnvVar))
	if root == "" {
		root = defaultRoot
	}
	return &Manager{
		root:  root,
		cache: make(map[string]*workspace),
		clock: time.Now,
	}
}

// Root returns the manager's configured root directory.
func (m *Manager) Root() string { return m.root }

// provisionForPlan returns the workspace for planId, creating its
// directory tree lazily on first request. Idempotent.
func (m *Manager) provisionForPlan(planId string) (*workspace, error) {
	if strings.TrimSpace(planId) == "" {
		return nil, errors.New("workbench: planId is required")
	}
	if !safePlanId(planId) {
		return nil, fmt.Errorf("workbench: planId %q has disallowed characters", planId)
	}
	m.mu.Lock()
	ws, ok := m.cache[planId]
	if !ok {
		ws = &workspace{
			planId:    planId,
			rootPath:  filepath.Join(m.root, planId),
			createdAt: m.clock(),
		}
		m.cache[planId] = ws
	}
	m.mu.Unlock()

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := os.MkdirAll(ws.rootPath, 0o755); err != nil {
		return nil, fmt.Errorf("workbench: mkdir workspace root: %w", err)
	}
	ws.lastUsedAt = m.clock()
	return ws, nil
}

// safePlanId guards against directory-traversal and shell-metachar
// injection via the planId path component. Plan ids in this codebase
// follow the partition:concept:hash convention -- alphanumerics +
// `:` + `-` only. We accept that plus `_` and `.` and reject the
// rest.
func safePlanId(planId string) bool {
	if planId == "" || planId == "." || planId == ".." {
		return false
	}
	for _, r := range planId {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == ':' || r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// safeJoin resolves a user-supplied path relative to the workspace
// root, rejecting anything that escapes the root (absolute paths,
// `..` traversal). Returns the absolute path inside the workspace.
func (w *workspace) safeJoin(relPath string) (string, error) {
	if relPath == "" {
		return w.rootPath, nil
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("workbench: absolute paths not permitted (got %q)", relPath)
	}
	cleaned := filepath.Clean(filepath.Join(w.rootPath, relPath))
	rootClean := filepath.Clean(w.rootPath)
	// Ensure cleaned is rootClean OR a descendant of rootClean.
	if cleaned != rootClean && !strings.HasPrefix(cleaned, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("workbench: path %q escapes workspace root", relPath)
	}
	return cleaned, nil
}
