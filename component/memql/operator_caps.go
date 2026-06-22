package memql

import "log/slog"

// operatorCaps.go — Expansion of CoPresent Control capability slugs
// into the concrete tool names the tool-calling loop understands.
//
// The CoPresent frontend represents tool capabilities as stable
// high-level slugs (copresent-control, data-query, email-compose, …)
// because that's what users see in agent-creation UIs. The tool
// dispatcher, by contrast, works in concrete tool names (uiClick,
// uiType, uiReadState, …). When an agent's Agent record carries a
// capability slug in its tools list, the backend must expand the
// slug into the registered tool names before passing the list to
// the tool-calling loop — otherwise the LLM sees a "tool" it can't
// actually call.
//
// Capability-slug expansions that live here today:
//
//   - copresent-takeover      -- expands into uiClick / uiType / etc.,
//                                the tools that drive the CoPresent SPA on
//                                autopilot (the AI does it FOR the user).
//   - copresent-guide         -- same operator primitives, but the immersive
//                                voice-driven Scene experience (the AI does it
//                                WITH the user). Both replaced the retired
//                                single `copresent-control` slug (copresent#187).
//   - computer-use-headless   -- expands into workerHost + the cross-
//                                cutting trio (workerStatus,
//                                requestComputerUseScope, canvasPublish).
//                                Shell / fs / http on the user's machine.
//   - computer-use-embodied   -- expands into workerComputer + the same
//                                cross-cutting trio. Mouse / keyboard /
//                                screenshot on the user's machine.
//   - workbench-use           -- expands into workbenchHost + canvasPublish.
//                                Sandboxed Linux execution per-Plan in the
//                                cluster. Universal: every agent has it
//                                by default. No scope-request tool (no
//                                blast radius to gate against) and no
//                                status tool (the workbench is a cluster
//                                service, not a remote process that can
//                                disconnect).
//
// The two computer-use slugs replaced a single legacy `computer-use`
// slug on 2026-05-17. Splitting by mode lets the headless slice be
// served by the workbench backend without dragging the embodied
// (GUI-only) tools along. Authorization (scope grants, kill switch,
// knowledge domain) is still unified under the "computer use"
// concept because both modes act on the user's machine. workbench-use
// is a sibling, not a child -- it's the safer default for headless
// work and runs entirely inside the cluster.
//
// If additional capability bundles get added in the future, add
// their names here and the expansion picks them up automatically.

// OperatorPrimitiveNames is the canonical list of tool names
// registered by the operator subsystem. An agent holding either the
// `copresent-takeover` or `copresent-guide` capability slug can call
// every one of these.
//
// Keep this list in sync with the tool JSON files in
// tools/v1/copresent/operator/. Missing an entry here means the
// agent's prompt lists the tool (so it tries to call it) but the
// filtered-tools loop rejects it. Extra entries here that don't
// exist in the registry are silently ignored downstream by
// InvokeAIChatWithFilteredTools.
var OperatorPrimitiveNames = []string{
	"uiRequestControl",
	"uiReleaseControl",
	"uiReadState",
	"uiDescribe",
	"uiClick",
	"uiType",
	"uiSelect",
	"uiHighlight",
	"uiNavigate",
	"uiPointerTo",
	"uiAskUser",
	"uiWaitFor",
	"uiRetry",
	"uiNarrate",
	"agentUpdateSelf",
	// similarTo lets the agent pull top-K nodes of a given concept
	// ranked by cosine similarity to a free-form query -- typically
	// app-knowledge chunks from its own declared knowledge domains,
	// mid-takeover. The replier seeds the prompt with an up-front
	// top-5 (copresent-ui) for operator turns; this tool is for
	// follow-up depth when the agent hits a widget or flow it
	// doesn't recognise from the initial block. Generalised over the
	// target concept so any vector-indexed concept is reachable.
	"similarTo",
}

// workerCrossCuttingNames are the tools shared by both computer-use
// modes: status (live availability check), scope elevation (per-task
// approval flow), and canvas publish (surface "what I just did"
// cards after a worker call). Every computer-use-capable agent
// needs these regardless of which mode it's using -- the user
// expects a task-done card showing the actual outcome (file
// created, command output, screenshot taken, etc.), not just a
// chat-line "I did it."
var workerCrossCuttingNames = []string{
	"workerStatus",
	"requestComputerUseScope",
	"canvasPublish",
}

// WorkerHeadlessCapabilityNames is the canonical tool list served
// to agents that hold `computer-use-headless`: shell / fs / http
// on the user's machine, plus the cross-cutting trio. Future
// sandbox capability will parallel this one (same headless verbs,
// different backend); the embodied slug is the GUI-only sibling.
//
// Keep in sync with tools/v1/agent/worker/workerHost.memql +
// tools/v1/copresent/canvas/toolCanvasPublish.memql.
var WorkerHeadlessCapabilityNames = append(
	[]string{"workerHost"},
	workerCrossCuttingNames...,
)

// WorkerEmbodiedCapabilityNames is the canonical tool list served
// to agents that hold `computer-use-embodied`: mouse / keyboard /
// screenshot on the user's machine, plus the cross-cutting trio.
// Mode-exclusive -- no sandbox analogue exists since the agent
// needs a real display.
//
// Keep in sync with tools/v1/agent/worker/workerComputer.memql.
var WorkerEmbodiedCapabilityNames = append(
	[]string{"workerComputer"},
	workerCrossCuttingNames...,
)

// WorkbenchCapabilityNames is the canonical tool list served to
// agents that hold `workbench-use`: headless shell / fs / http
// operations against a sandboxed per-Plan Linux environment in
// the cluster, plus canvasPublish so the agent can surface
// "what I just did" cards (file written, command output) after
// a successful workbenchHost call.
//
// Universal capability -- expected to be default-on for every
// agent. No status tool (the workbench is a cluster service,
// not a process that can disconnect), no scope-request tool
// (no host blast radius to gate against; the workspace is
// torn down with the Plan).
//
// Keep in sync with the workbenchHost tool definition.
var WorkbenchCapabilityNames = []string{
	"workbenchHost",
	"canvasPublish",
}

// capabilitySlugs maps high-level capability slugs to the concrete
// tool names they expand to. The zero slug list (empty inner slice)
// is valid — it means "this capability provides no extra tools", an
// edge case we don't currently use but the expander handles
// gracefully.
var capabilitySlugs = map[string][]string{
	// CoPresent Control v2 (copresent#187): the single `copresent-control`
	// slug was split into two purpose-built bundles -- `copresent-takeover`
	// (autopilot) and `copresent-guide` (immersive voice-driven Scenes).
	// Both expand to the SAME operator primitives; the EXPERIENCE differs
	// (gated by which skill/tool the agent holds), not the primitive set.
	"copresent-takeover": OperatorPrimitiveNames,
	"copresent-guide":    OperatorPrimitiveNames,
	// Legacy alias retained for migration: GA agent rows materialized
	// before the split still carry skillId/toolSlug `copresent-control`
	// (the per-user assistant seed is insert-if-missing, so existing rows
	// are not rewritten). Keep expanding it so those GAs keep driving the
	// UI until they are re-provisioned with the new skills.
	"copresent-control":     OperatorPrimitiveNames,
	"computer-use-headless": WorkerHeadlessCapabilityNames,
	"computer-use-embodied": WorkerEmbodiedCapabilityNames,
	"workbench-use":         WorkbenchCapabilityNames,
}

// ExpandCapabilitySlugs takes a raw tool list from an Agent record
// (possibly containing both concrete tool names like "uiClick" and
// capability slugs like "copresent-control") and returns a
// de-duplicated, flat list of concrete tool names.
//
// Ordering: concrete names from the input are preserved in order;
// slug expansions are appended in the order the slug was seen, with
// the slug removed from the output. Duplicates are collapsed on
// first occurrence.
//
// Unknown slugs are passed through unchanged so we don't silently
// drop tool references — the downstream tool-loop filter will reject
// them with a clear "unknown tool" error.
func ExpandCapabilitySlugs(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw)*2)
	out := make([]string, 0, len(raw)*2)
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
		if expansion, ok := capabilitySlugs[entry]; ok {
			for _, name := range expansion {
				add(name)
			}
			continue
		}
		add(entry)
	}
	return out
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
// carrier/agent tool tree at all (voice, identity), NONE of a slug's tools are
// registered, so the slug is skipped -- no false alarm. It logs ONE loud ERROR
// per incomplete slug (turning a silent-until-runtime failure into a loud-at-
// boot one) and returns the full missing set so callers/tests can fail fast.
//
// `has` is the registry membership probe (pass toolRegistry.Has at boot).
func VerifyCapabilityToolsRegistered(has func(string) bool, logger *slog.Logger) []string {
	if has == nil {
		return nil
	}
	var allMissing []string
	for slug, tools := range capabilitySlugs {
		anyPresent := false
		var missing []string
		for _, name := range tools {
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
					"expected", tools,
				)
			}
			allMissing = append(allMissing, missing...)
		}
	}
	return allMissing
}

// HasOperatorCapability reports whether the expanded tool list
// includes any operator primitive. Used to route prompt-rendering
// decisions (e.g. "apply the Operator scope fence rules") without
// re-expanding inside the template layer.
func HasOperatorCapability(expanded []string) bool {
	for _, name := range expanded {
		for _, prim := range OperatorPrimitiveNames {
			if name == prim {
				return true
			}
		}
	}
	return false
}
