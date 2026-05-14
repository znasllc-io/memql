package dslfs

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Import-path resolution for the DSL import-model refactor
// (docs/dsl-import-model-refactor.md). This file is the pure,
// filesystem-free piece: given the importing file's path, the raw
// import string from source, and the DSL root, it returns the
// canonical path of the imported file inside the root or a typed
// error explaining why the import is invalid.
//
// The loader's symlink-resolve + root-cap enforcement on the real
// disk lives in the disk-mode override path; the embedded FS has no
// symlinks, so the canonical-path comparison is enough.

// ResolveImport canonicalizes an `import "<path>" [as alias]` entry.
//
//   - importingFile is the slash-separated path of the file the
//     import lives in, RELATIVE TO THE DSL ROOT (no leading slash).
//     Example: "cognition/queries/spaceParticipants.memql".
//
//   - rawPath is the string literal from the source (e.g.
//     "./common/space", "../identity/user", "./foo.memql").
//
// The function:
//
//   - Rejects absolute paths (starts with "/") and URLs (anything
//     with "://").
//   - Joins rawPath with the importing file's directory.
//   - Cleans the result (collapses "./" and "../" segments).
//   - Auto-appends ".memql" when rawPath has no extension.
//   - Verifies the cleaned path does NOT escape the DSL root
//     (no leading "../" after cleaning).
//
// Returns the canonical, root-relative path of the imported file
// (always slash-separated, ".memql" extension included). Note: this
// function does not touch the filesystem — existence checks happen
// in the loader after resolution.
func ResolveImport(importingFile, rawPath string) (string, error) {
	if importingFile == "" {
		return "", fmt.Errorf("importing file path cannot be empty")
	}
	if rawPath == "" {
		return "", fmt.Errorf("import path cannot be empty")
	}

	// Reject absolute paths. Imports must be relative.
	if strings.HasPrefix(rawPath, "/") {
		return "", fmt.Errorf("absolute import path %q not allowed; use a relative path (./... or ../...)", rawPath)
	}

	// Reject URL-shaped paths. Imports are filesystem-only.
	if strings.Contains(rawPath, "://") {
		return "", fmt.Errorf("URL-shaped import path %q not allowed; only relative file paths are supported", rawPath)
	}

	// Auto-append `.memql` if there's no extension. We check the
	// final segment specifically (so "./foo.bar/baz" with ".bar"
	// in the directory still gets ".memql" on "baz").
	final := path.Base(rawPath)
	if !strings.Contains(final, ".") {
		rawPath += ".memql"
	} else if !strings.HasSuffix(rawPath, ".memql") {
		// Tolerate explicit ".memql"; reject any other extension.
		return "", fmt.Errorf("import path %q has unsupported extension; only .memql files can be imported", rawPath)
	}

	dir := path.Dir(importingFile)
	joined := path.Join(dir, rawPath)

	// path.Clean handles "../" collapse. Anything that still starts
	// with ".." after the join+clean means the import escaped the
	// root.
	clean := path.Clean(joined)
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("import path %q escapes the DSL root (resolved to %q)", rawPath, clean)
	}

	return clean, nil
}

// aliasIdentifier regexes — the basename-derived default alias must
// be a legal identifier; reserved engine names cannot be used as
// aliases.
var (
	aliasIdentRe   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	reservedAlias  = map[string]bool{"actor": true, "now": true, "partition": true, "config": true, "trace": true}
	ErrAliasReserved = errors.New("alias collides with reserved engine identifier")
)

// DefaultAlias derives the alias for an import that omitted the
// `as` clause. The rule is: take the basename of the resolved
// import path (without ".memql"), and require it to be a legal
// identifier. Reserved names (actor, now, partition, config,
// trace) are rejected even as defaults — those collide with the
// engine-provided body identifiers.
//
// The caller is responsible for collision detection across the
// import block — DefaultAlias only computes the per-import default.
func DefaultAlias(resolvedPath string) (string, error) {
	base := path.Base(resolvedPath)
	base = strings.TrimSuffix(base, ".memql")
	if base == "" {
		return "", fmt.Errorf("cannot derive default alias from empty basename of %q", resolvedPath)
	}
	if !aliasIdentRe.MatchString(base) {
		return "", fmt.Errorf("default alias %q for import %q is not a legal identifier; add an `as <name>` clause", base, resolvedPath)
	}
	if reservedAlias[base] {
		return "", fmt.Errorf("%w: %q (use `as <name>` to rebind)", ErrAliasReserved, base)
	}
	return base, nil
}

// ValidateAlias enforces the rules for an explicit `as <alias>`
// clause: legal identifier + not a reserved engine name.
func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias cannot be empty")
	}
	if !aliasIdentRe.MatchString(alias) {
		return fmt.Errorf("alias %q is not a legal identifier (must match [A-Za-z_][A-Za-z0-9_]*)", alias)
	}
	if reservedAlias[alias] {
		return fmt.Errorf("%w: %q", ErrAliasReserved, alias)
	}
	return nil
}
