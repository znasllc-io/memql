// Package dsl is the unified embed surface for the new domain-first
// .memql tree. It lives alongside the legacy per-kind embed packages
// (dsl/v1/concepts/, dsl/v1/queries/, etc.) during the transitional
// state of the file-restructure migration (see
// docs/dsl-import-model-refactor.md).
//
// The Tree fs.FS exposed here is what the new unified loader
// (component/memql/unified_loader.go) walks. Each per-kind legacy
// embed package continues to expose its own fs.FS for the legacy
// loaders until Pass 2 of the restructure migration retires them.
//
// memql#356: Tree() also overlays plugin-registered DSL subtrees
// (see RegisterTree below) so external Go modules can contribute
// additional domains without modifying this package's `go:embed`
// list. The CoPresent BFF
// (github.com/visionarys-io/memql-bff-copresent) consumes this
// extension point to mount its `copresent/` domain at boot.
package dsl

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"
)

// embedFS holds every .memql + .tmpl under the domain-first tree.
// The all: prefix ensures _-prefixed files are included so the
// walker can apply the soft-disable rule consistently (whereas the
// default behavior would skip them at the embed step entirely).
//
//go:embed all:agents all:calendar all:cluster all:cognition all:common all:curriculum all:data all:guide all:harness all:identity all:knowledge all:memql all:observability all:planner all:platform all:policies all:providers all:router all:safety all:workbench all:worker
var embedFS embed.FS

// pluginTrees holds the additional DSL subtrees registered by external
// Go modules via RegisterTree. Each entry maps a domain name (the
// top-level directory inside the merged tree -- e.g. "copresent") to
// the FS rooted at the domain's contents (the consumer's embed.FS
// containing `concepts.memql, queries.memql, prompts/`, etc).
//
// Indexed by domain name so RegisterTree is idempotent on the same
// domain (re-registering replaces; this keeps tests + duplicate-init
// scenarios well-defined).
var (
	pluginTreesMu sync.RWMutex
	pluginTrees   = map[string]fs.FS{}
)

// RegisterTree mounts an additional DSL subtree under `domain/` in
// the unified tree returned by Tree. Intended for use from init()
// hooks in product-specific Go packages that the bff binary blank-
// imports.
//
// Example, in memql-bff-copresent's integrations/copresent package:
//
//	//go:embed all:*.memql all:prompts
//	var copresentFS embed.FS
//
//	func init() {
//	    memqldsl.RegisterTree("copresent", copresentFS)
//	}
//
// After registration, every DSL loader that walks dsl.Tree() sees
// the copresent files under `copresent/` alongside the embedded
// domains. The CoPresent SDK's generated queries / mutations resolve
// against the live engine the same way agents/, cluster/, cognition/
// do today.
//
// Panics on empty domain or nil tree -- both are programming errors
// in the init() path.
func RegisterTree(domain string, tree fs.FS) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		panic("dsl.RegisterTree: domain must be non-empty")
	}
	if strings.Contains(domain, "/") {
		panic("dsl.RegisterTree: domain must not contain '/' (got: " + domain + ")")
	}
	if tree == nil {
		panic("dsl.RegisterTree: tree must not be nil")
	}
	pluginTreesMu.Lock()
	defer pluginTreesMu.Unlock()
	pluginTrees[domain] = tree
}

// Tree returns an fs.FS rooted at the unified DSL tree. Each top-
// level entry under the FS is a domain folder (cognition/,
// common/, providers/, policies/, ...) plus any domains contributed
// at init() time via RegisterTree.
//
// The returned FS is read-only and safe for concurrent use; the
// underlying layered view is rebuilt on every call to pick up any
// init()-time registrations that happened after the previous call
// (the registry is a global protected by a sync.RWMutex; this is
// fine -- init() registrations land before the first Tree() call
// in normal binary startup, but we don't lock in that ordering).
func Tree() fs.FS {
	pluginTreesMu.RLock()
	overlays := make(map[string]fs.FS, len(pluginTrees))
	for k, v := range pluginTrees {
		overlays[k] = v
	}
	pluginTreesMu.RUnlock()
	if len(overlays) == 0 {
		// Fast path: no plugins registered. Skip the layered wrapper
		// so behaviour is bit-exact identical to the pre-#356 shape.
		return embedFS
	}
	return &layeredFS{base: embedFS, overlays: overlays}
}

// layeredFS overlays a set of named plugin trees onto a base FS.
// Plugin trees are mounted under `<domain>/`; lookups under that
// prefix delegate to the registered FS. Lookups elsewhere delegate
// to the base FS. Root-level directory listings merge base + every
// non-conflicting overlay's domain key.
//
// Conflicts (a plugin domain that collides with an embedded domain
// name) are fully shadowed: the embedded tree owns the domain
// outright, and the overlay's contents are unreachable. This keeps
// memql's first-party domains canonical and avoids a partial-merge
// shape where embedded files appear alongside plugin files under
// the same domain.
type layeredFS struct {
	base     fs.FS
	overlays map[string]fs.FS
	// embeddedDomains caches the set of top-level directory names
	// in the base FS so the per-call shadowing check is O(1). Built
	// lazily on first use; immutable for the lifetime of the
	// layeredFS instance (Tree() builds a fresh one per call).
	embeddedDomainsOnce sync.Once
	embeddedDomains     map[string]struct{}
}

// embeddedHas reports whether the base FS has a top-level entry
// named domain (a "domain" in the loader's sense -- the first path
// segment under root). Used to decide whether an overlay's
// registration is shadowed.
func (l *layeredFS) embeddedHas(domain string) bool {
	l.embeddedDomainsOnce.Do(func() {
		entries, err := fs.ReadDir(l.base, ".")
		if err != nil {
			l.embeddedDomains = map[string]struct{}{}
			return
		}
		l.embeddedDomains = make(map[string]struct{}, len(entries))
		for _, e := range entries {
			l.embeddedDomains[e.Name()] = struct{}{}
		}
	})
	_, ok := l.embeddedDomains[domain]
	return ok
}

// overlayFor returns the overlay registered for domain, if one
// exists AND the base FS does not already own that domain. The
// "fully shadowed on conflict" rule lives here -- callers that go
// through this helper get the right answer without re-checking.
func (l *layeredFS) overlayFor(domain string) (fs.FS, bool) {
	if l.embeddedHas(domain) {
		return nil, false
	}
	overlay, ok := l.overlays[domain]
	return overlay, ok
}

// Open opens the named file. Path semantics match standard fs.FS:
// rooted, slash-separated, no leading slash, "." for the root.
func (l *layeredFS) Open(name string) (fs.File, error) {
	if name == "." || name == "" {
		return l.base.Open(".")
	}
	// Determine the path's first segment. If that segment is an
	// embedded domain, the embedded tree owns it -- delegate
	// straight to base (which will succeed for hits and return
	// fs.ErrNotExist for misses, both correct).
	prefix, rest := splitFirst(name)
	if l.embeddedHas(prefix) {
		return l.base.Open(name)
	}
	// First segment isn't an embedded domain. Try a plugin overlay
	// registered under that name.
	if overlay, ok := l.overlayFor(prefix); ok {
		target := rest
		if target == "" {
			target = "."
		}
		return overlay.Open(target)
	}
	// No embedded domain, no overlay -- not found.
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir lists directory entries. At the root, the result merges
// embedded entries + a synthetic entry per registered non-shadowed
// plugin domain. Inside an overlay (or an embedded domain),
// delegates to the appropriate underlying FS.
func (l *layeredFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." || name == "" {
		base, err := fs.ReadDir(l.base, ".")
		if err != nil {
			return nil, err
		}
		seen := map[string]struct{}{}
		out := make([]fs.DirEntry, 0, len(base)+len(l.overlays))
		for _, e := range base {
			seen[e.Name()] = struct{}{}
			out = append(out, e)
		}
		// Sort overlay names so the merged listing is deterministic;
		// loaders don't rely on order today but tests are easier to
		// pin when the result is stable.
		names := make([]string, 0, len(l.overlays))
		for k := range l.overlays {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if _, conflict := seen[k]; conflict {
				// Embedded domain shadows the overlay -- skip the
				// synthetic entry so a single name doesn't appear
				// twice in the listing.
				continue
			}
			out = append(out, syntheticDirEntry{name: k})
		}
		return out, nil
	}
	prefix, rest := splitFirst(name)
	if l.embeddedHas(prefix) {
		return fs.ReadDir(l.base, name)
	}
	if overlay, ok := l.overlayFor(prefix); ok {
		target := rest
		if target == "" {
			target = "."
		}
		return fs.ReadDir(overlay, target)
	}
	return fs.ReadDir(l.base, name)
}

// splitFirst returns the first path segment + the rest. "copresent"
// -> ("copresent", ""). "copresent/queries.memql" -> ("copresent",
// "queries.memql"). "copresent/prompts/agentReply.tmpl" ->
// ("copresent", "prompts/agentReply.tmpl").
func splitFirst(name string) (head, tail string) {
	idx := strings.IndexByte(name, '/')
	if idx < 0 {
		return name, ""
	}
	return name[:idx], name[idx+1:]
}

// syntheticDirEntry stands in for the directory entry of a mounted
// overlay at the root of the layered FS. Loaders only read .Name()
// + .IsDir() at the root level, so the minimum surface is enough.
// Info() returns a synthetic FileInfo that reports the entry as a
// directory; loaders that walk deeper hit the overlay directly via
// Open / ReadDir.
type syntheticDirEntry struct {
	name string
}

func (s syntheticDirEntry) Name() string {
	return s.name
}

func (s syntheticDirEntry) IsDir() bool {
	return true
}

func (s syntheticDirEntry) Type() fs.FileMode {
	return fs.ModeDir
}

func (s syntheticDirEntry) Info() (fs.FileInfo, error) {
	return syntheticFileInfo{name: s.name}, nil
}

// syntheticFileInfo is the minimal companion to syntheticDirEntry.
// Loaders that call Info() on the root-level overlay entries only
// read IsDir + Name + Mode; everything else returns zero-value
// safe defaults.
type syntheticFileInfo struct {
	name string
}

func (s syntheticFileInfo) Name() string       { return s.name }
func (s syntheticFileInfo) Size() int64        { return 0 }
func (s syntheticFileInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (s syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (s syntheticFileInfo) IsDir() bool        { return true }
func (s syntheticFileInfo) Sys() any           { return nil }
