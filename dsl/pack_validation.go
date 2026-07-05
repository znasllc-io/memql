package dsl

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Pack model (epic 2, issue 2.4)
// ==============================
//
// A "pack" is the unit of product-specific extension. It is three
// registration primitives, all called from build-tag-gated init()
// hooks so the build tags decide which node-type binaries carry the
// pack:
//
//  1. memql.RegisterPluginForContract(name, version, factory) -- the Go
//     IntegrationProvider, version-checked by the loader against
//     memql.PluginContractVersion (see component/memql/plugins.go +
//     app/plugins.go; issue 2.1). RegisterPlugin stamps the current
//     version implicitly.
//  2. dsl.RegisterTree(domain, fs) -- the embedded .memql subtree,
//     mounted under domain/ in the unified Tree(). NAMESPACE OWNERSHIP
//     is validated here: a pack's domain must not collide with a core
//     embedded domain or with another pack's already-registered domain.
//  3. node.RegisterRoutingRule(rule) -- cross-node event routing.
//
// Runtime (non-compiled) pack loading is explicitly out of scope: packs
// stay embedded via build tags, like the carrier repo's pack today.
//
// This file owns the namespace-ownership half of pack load-time
// validation. The contract-version half lives in component/memql.

// ValidatePackDomain reports whether a pack may claim `domain` as its
// DSL namespace, given the set of `coreDomains` (top-level directories
// in the embedded tree) and the set of `existing` already-registered
// plugin domains. It is a pure helper -- no global state -- so the
// rule is unit-testable in isolation and reusable by RegisterTree.
//
// The rule, in order:
//
//   - domain must be non-empty (after trimming surrounding whitespace).
//   - domain must not contain a path separator (it names a single
//     top-level namespace, not a nested path).
//   - domain must not collide with a core embedded domain. Core domains
//     are canonical and owned by memQL; a pack cannot shadow or extend
//     one.
//   - domain must not collide with another pack's already-registered
//     domain. Two packs claiming the same namespace is ambiguous and is
//     rejected rather than silently letting the last registration win.
//
// Returns a descriptive error naming the colliding namespace on
// violation, nil when the domain is free to claim. Re-registering the
// SAME domain string that is already in `existing` is treated as a
// collision -- callers that intend idempotent replacement must exclude
// the domain from `existing` before calling (RegisterTree does not, so
// a duplicate domain fails loudly).
func ValidatePackDomain(domain string, coreDomains, existing []string) error {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return fmt.Errorf("pack domain must be non-empty")
	}
	if strings.Contains(trimmed, "/") {
		return fmt.Errorf("pack domain %q must not contain '/' (a domain names a single top-level namespace)", trimmed)
	}
	for _, core := range coreDomains {
		if trimmed == core {
			return fmt.Errorf("pack domain %q collides with core embedded domain %q -- core domains are canonical and cannot be shadowed or extended by a pack; pick a unique namespace", trimmed, core)
		}
	}
	for _, e := range existing {
		if trimmed == e {
			return fmt.Errorf("pack domain %q is already registered by another pack -- two packs cannot own the same namespace; pick a unique namespace", trimmed)
		}
	}
	return nil
}

// coreDomains returns the sorted set of top-level directory names in the
// embedded DSL tree -- the namespaces memQL owns canonically. These are
// the domains a pack may not collide with. Built by reading the embedded
// FS root so it stays in lockstep with the //go:embed directive without a
// hand-maintained list. The "_reference" authoring skeletons directory is
// excluded -- it is not a live DSL namespace a pack could meaningfully
// collide with, and excluding it keeps the rejection message focused on
// real namespaces.
func coreDomains() []string {
	entries, err := fs.ReadDir(embedFS, ".")
	if err != nil {
		// The embedded FS is baked into the binary; a read failure here is
		// a build/embed defect, not a runtime condition. Return empty so
		// validation degrades to "plugin-vs-plugin only" rather than
		// panicking the whole process at this layer; RegisterTree's own
		// guards still reject empty/slash domains.
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// registeredPluginDomains returns the sorted set of domain names already
// in the plugin registry. Caller must hold pluginTreesMu (read or write).
func registeredPluginDomainsLocked() []string {
	out := make([]string, 0, len(pluginTrees))
	for k := range pluginTrees {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
