package genesis

import (
	"fmt"
	"os"
	"strings"
)

// DefaultNodeType is the node type assumed when MEMQL_NODE_TYPE is
// unset. It mirrors the engine default (the bff binary is the no-tag
// build) so the boot validator keys off the same node identity the
// rest of the process does.
const DefaultNodeType = "bff"

// ResolveNodeType returns the boot node type from MEMQL_NODE_TYPE,
// defaulting to DefaultNodeType ("bff") when the var is unset or empty.
// The multi-node fleet keys its required-var set off this value, so
// each pod validates only its own set (a voice node fails on missing
// LiveKit vars; a bff node does not).
func ResolveNodeType() string {
	if nt := strings.TrimSpace(os.Getenv("MEMQL_NODE_TYPE")); nt != "" {
		return nt
	}
	return DefaultNodeType
}

// MissingRequired returns the names of the vars that the given node
// type requires at boot (per the manifest's `required` axis) but that
// are not present, in manifest order. A var counts as present when
// either the lookup returns a non-empty value OR the manifest entry
// carries a non-empty default (a defaulted var is satisfied even when
// the environment omits it). An empty result means every required var
// for this node type is satisfied.
//
// This is the pure, side-effect-free core of the fail-fast boot
// validator (#2108): callers supply the node type, the manifest, and a
// lookup (typically os.LookupEnv) so the policy is unit-testable
// without touching the process environment.
func MissingRequired(nodeType string, m *Manifest, lookup func(string) (string, bool)) []string {
	if m == nil {
		return nil
	}
	defaulted := make(map[string]bool)
	for _, e := range m.AllEntries() {
		if e.Default != "" {
			defaulted[e.Name] = true
		}
	}
	var missing []string
	for _, name := range m.RequiredForNodeType(nodeType) {
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			continue
		}
		if defaulted[name] {
			continue
		}
		missing = append(missing, name)
	}
	return missing
}

// MissingRequiredError formats the missing-required-vars list into a
// single actionable boot error string, or returns "" when nothing is
// missing. The message names the node type, lists every missing var,
// and hints where to set them, so the operator gets ONE clear failure
// instead of a cascade of deep-init panics.
func MissingRequiredError(nodeType string, missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"node type %q requires: %s (set them in the environment / genesis envelope)",
		nodeType, strings.Join(missing, ", "),
	)
}
