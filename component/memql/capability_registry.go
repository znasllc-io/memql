package memql

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// capability_registry.go -- expansion of high-level capability slugs into
// the concrete tool names the tool-calling loop understands.
//
// Frontends represent tool capabilities as stable high-level slugs
// (workbench-use, computer-use-headless, ...) because that's what users see
// in agent-creation UIs. The tool dispatcher works in concrete tool names
// (workbenchHost, workerHost, ...). When an agent's record carries a
// capability slug in its tools list, the backend expands the slug into the
// registered tool names before passing the list to the tool-calling loop --
// otherwise the LLM sees a "tool" it can't actually call.
//
// The registry is the single expansion mechanism: the engine registers its
// own bundles (worker_caps.go), and a product pack registers its
// product-specific bundles from init() in its registration-anchor package
// (the same contract as RegisterSuggestDomain / RegisterAppProfile). A
// pure-engine binary simply has the engine bundles only; unknown slugs pass
// through ExpandCapabilitySlugs unchanged so downstream tool-loop filters
// emit clear errors instead of silently dropping references.

// CapabilityTagOperator marks slugs whose tools let an agent drive a UI on
// the user's behalf (autopilot / guided takeover). The agent replier keys
// prompt-rendering decisions (the operator scope-fence rules, app-profile
// injection) on this tag via HasCapabilityTag.
const CapabilityTagOperator = "operator"

type capabilityBundle struct {
	tools []string
	tags  map[string]struct{}
}

var (
	capMu           sync.RWMutex
	capabilitySlugs = map[string]capabilityBundle{}
)

// RegisterCapabilitySlug installs one capability slug -> tool-name bundle.
// Call from init(); panics on an empty slug or a duplicate registration
// (loud programmer error). An empty tools list is valid -- it means "this
// capability provides no extra tools". Optional tags classify the bundle
// (see CapabilityTagOperator).
func RegisterCapabilitySlug(slug string, tools []string, tags ...string) {
	if slug == "" {
		panic("memql.RegisterCapabilitySlug: empty slug")
	}
	capMu.Lock()
	defer capMu.Unlock()
	if _, dup := capabilitySlugs[slug]; dup {
		panic(fmt.Sprintf("memql.RegisterCapabilitySlug: duplicate registration for %q", slug))
	}
	bundle := capabilityBundle{tools: append([]string(nil), tools...)}
	if len(tags) > 0 {
		bundle.tags = make(map[string]struct{}, len(tags))
		for _, t := range tags {
			bundle.tags[t] = struct{}{}
		}
	}
	capabilitySlugs[slug] = bundle
}

// UnregisterCapabilitySlug removes a registration. Test teardown only
// (mirrors dsl.UnregisterTree).
func UnregisterCapabilitySlug(slug string) {
	capMu.Lock()
	defer capMu.Unlock()
	delete(capabilitySlugs, slug)
}

// ExpandCapabilitySlugs takes a raw tool list from an Agent record
// (possibly containing both concrete tool names like "workbenchHost" and
// capability slugs like "workbench-use") and returns a de-duplicated, flat
// list of concrete tool names.
//
// Ordering: concrete names from the input are preserved in order; slug
// expansions are appended in the order the slug was seen, with the slug
// removed from the output. Duplicates are collapsed on first occurrence.
//
// Unknown slugs are passed through unchanged so we don't silently drop
// tool references -- the downstream tool-loop filter will reject them with
// a clear "unknown tool" error.
func ExpandCapabilitySlugs(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	capMu.RLock()
	defer capMu.RUnlock()
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, entry := range raw {
		if bundle, ok := capabilitySlugs[entry]; ok {
			for _, name := range bundle.tools {
				add(name)
			}
			continue
		}
		add(entry)
	}
	return out
}

// HasCapabilityTag reports whether the expanded tool list includes any tool
// of any slug registered with the given tag. Used to route prompt-rendering
// decisions (e.g. "apply the operator scope-fence rules") without
// re-expanding inside the template layer.
func HasCapabilityTag(expanded []string, tag string) bool {
	capMu.RLock()
	defer capMu.RUnlock()
	tagged := map[string]struct{}{}
	for _, bundle := range capabilitySlugs {
		if _, ok := bundle.tags[tag]; !ok {
			continue
		}
		for _, name := range bundle.tools {
			tagged[name] = struct{}{}
		}
	}
	if len(tagged) == 0 {
		return false
	}
	for _, name := range expanded {
		if _, ok := tagged[name]; ok {
			return true
		}
	}
	return false
}

// VerifyCapabilityToolsRegistered is a boot-time self-check (memql#1156).
// It guards against the failure mode where a tool slice silently fails to
// parse and is dropped from the registry (e.g. memql#1154's unrecognized
// @requires annotation), so the agent only discovers the missing tool at
// RUNTIME as "tool not in registry" -- by which point produceArtifact can
// already be looping.
//
// For each capability slug, if ANY of its expanded tools is registered (i.e.
// this node loads that capability surface) then ALL of them must be; a partial
// set is the bug. This self-scopes cleanly: on a node that doesn't load the
// relevant tool tree at all, NONE of a slug's tools are registered, so the
// slug is skipped -- no false alarm. It logs ONE loud ERROR per incomplete
// slug (turning a silent-until-runtime failure into a loud-at-boot one) and
// returns the full missing set so callers/tests can fail fast.
//
// A slug with NONE of its tools registered is NOT an error -- see above -- but
// it is no longer SILENT either (memql#4692). It is reported through
// UnavailableCapabilitySlugs, and the boot log states it, because "this node
// does not provide capability X" and "this node provides it" produce identical
// output otherwise, and an agent whose role locks that slug will name the tool
// all the way into the model's prompt regardless. The engine cannot tell the
// two apart on its own -- workbenchHost is legitimately pack-owned, so an
// engine-only cluster missing it is correct, and failing boot on it would fail
// every such cluster. What it CAN do is say so once, where an operator looking
// at "the goal produced nothing" will find it.
//
// `has` is the registry membership probe (pass toolRegistry.Has at boot).
func VerifyCapabilityToolsRegistered(has func(string) bool, logger *slog.Logger) []string {
	missing, unavailable := verifyCapabilityTools(has)
	if logger != nil {
		for _, slug := range unavailable {
			logger.Warn("capability slug is UNAVAILABLE on this node -- none of its tools are in the registry. "+
				"This is expected where the capability is pack-owned and this deployment does not load that pack, "+
				"and it is NOT a boot error. But an agent whose role locks this slug still carries the tool NAMES, "+
				"so anything reading the name list rather than the registry will believe the capability is present "+
				"(memql#4692).",
				"capability", slug,
				"tools", CapabilityToolNames(slug),
			)
		}
	}
	if logger != nil {
		logMissingCapabilityTools(missing, logger)
	}
	return flattenMissing(missing)
}

// UnavailableCapabilitySlugs returns the slugs with NONE of their tools
// registered. Exported so a deployment's own boot checks and tests can assert
// which capabilities a build actually provides, rather than inferring it from
// the absence of an error.
func UnavailableCapabilitySlugs(has func(string) bool) []string {
	_, unavailable := verifyCapabilityTools(has)
	return unavailable
}

// CapabilityToolNames returns the tools a slug expands to, or nil.
func CapabilityToolNames(slug string) []string {
	capMu.RLock()
	defer capMu.RUnlock()
	bundle, ok := capabilitySlugs[slug]
	if !ok {
		return nil
	}
	out := make([]string, len(bundle.tools))
	copy(out, bundle.tools)
	return out
}

// verifyCapabilityTools splits every registered slug into the two outcomes that
// must not be conflated: PARTIALLY registered (a real bug -- a tool slice
// failed to parse) and FULLY absent (this node does not load that surface).
// Both lists are sorted so two boots of the same build log the same thing.
func verifyCapabilityTools(has func(string) bool) (map[string][]string, []string) {
	if has == nil {
		return nil, nil
	}
	capMu.RLock()
	defer capMu.RUnlock()
	missing := map[string][]string{}
	var unavailable []string
	for slug, bundle := range capabilitySlugs {
		anyPresent := false
		var absent []string
		for _, name := range bundle.tools {
			if has(name) {
				anyPresent = true
			} else {
				absent = append(absent, name)
			}
		}
		switch {
		case anyPresent && len(absent) > 0:
			missing[slug] = absent
		case !anyPresent && len(bundle.tools) > 0:
			unavailable = append(unavailable, slug)
		}
	}
	sort.Strings(unavailable)
	return missing, unavailable
}

func flattenMissing(missing map[string][]string) []string {
	if len(missing) == 0 {
		return nil
	}
	slugs := make([]string, 0, len(missing))
	for slug := range missing {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	var out []string
	for _, slug := range slugs {
		out = append(out, missing[slug]...)
	}
	return out
}

func logMissingCapabilityTools(missing map[string][]string, logger *slog.Logger) {
	slugs := make([]string, 0, len(missing))
	for slug := range missing {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		logger.Error("capability tool(s) MISSING from the tool registry -- a tool slice likely failed to parse and was SILENTLY skipped by the loader; the agent will hit 'tool not in registry' at runtime and can loop. Fix the tool definition (memql#1156 / #1154).",
			"capability", slug,
			"missing", missing[slug],
			"expected", CapabilityToolNames(slug),
		)
	}
}
