// Package dslfs provides the runtime indirection between the embedded
// .memql tree (compiled into every node-type binary via //go:embed) and
// an optional on-disk override rooted at MEMQL_DSL_PATH.
//
// Every loader that walks a per-type embedded FS goes through Overlay()
// to pick its source. When MEMQL_DSL_PATH is unset, Overlay returns the
// embedded FS unchanged. When MEMQL_DSL_PATH points at a directory that
// contains a sub-directory matching the type name (e.g.
// $MEMQL_DSL_PATH/queries), the on-disk subtree replaces the embedded
// one for that type. Missing-from-disk types continue to read from the
// embedded copy, so partial overrides are supported.
//
// This is the Phase 0 hook for the policies + DSL consolidation
// initiative. The same mechanism services dev hacking, per-deploy
// patches, and policy-trace fixtures in tests.
package dslfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const envPathKey = "MEMQL_DSL_PATH"

// PathFromEnv returns the override root from MEMQL_DSL_PATH, or "" if
// the variable is unset or empty. Whitespace is trimmed.
func PathFromEnv() string {
	return strings.TrimSpace(os.Getenv(envPathKey))
}

// Overlay returns the fs.FS the named DSL type should read from.
// typeName matches the top-level directory name under MEMQL_DSL_PATH
// (one of: concepts, mutations, queries, specs, automations, prompts,
// providers, shapes, tools, policies).
//
// If MEMQL_DSL_PATH is set and "<path>/<typeName>" exists, the on-disk
// FS is returned. Otherwise the embedded FS is returned as-is.
func Overlay(typeName string, embedded fs.FS) fs.FS {
	base := PathFromEnv()
	if base == "" {
		return embedded
	}
	root := filepath.Join(base, typeName)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return embedded
	}
	return os.DirFS(root)
}

// resolvedOverride records what the engine actually picked per type.
// Loaders that want to log "loaded from disk override" can call
// MarkSource() and Source() to keep the bookkeeping uniform.
var (
	sourceMu sync.RWMutex
	source   = map[string]string{}
)

// MarkSource records that typeName was loaded from origin ("embedded"
// or "disk:<path>"). Pure observation — used by structured startup
// logs.
func MarkSource(typeName, origin string) {
	sourceMu.Lock()
	source[typeName] = origin
	sourceMu.Unlock()
}

// Source returns the recorded origin for typeName, or "" if none was
// recorded.
func Source(typeName string) string {
	sourceMu.RLock()
	defer sourceMu.RUnlock()
	return source[typeName]
}

// OriginFor returns a short label describing whether typeName ended up
// reading from the embedded FS or an on-disk override. Useful for
// startup logging.
func OriginFor(typeName string) string {
	base := PathFromEnv()
	if base == "" {
		return "embedded"
	}
	root := filepath.Join(base, typeName)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "embedded"
	}
	return "disk:" + root
}
