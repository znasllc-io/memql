package safety

// Surface names the execution path a proposed action would take.
// Surface determines blast radius (computer-use surfaces drive the
// user's real machine; workbench is sandboxed per-Plan; tool surfaces
// are engine-internal) which the decision policy uses to vary
// strictness.
type Surface string

const (
	SurfaceComputerUseHeadless Surface = "computer_use_headless"
	SurfaceComputerUseEmbodied Surface = "computer_use_embodied"
	SurfaceWorkbench           Surface = "workbench"
	SurfaceToolWebhook         Surface = "tool_webhook"
	SurfaceToolIntegration     Surface = "tool_integration"
)

// Action names the abstract operation the surface would perform.
// Kept small + surface-agnostic so the classifier and decision
// policy don't have to switch on per-surface vocabularies.
type Action string

const (
	ActionExec       Action = "exec"
	ActionFSRead     Action = "fs_read"
	ActionFSWrite    Action = "fs_write"
	ActionFSList     Action = "fs_list"
	ActionFSStat     Action = "fs_stat"
	ActionHTTPFetch  Action = "http_fetch"
	ActionScreenshot Action = "screenshot"
	ActionWebhook    Action = "webhook"
	ActionToolCall   Action = "tool_call"
)

// CallerContext carries the identity and lineage attached to a
// proposed action. Populated by the per-surface adapter (#229).
// Plain value type -- safe to pass and log (no pointers, no secrets;
// all secrets live inside the Payload and get redacted before
// recording).
type CallerContext struct {
	AgentID       string
	OwnerUserID   string
	PlanID        string
	TaskID        string
	CorrelationID string
	// Scope is the effective scope already resolved by the existing
	// scope/kill-switch gate (observe / interact / full / ""). The
	// classifier doesn't re-derive it; the decision policy reads it.
	Scope string
	// Capability names the agent-side capability that already
	// authorized this surface (e.g. workerHost / workerComputer /
	// workbench_use). Empty for surfaces that don't gate by
	// capability.
	Capability string
}

// Payload holds the action-specific data the classifier inspects.
// Every field is optional; only the ones the Action actually needs
// are populated. Keeping it a single struct (rather than a oneof per
// action) lets the rule engine + LLM classifier consume it uniformly.
type Payload struct {
	// Command is the raw shell command string for ActionExec.
	Command string
	// Paths are filesystem path(s) the action would read/write/list.
	// One entry for single-path actions; multiple for batch shapes.
	Paths []string
	// URL is the target for ActionHTTPFetch / ActionWebhook.
	URL string
	// Method is the HTTP verb for ActionHTTPFetch / ActionWebhook
	// (e.g. "GET", "POST"). Empty when unknown / not applicable.
	Method string
	// Body is the request body (may contain secrets -- redacted
	// before recording). Kept as a string to stay surface-agnostic;
	// JSON callers stringify before constructing the descriptor.
	Body string
	// ToolName names the AI/integration tool being called for
	// ActionToolCall / ActionWebhook -- used by the rule engine to
	// scope tool-specific allow/deny rules.
	ToolName string
	// Args is a free-form map for fields that don't fit the typed
	// slots above (GUI coordinates, screenshot region, etc.).
	// Treated as untrusted, redacted before recording.
	Args map[string]any
}

// ActionDescriptor is the normalized, surface-agnostic description of
// a proposed action. Every execution surface lowers its native
// request shape (worker `Request`, workbench args, tool args) into
// this struct, and the Gate + classifier + decision policy operate
// only on it.
type ActionDescriptor struct {
	Surface Surface
	Action  Action
	Payload Payload
	Caller  CallerContext
}

// NewExecAction builds a descriptor for a shell-exec action.
// Surface picks the right blast-radius class (workbench vs
// computer_use_headless). Used directly by per-surface adapters
// (#229) and by tests.
func NewExecAction(surface Surface, command string, caller CallerContext) ActionDescriptor {
	return ActionDescriptor{
		Surface: surface,
		Action:  ActionExec,
		Payload: Payload{Command: command},
		Caller:  caller,
	}
}

// NewHTTPAction builds a descriptor for an http_fetch action.
func NewHTTPAction(surface Surface, method, url, body string, caller CallerContext) ActionDescriptor {
	return ActionDescriptor{
		Surface: surface,
		Action:  ActionHTTPFetch,
		Payload: Payload{Method: method, URL: url, Body: body},
		Caller:  caller,
	}
}

// NewFSAction builds a descriptor for a filesystem action
// (fs_read / fs_write / fs_list / fs_stat).
func NewFSAction(surface Surface, action Action, paths []string, caller CallerContext) ActionDescriptor {
	return ActionDescriptor{
		Surface: surface,
		Action:  action,
		Payload: Payload{Paths: append([]string(nil), paths...)},
		Caller:  caller,
	}
}

// NewToolAction builds a descriptor for a generic AI/integration
// tool call (webhook or integration capability). The free-form `args`
// is treated as untrusted and redacted before any recording.
func NewToolAction(surface Surface, toolName string, args map[string]any, caller CallerContext) ActionDescriptor {
	return ActionDescriptor{
		Surface: surface,
		Action:  ActionToolCall,
		Payload: Payload{ToolName: toolName, Args: copyArgs(args)},
		Caller:  caller,
	}
}

// copyArgs makes a shallow copy so descriptor consumers can't mutate
// the caller's map. Nested maps are NOT deep-copied -- callers that
// need that guarantee should pass an already-copied map.
func copyArgs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
