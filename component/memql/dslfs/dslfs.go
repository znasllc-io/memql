// Package dslfs holds the MEMQL_DSL_PATH runtime-override helpers shared by
// the DSL loading path.
//
// Product DSL is delivered to a product-agnostic engine node at boot by
// mounting each product-domain directory under MEMQL_DSL_PATH into the unified
// tree via RegisterTree (see dsl.MountRuntimeDomainsFromEnv). This package
// exposes the low-level env accessor (PathFromEnv) and an origin-label helper
// (OriginFor) used for structured logging + the cockpit's pack-browser origin
// column.
package dslfs

import (
	"os"
	"path/filepath"
	"strings"
)

const envPathKey = "MEMQL_DSL_PATH"

// PathFromEnv returns the override root from MEMQL_DSL_PATH, or "" if
// the variable is unset or empty. Whitespace is trimmed.
func PathFromEnv() string {
	return strings.TrimSpace(os.Getenv(envPathKey))
}

// OriginFor returns a short label describing whether the named subtree ended
// up reading from the embedded FS or an on-disk override under
// MEMQL_DSL_PATH. Useful for startup logging + pack-browser display. Returns
// "embedded" when MEMQL_DSL_PATH is unset or has no matching sub-directory,
// else "disk:<path>".
func OriginFor(name string) string {
	base := PathFromEnv()
	if base == "" {
		return "embedded"
	}
	root := filepath.Join(base, name)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "embedded"
	}
	return "disk:" + root
}
