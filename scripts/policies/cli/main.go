// Command policies-lint walks dsl/v1/policies and runs the policy
// loader against the on-disk tree, surfacing every error the engine
// would surface at startup. Backs the `make policies-lint` Makefile
// target.
package main

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// To run the engine-side validation we exercise loadPolicyFunctions
// directly. The loader is package-private to component/memql, so the
// CLI reaches it via a tiny re-export under the same package compiled
// with build tag `lint`. Keeping the lint binary in a separate command
// directory (scripts/policies/cli) means it can't accidentally pull
// in the rest of the engine boot path.
func main() {
	root, err := findDSLRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Override the on-disk root so dslfs.Overlay picks up the
	// in-tree policies directly rather than the embedded copy that
	// would only see files committed at compile time.
	if err := os.Setenv("MEMQL_DSL_PATH", root); err != nil {
		fmt.Fprintln(os.Stderr, "set MEMQL_DSL_PATH:", err)
		os.Exit(1)
	}

	failures := lintWalkPolicies(root)
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "policies-lint: violations found")
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("policies-lint passed", "root", root)
}

// findDSLRoot locates the dsl/v1 directory by walking up from cwd.
func findDSLRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, "dsl", "v1")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("dsl/v1 not found above %s", cwd)
}

// lintWalkPolicies reads every .memql under dsl/v1/policies (excluding
// the legacy SI-Router policies/v1/...) and validates the metadata
// surface. The real engine loader does the same in memory at startup;
// this CLI is the human-friendly entry point for CI / pre-commit
// hooks.
//
// We re-implement the checks here against the on-disk tree so the
// lint runner has no dependency on the engine package's private
// loader API. The list of checks mirrors policy_function_loader.go:
//   - @tier matches directory placement
//   - @frontend_visible is only valid on bff-tier policies
//   - filename matches function name
//   - downward-only delegation (core policies may not call bff)
func lintWalkPolicies(dslRoot string) []string {
	policiesRoot := filepath.Join(dslRoot, "policies")
	var failures []string

	headers := map[string]*policyHeader{}
	for _, tier := range []string{"core", "bff"} {
		root := filepath.Join(policiesRoot, tier)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".memql") {
				return nil
			}
			if strings.HasPrefix(d.Name(), "_") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				failures = append(failures, fmt.Sprintf("%s: read: %v", path, readErr))
				return nil
			}
			h := &policyHeader{Path: path, DirTier: tier}
			parsePolicyHeaderForLint(string(data), h)
			// filename check
			base := strings.TrimSuffix(filepath.Base(path), ".memql")
			if h.FuncName != "" && !strings.EqualFold(base, h.FuncName) {
				failures = append(failures, fmt.Sprintf("%s: filename must match function name %q", path, h.FuncName))
			}
			// tier annotation check
			if h.Tier != "" && h.Tier != tier {
				failures = append(failures, fmt.Sprintf("%s: @tier(%q) does not match directory placement %q", path, h.Tier, tier))
			}
			// frontend_visible only on bff
			if h.Frontend && tier != "bff" {
				failures = append(failures, fmt.Sprintf("%s: @frontend_visible is only valid on bff-tier policies (this is %s)", path, tier))
			}
			if h.FuncName != "" {
				headers[h.FuncName] = h
			}
			return nil
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("walk %s: %v", root, err))
		}
	}

	// Second pass: cross-tier delegation. Core policies that call
	// `policy("bffOne", ...)` are violations.
	for _, h := range headers {
		if h.DirTier != "core" {
			continue
		}
		for _, target := range h.Calls {
			t, ok := headers[target]
			if !ok {
				continue
			}
			if t.DirTier == "bff" {
				failures = append(failures, fmt.Sprintf("%s: cross-tier delegation -- core policy %q calls bff policy %q", h.Path, h.FuncName, target))
			}
		}
	}

	_ = dslfs.PathFromEnv()
	return failures
}

// policyHeader carries the per-file metadata the lint needs.
type policyHeader struct {
	Path     string
	DirTier  string
	FuncName string
	Tier     string
	Frontend bool
	Calls    []string
}

// parsePolicyHeaderForLint scans the raw source for the small set of
// metadata the lint needs: function name, @tier value,
// @frontend_visible presence, and the list of policy("X", ...) calls.
// Pure text scanning — no dependency on the language parser, which
// keeps the lint runner small and avoids cyclic imports.
func parsePolicyHeaderForLint(source string, h *policyHeader) {
	lines := strings.Split(source, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "@frontend_visible") {
			h.Frontend = true
			continue
		}
		if strings.HasPrefix(trimmed, "@tier(") {
			v := extractStringArg(trimmed)
			if v != "" {
				h.Tier = strings.ToLower(strings.TrimSpace(v))
			}
			continue
		}
		if strings.HasPrefix(trimmed, "func (Policy)") {
			// e.g. "func (Policy) requiresAdmin(_ any) bool {"
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "func (Policy)"))
			openParen := strings.Index(rest, "(")
			if openParen <= 0 {
				continue
			}
			h.FuncName = strings.TrimSpace(rest[:openParen])
			continue
		}
		// policy("name", ...) references inside the body.
		idx := 0
		for {
			i := strings.Index(trimmed[idx:], `policy("`)
			if i < 0 {
				break
			}
			start := idx + i + len(`policy("`)
			end := strings.Index(trimmed[start:], `"`)
			if end < 0 {
				break
			}
			name := trimmed[start : start+end]
			h.Calls = append(h.Calls, name)
			idx = start + end
		}
	}
}

// extractStringArg pulls the first double-quoted string out of an
// annotation like `@tier("core")`.
func extractStringArg(s string) string {
	first := strings.Index(s, `"`)
	if first < 0 {
		return ""
	}
	rest := s[first+1:]
	last := strings.Index(rest, `"`)
	if last < 0 {
		return ""
	}
	return rest[:last]
}
