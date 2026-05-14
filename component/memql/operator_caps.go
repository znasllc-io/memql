package memql

// operatorCaps.go — Expansion of CoPresent Control capability slugs
// into the concrete tool names the tool-calling loop understands.
//
// The CoPresent frontend represents tool capabilities as stable
// high-level slugs (copresent_control, data_query, email_compose, …)
// because that's what users see in agent-creation UIs. The tool
// dispatcher, by contrast, works in concrete tool names (uiClick,
// uiType, uiReadState, …). When an agent's Agent record carries a
// capability slug in its tools list, the backend must expand the
// slug into the registered tool names before passing the list to
// the tool-calling loop — otherwise the LLM sees a "tool" it can't
// actually call.
//
// Two capability-slug expansions live here today:
//
//   - copresent_control -- expands into uiClick / uiType / etc., the
//                          tools that drive the CoPresent SPA.
//   - computer_use      -- expands into workerHost / workerComputer
//                          (already stored as primitives by the
//                          frontend's toolsToSlugs fan-out, but kept
//                          here for the backend-only `workerStatus`
//                          which IS NOT in the frontend's bundle list
//                          -- expanding it server-side means existing
//                          agents pick it up without re-saving).
//
// If additional capability bundles get added in the future, add
// their names here and the expansion picks them up automatically.

// OperatorPrimitiveNames is the canonical list of tool names
// registered by the operator subsystem. An agent with the
// `copresent_control` capability slug can call every one of these.
//
// Keep this list in sync with the tool JSON files in
// tools/v1/copresent/operator/. Missing an entry here means the
// agent's prompt lists the tool (so it tries to call it) but the
// filtered-tools loop rejects it. Extra entries here that don't
// exist in the registry are silently ignored downstream by
// InvokeSIChatWithFilteredTools.
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
	// mid-takeover. The delegateTakeover handler already seeds the
	// prompt with an up-front top-5 for the goal; this tool is for
	// follow-up depth when the agent hits a widget or flow it
	// doesn't recognise from the initial block. Generalised over the
	// target concept so any vector-indexed concept is reachable.
	"similarTo",
}

// WorkerCapabilityNames is the canonical list of tool names served
// to agents that hold the `computer_use` capability slug. The
// frontend's toolsToSlugs already fans `computer_use` out into
// workerHost + workerComputer at save time, but it omits
// `workerStatus`, `requestComputerUseScope`, and `canvasPublish` --
// internal-only / cross-cutting tools the user shouldn't see as
// separate UI chips. Listing them here means every agent with
// computer_use gets them in their LLM-visible tool list
// automatically, with no migration of stored records.
//
// `canvasPublish` is included because a computer_use-capable agent
// needs a way to surface "what I just did" cards on the canvas
// after a successful workerHost / workerComputer call -- the user
// expects a task-done card showing the actual outcome (file
// created, command output, etc.), not just a chat-line "I did it."
//
// Keep this in sync with tools/v1/agent/worker/*.memql +
// tools/v1/copresent/canvas/toolCanvasPublish.memql. See
// ExpandCapabilitySlugs for the expansion logic that consumes
// this list.
var WorkerCapabilityNames = []string{
	"workerHost",
	"workerComputer",
	"workerStatus",
	"requestComputerUseScope",
	"canvasPublish",
}

// capabilitySlugs maps high-level capability slugs to the concrete
// tool names they expand to. The zero slug list (empty inner slice)
// is valid — it means "this capability provides no extra tools", an
// edge case we don't currently use but the expander handles
// gracefully.
var capabilitySlugs = map[string][]string{
	"copresent_control": OperatorPrimitiveNames,
	"computer_use":      WorkerCapabilityNames,
}

// ExpandCapabilitySlugs takes a raw tool list from an Agent record
// (possibly containing both concrete tool names like "uiClick" and
// capability slugs like "copresent_control") and returns a
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
