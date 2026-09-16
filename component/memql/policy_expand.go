package memql

// policy_expand.go -- `policy:<name>` expansion, cycle detection and the
// closed-grammar check, run once after every policy is registered.
//
// WHY EXPANSION HAPPENS AT LOAD AND NOT AT RESOLVE. A chain the router walks
// must be a list of doors. If `policy:<name>` survived to resolve time, every
// walk would carry a recursion, every cycle would be an infinite loop
// discovered on a live call, and the door report -- the thing that turns "no
// provider available" into a sentence somebody can act on -- would have to
// explain a nesting rather than a sequence. Expanding here means the router
// only ever sees doors, and a cycle is a boot refusal naming the loop.
//
// WHY THE GRAMMAR IS CHECKED HERE TOO, and not only in the parser. The parser
// covers the embedded tree and any bundle that goes through it, but this is
// the load-time gate that lands on the LoadReport and is refused by strict
// boot -- which is what covers a product bundle mounted at MEMQL_DSL_PATH.

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// maxPolicyExpansionDepth bounds the walk. A legitimate chain of chains is two
// or three deep; sixteen is past any authoring intent and stops a pathological
// tree from being explored at all.
const maxPolicyExpansionDepth = 16

// ExpandPolicyChains rewrites every registered policy so its chain contains no
// `policy:<name>` entry, validates every remaining entry against the closed
// grammar, and refuses a cycle naming the loop.
//
// It rewrites Primary and Fallbacks in place rather than caching a second
// chain, so there is exactly one answer to "what does this policy resolve to"
// and no way for a reader to get the un-expanded one.
func ExpandPolicyChains(registry *PolicyRegistry) error {
	if registry == nil {
		return nil
	}
	all := registry.All()

	// Resolve every policy first, then write, so a partially-expanded registry
	// is never observable -- an expansion that failed halfway would otherwise
	// leave some chains rewritten and some not.
	expanded := make(map[string][]string, len(all))
	for _, cfg := range all {
		if cfg == nil {
			continue
		}
		chain, err := expandOne(registry, cfg.Name, nil, 0)
		if err != nil {
			return err
		}
		expanded[cfg.Name] = chain
	}

	for _, cfg := range all {
		if cfg == nil {
			continue
		}
		chain := expanded[cfg.Name]
		for _, entry := range chain {
			if err := languageParser.ValidatePolicyEntry(entry); err != nil {
				return fmt.Errorf("policy %q: %w", cfg.Name, err)
			}
		}
		if len(chain) == 0 {
			cfg.Primary = ""
			cfg.Fallbacks = nil
			continue
		}
		cfg.Primary = chain[0]
		cfg.Fallbacks = append([]string(nil), chain[1:]...)
	}
	return nil
}

// expandOne returns name's chain with every policy: entry replaced by the
// chain it names. stack is the path taken to get here, which is what makes the
// cycle message readable: "a -> b -> a" tells an author which edge to cut,
// where "cycle detected" sends somebody with four policies to read all four.
func expandOne(registry *PolicyRegistry, name string, stack []string, depth int) ([]string, error) {
	if depth > maxPolicyExpansionDepth {
		return nil, fmt.Errorf("policy %q: chain nests more than %d policies deep (%s) -- "+
			"a chain of chains that deep is past any authoring intent",
			name, maxPolicyExpansionDepth, strings.Join(append(stack, name), " -> "))
	}
	for _, seen := range stack {
		if seen == name {
			return nil, fmt.Errorf("policy chain cycles: %s -- a policy that names itself, however "+
				"indirectly, has no chain to walk", strings.Join(append(stack, name), " -> "))
		}
	}
	cfg, ok := registry.Lookup(name)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("policy %q is named by a chain and is not declared anywhere -- "+
			"an unresolvable policy: entry is a door that can never open, and a chain containing "+
			"one silently loses every entry behind it", name)
	}

	stack = append(stack, name)
	out := make([]string, 0, len(cfg.Fallbacks))
	for _, entry := range cfg.ProviderChain() {
		inner, isPolicy := languageParser.IsPolicyEntry(entry)
		if !isPolicy {
			out = append(out, entry)
			continue
		}
		sub, err := expandOne(registry, inner, stack, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return dedupeChain(out), nil
}

// dedupeChain drops a repeated entry, keeping the FIRST occurrence.
//
// Composition makes repeats ordinary rather than exceptional: two policies a
// third one names may both start at fleet:strongest, and the second occurrence
// is an entry the walk would try, find in exactly the state it found it the
// first time, and pass over -- one more line in the door report saying nothing.
// Keeping the first occurrence preserves the author's ordering.
func dedupeChain(chain []string) []string {
	seen := make(map[string]bool, len(chain))
	out := make([]string, 0, len(chain))
	for _, entry := range chain {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out
}
