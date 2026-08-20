package dsl

import (
	"sort"
	"strings"
	"sync"
)

// pack_enablement.go carries the per-instance pack disablement set for this
// process (module-registry design,
// docs/superpowers/specs/2026-08-20-module-registry-design.md section 4.2).
//
// The DURABLE state lives in the graph -- one v1:platform:packState row per
// pack, absence meaning enabled -- because per-node env is exactly how a
// two-replica mesh ends up running two different products. What lives HERE
// is that state's boot-time projection: app phase 3 reads the rows after
// the database starts and before engine.Init runs the DSL loaders, and
// hands the disabled set to this package. Everything downstream consults
// one answer.
//
// A DISABLED PACK IS MOUNTED-INERT, NOT UNMOUNTED:
//
//   - Its tree stays registered (RegisterTree is init()-time and
//     unconditional), so the namespace stays owned and a colliding second
//     registration is refused exactly as when enabled. Disablement never
//     frees a namespace.
//   - Its CONCEPTS still load. Schemas are declarative and inert; keeping
//     them keeps cross-domain `use <pack>.concepts.{...}` imports and
//     @relationship targets resolving (dsl/actions imports harness plan +
//     step), and keeps rows written before the flip browsable. That is why
//     SkipsBehavioralLoad carves concepts.memql out of the skip.
//   - Every BEHAVIORAL construct -- queries, mutations, builtins, tools,
//     prompts, logic, automations, specs, shapes, seeds -- is skipped at
//     load, so it is absent from the registries rather than present and
//     refusing.
//
// The skip is applied at the two places behavioral loads read the tree:
// baseloader.ReadAll (every component/memql unified loader, the contract
// gates, the duplicate detector, the construct catalog) and the
// automations walker (component/automations.LoadFromUnifiedTree). Keeping
// the predicate here, next to Tree(), is what lets both consumers agree
// without a dependency on the engine package.
//
// The set is written once at boot (and by tests); a flip of the graph row
// takes effect as each node restarts -- restart-required is the v1
// lifecycle, and the module inventory reports enabled-vs-loaded honestly
// in between.

var (
	disabledPacksMu sync.RWMutex
	disabledPacks   = map[string]struct{}{}
)

// SetDisabledPackDomains replaces the process's disabled-pack set. Called
// from app bootstrap after the v1:platform:packState read, before the DSL
// loaders run; also the test seam. Nil or empty means every pack is
// enabled, which is the default state and the fresh-database state.
func SetDisabledPackDomains(domains []string) {
	next := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		trimmed := strings.TrimSpace(d)
		if trimmed == "" {
			continue
		}
		next[trimmed] = struct{}{}
	}
	disabledPacksMu.Lock()
	disabledPacks = next
	disabledPacksMu.Unlock()
}

// DisabledPackDomains returns the disabled-pack set, sorted, for
// reporting. Empty in the default all-enabled state.
func DisabledPackDomains() []string {
	disabledPacksMu.RLock()
	defer disabledPacksMu.RUnlock()
	out := make([]string, 0, len(disabledPacks))
	for d := range disabledPacks {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// PackDomainDisabled reports whether the domain is in the disabled set.
func PackDomainDisabled(domain string) bool {
	disabledPacksMu.RLock()
	defer disabledPacksMu.RUnlock()
	_, ok := disabledPacks[strings.TrimSpace(domain)]
	return ok
}

// SkipsBehavioralLoad reports whether a tree path belongs to a disabled
// pack's BEHAVIORAL surface and must therefore be invisible to the
// loaders. True for every file under a disabled domain EXCEPT its
// concepts.memql -- see the mounted-inert rationale in the file comment.
//
// The path is a merged-tree path ("harness/mutations.memql"); the first
// segment is the domain. A concept declared outside concepts.memql in a
// disabled pack would be skipped with the rest of its file -- the
// per-construct layout makes that shape convention-violating already, and
// the loader convention (one construct kind per <kind>s.memql) is what
// this predicate keys on.
func SkipsBehavioralLoad(path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}
	domain := trimmed
	if idx := strings.IndexByte(trimmed, '/'); idx >= 0 {
		domain = trimmed[:idx]
	}
	if !PackDomainDisabled(domain) {
		return false
	}
	base := trimmed
	if idx := strings.LastIndexByte(trimmed, '/'); idx >= 0 {
		base = trimmed[idx+1:]
	}
	return base != "concepts.memql"
}
