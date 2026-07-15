package dsl

// lint_mount.go is the fs.FS-sourced sibling of MountRuntimeDomainsFromEnv
// (runtime_mount.go). Where the runtime mount registers a product's domains
// from a MEMQL_DSL_PATH directory at boot, this registers them from an
// arbitrary fs.FS, so an offline tool (memqllint) can reproduce the boot-time
// merged tree -- embedded core + product overlay -- and run the engine's full
// init-time DSL validation over a linted pack exactly as a node would at boot.

import (
	"io/fs"
	"log/slog"
	"strings"
)

// MountOverlayDomains registers every product-domain directory found at the
// top level of root as a plugin overlay in the unified Tree(), and returns the
// mounted domain names plus an unmount func that removes exactly those
// registrations. Callers MUST defer the unmount so the global registration
// state is restored for the next caller (this matters for in-process test
// runs; a one-shot CLI could skip it).
//
// A top-level entry is mounted only when it is a directory that directly
// contains at least one .memql file -- the shape of a product namespace
// directory (concepts.memql, queries.memql, ...). Entries that are files, that
// begin with "_" or "." (soft-disabled / hidden, matching the walker
// convention), that carry no .memql file (e.g. a bare prompts/ sidecar dir),
// or whose name collides with a core embedded domain are skipped. The core
// collision skip mirrors MountRuntimeDomainsFromEnv: the embedded tree owns
// that namespace and RegisterTree would panic on the collision.
func MountOverlayDomains(logger *slog.Logger, root fs.FS) (mounted []string, unmount func()) {
	noop := func() {}
	if root == nil {
		return nil, noop
	}
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		if logger != nil {
			logger.Warn("lint overlay mount: root unreadable; ignoring",
				"component", "dsl.lintMount", "err", err)
		}
		return nil, noop
	}

	core := make(map[string]struct{})
	for _, d := range coreDomains() {
		core[d] = struct{}{}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		domain := e.Name()
		if strings.HasPrefix(domain, "_") || strings.HasPrefix(domain, ".") {
			continue
		}
		if _, isCore := core[domain]; isCore {
			if logger != nil {
				logger.Warn("lint overlay mount: ignoring domain that collides with a core embedded domain",
					"component", "dsl.lintMount", "domain", domain)
			}
			continue
		}
		if !directoryHasMemqlFile(root, domain) {
			continue
		}
		sub, subErr := fs.Sub(root, domain)
		if subErr != nil {
			if logger != nil {
				logger.Warn("lint overlay mount: sub-FS failed; skipping domain",
					"component", "dsl.lintMount", "domain", domain, "err", subErr)
			}
			continue
		}
		RegisterTree(domain, sub)
		mounted = append(mounted, domain)
	}

	unmount = func() {
		for _, d := range mounted {
			UnregisterTree(d)
		}
	}
	return mounted, unmount
}

// directoryHasMemqlFile reports whether dir (a top-level entry of root)
// directly contains at least one .memql file -- the marker that distinguishes
// a product namespace directory from an incidental sidecar directory.
func directoryHasMemqlFile(root fs.FS, dir string) bool {
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".memql") {
			return true
		}
	}
	return false
}
