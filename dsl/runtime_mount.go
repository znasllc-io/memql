package dsl

// runtime_mount.go delivers product DSL to a product-agnostic engine node at
// boot. It is the runtime counterpart to compile-time RegisterTree: where a
// carrier binary mounts its product domains via an init()/embed.FS at compile
// time, a plain engine binary mounts them from disk at boot — so the SAME
// engine image runs any product's DSL with zero compiled-in product code.
//
// The disk directory is populated by a bundle image's init-container copying
// the product's .memql tree into a shared volume the node reads at
// MEMQL_DSL_PATH. This is the mechanism validated by spike #2473; see
// docs/internal/design/platform-consolidation.md.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/core/dslfs"
)

// MountRuntimeDomainsFromEnv registers every product-domain directory found
// under MEMQL_DSL_PATH into the unified Tree() via os.DirFS + RegisterTree —
// the same overlay a carrier uses at compile time, sourced from disk. It MUST
// run before the first Tree() walk (i.e. before the concept/function/shape/…
// loaders in app bootstrap). No-op when MEMQL_DSL_PATH is unset.
//
// Expected layout mirrors the embedded tree: MEMQL_DSL_PATH/<domain>/
// <construct>s.memql (+ prompts/*.tmpl), one directory per product namespace.
// Directory names beginning with "_" or "." are skipped (soft-disabled /
// hidden, matching the walker convention). A directory whose name collides
// with a core embedded domain is skipped with a warning — the embedded tree
// owns that namespace and RegisterTree would reject the collision.
//
// Returns the domain names mounted (for structured logging + tests).
func MountRuntimeDomainsFromEnv(logger *slog.Logger) []string {
	root := dslfs.PathFromEnv()
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if logger != nil {
			logger.Warn("MEMQL_DSL_PATH runtime mount: root unreadable; ignoring",
				"component", "dsl.runtimeMount", "root", root, "err", err)
		}
		return nil
	}

	core := make(map[string]struct{})
	for _, d := range coreDomains() {
		core[d] = struct{}{}
	}

	var mounted []string
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
				logger.Warn("MEMQL_DSL_PATH: ignoring runtime domain that collides with a core embedded domain",
					"component", "dsl.runtimeMount", "domain", domain)
			}
			continue
		}
		RegisterTree(domain, os.DirFS(filepath.Join(root, domain)))
		mounted = append(mounted, domain)
		if logger != nil {
			logger.Info("MEMQL_DSL_PATH: mounted runtime product DSL domain",
				"component", "dsl.runtimeMount",
				"domain", domain,
				"path", filepath.Join(root, domain))
		}
	}
	if logger != nil {
		logger.Info("MEMQL_DSL_PATH: runtime product DSL mount complete",
			"component", "dsl.runtimeMount", "root", root, "domainsMounted", len(mounted))
	}
	return mounted
}
