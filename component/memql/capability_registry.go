package memql

import (
	"fmt"
	"log/slog"
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
// `has` is the registry membership probe (pass toolRegistry.Has at boot).
func VerifyCapabilityToolsRegistered(has func(string) bool, logger *slog.Logger) []string {
	if has == nil {
		return nil
	}
	capMu.RLock()
	defer capMu.RUnlock()
	var allMissing []string
	for slug, bundle := range capabilitySlugs {
		anyPresent := false
		var missing []string
		for _, name := range bundle.tools {
			if has(name) {
				anyPresent = true
			} else {
				missing = append(missing, name)
			}
		}
		if anyPresent && len(missing) > 0 {
			if logger != nil {
				logger.Error("capability tool(s) MISSING from the tool registry -- a tool slice likely failed to parse and was SILENTLY skipped by the loader; the agent will hit 'tool not in registry' at runtime and can loop. Fix the tool definition (memql#1156 / #1154).",
					"capability", slug,
					"missing", missing,
					"expected", bundle.tools,
				)
			}
			allMissing = append(allMissing, missing...)
		}
	}
	return allMissing
}
