package worker

// pipeline_purpose.go -- the PIPELINE PURPOSE (epic memql#5478, #5494).
//
// ===========================================================================
// A DISPATCH WITH NO AGENT
// ===========================================================================
// A pipeline step that names a need the cluster cannot meet -- docker, a
// display, macOS tooling -- runs on one of the pipeline owner's own machines
// (design D10: the fleet is the routed exception). It travels the road every
// agent call travels: the same router and owner policy, the same
// re-pick-before-side-effects loop, the same cross-replica forward. What it
// cannot travel is the agent's consent gates, because each of them asks a
// question about an AGENT, and there is none:
//
//   - per-task approval asks whether the person approved this agent's plan;
//   - standing scope asks what the person lets this agent do;
//   - the classifier asks whether a command a model composed is dangerous.
//
// component/packages/fleetbuild.go refused exactly this act for a deploy
// rather than mint a plan nobody made to get past those gates. This purpose is
// the answer that refusal waited for: the consent is stated where it belongs,
// by the owner, twice --
//
//  1. as the PIPELINE's owner: the pipeline declares `compute:
//     cluster_and_fleet`, which the seam checks before Execute is called; and
//  2. as the MACHINE's owner: the machine's own policy.yaml allows pipelines,
//     which its cockpit advertises as the routing label pipelines=allowed (and
//     enforces again when the step arrives).
//
// And the owner's off switch, KillSwitchEngaged, still holds. It is the one
// control that means "nothing runs on my machines", and a pipeline step is
// something.
//
// RULE 0: THE ACTION BELONGS TO THE PURPOSE, IN BOTH DIRECTIONS (purposeBinding).
// No other purpose dispatches pipeline_step. On the machine it is an arbitrary
// shell command that the cockpit admits on its pipelines policy WITHOUT a
// consent window, and runs with the machine's own environment: an agent able
// to name it instead of exec -- a prompt-injected one holding full scope, say
// -- would run a command the owner's consent window never saw. And the purpose
// dispatches pipeline_step and nothing else, naming no agent, because every
// exemption below is argued from what pipeline_step is.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	pl "github.com/znasllc-io/memql/component/pipelines"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// PurposePipeline marks a dispatch made for a pipeline run rather than by an
// agent. An empty Request.Purpose is an agent's call, gated exactly as before
// the purpose existed.
const PurposePipeline = "pipeline"

// PipelineStepAction is the workerHost action that runs one pipeline step on a
// machine: clone the repository at a SHA, run the step's command, return its
// exit code and artifacts. The cockpit owns the contract's execution side.
const PipelineStepAction = "pipeline_step"

// PipelinesLabel and PipelinesAllowed are the routing pair a machine whose own
// policy allows pipeline steps advertises. Exact, like every fleet label: a
// requirement is this pair or it matches nothing.
const (
	PipelinesLabel       = "pipelines"
	PipelinesAllowed     = "allowed"
	PipelineStepContract = 2
)

// The refusal codes this purpose introduces.
const (
	// codeDeniedPipelinePurpose refuses pipeline_step under any purpose but
	// the pipeline's (rule 0), and a pipeline-purpose call the purpose does not
	// admit: another action, an agent named, not internal origin, no run or
	// owner, or no pipelines=allowed requirement.
	codeDeniedPipelinePurpose = "denied_pipeline_purpose"
	// codeUnknownPurpose refuses a purpose this engine does not know. Never
	// read as "no purpose": that would hand the agent gates a call that said it
	// was something else.
	codeUnknownPurpose = "unknown_purpose"
	// codePreferencesLookupFailed refuses a pipeline step whose owner's kill
	// switch could not be read.
	codePreferencesLookupFailed = "preferences_lookup_failed"
	// codeBadRequest refuses a pipeline step whose credentials arrive in a
	// shape the masker cannot be sure of (pipelineCredentialShape).
	codeBadRequest = "bad_request"
	// codePipelinesNotAllowed refuses, BEFORE START, a pipeline step for a
	// machine whose live registration does not carry pipelines=allowed on the
	// replica about to dispatch to it (machineAllowsPipelines).
	codePipelinesNotAllowed = "pipelines_not_allowed"
)

// fallbackFor is the fallback the dispatch loop applies when a candidate
// refuses before start (RULING R19).
//
// The owner's routing policy decides it for an AGENT's call: an owner who
// stores `fallback: none` wants a refusal reported rather than routed around.
// A pipeline step always moves on while a candidate remains. It has its own
// two consents, the design is that a machine on a sibling replica is skipped
// and never failed (D10), and a refusal before start ran nothing -- so moving
// on is a re-pick, not a second execution, whoever asked.
func fallbackFor(req Request, policy Policy) string {
	if req.Purpose == PurposePipeline {
		return FallbackNextMatching
	}
	return policy.Fallback
}

// machineAllowsPipelines reads the machine owner's consent where it is the
// MACHINE's own word (RULING R20): the labels its cockpit advertised on the
// connection this replica holds -- never operator labels, which live only on
// the row and which the owner can set on a machine whose policy says no. The
// router matched the ROW, up to a heartbeat old; the replica that dispatches
// re-reads the live registration, so a machine whose policy stopped allowing
// pipelines, or never did, is refused before anything reaches it.
func machineAllowsPipelines(w *workerservice.Worker, args map[string]any) bool {
	if args["execution"] == "native" {
		platform, _ := args["platform"].(string)
		if w == nil || platform == "" || w.CapabilityDescriptor.NativePlatform() != platform {
			return false
		}
	}
	return w != nil && w.LabelsSnapshot()[PipelinesLabel] == PipelinesAllowed &&
		w.CapabilityDescriptor != nil && w.CapabilityDescriptor.ActionContracts.Supports("workerHost."+PipelineStepAction, PipelineStepContract)
}

// purposeBinding is RULE 0, asked before every other gate and on BOTH halves of
// a forward: the sender before it routes (preDispatchCheck), and the receiving
// replica before it dispatches (ForwardHandler), so a forwarded envelope is
// re-decided rather than trusted to have passed. It refuses
//
//   - workerHost.pipeline_step under any purpose but the pipeline's -- an
//     agent's tool loop, the dispatchHost builtin (whose action is an open
//     string), and a forward that does not carry the purpose;
//   - the pipeline purpose on any other action, or naming an agent;
//   - a purpose this engine does not know.
//
// It returns the refusal's code and sentence, or two empty strings when the
// action and the purpose belong together.
func purposeBinding(purpose, tool, action, agentId string) (code, msg string) {
	step := tool == "workerHost" && action == PipelineStepAction
	switch purpose {
	case "":
		if step {
			return codeDeniedPipelinePurpose,
				"workerHost." + PipelineStepAction + " runs only for a pipeline; an agent runs commands with exec"
		}
	case PurposePipeline:
		if !step {
			return codeDeniedPipelinePurpose, fmt.Sprintf(
				"the pipeline purpose dispatches workerHost.%s and nothing else, not %s.%s",
				PipelineStepAction, tool, action)
		}
		if strings.TrimSpace(agentId) != "" {
			// The cockpit refuses a pipeline_step that names an agent, and the
			// record would credit the agent with a call it did not make.
			return codeDeniedPipelinePurpose,
				"a pipeline dispatch names no agent: its consents are the pipeline's and the machine's, not an agent's"
		}
	default:
		return codeUnknownPurpose, fmt.Sprintf("unknown dispatch purpose %q", purpose)
	}
	return "", ""
}

// purposeRefusal is purposeBinding as a gate result, for preDispatchCheck.
func purposeRefusal(req Request) (gateResult, bool) {
	code, msg := purposeBinding(req.Purpose, req.Tool, req.Action, req.AgentId)
	if code == "" {
		return gateResult{}, false
	}
	required := actionRequiredScope(req.Tool, req.Action)
	outcome := "denied_by_policy"
	if code == codeUnknownPurpose {
		// A purpose nothing defines is a caller's bug, not a policy decision
		// about anyone's machine -- recorded like unknown_action.
		outcome = "failure"
	}
	return gateResult{
		deny:               true,
		requiredCapability: required.Capability,
		requiredScope:      required.Scope,
		errorCode:          code,
		errorMessage:       msg,
		outcome:            outcome,
	}, true
}

// pipelineGate is preDispatchCheck for the pipeline purpose, after rule 0 has
// settled WHAT may be dispatched. Its order is who, whose, where, what the step
// carries, and then the owner's off switch.
func (d *Dispatcher) pipelineGate(ctx context.Context, req Request) gateResult {
	required := actionRequiredScope("workerHost", PipelineStepAction)
	refuse := func(msg string) gateResult {
		return gateResult{
			deny:               true,
			requiredCapability: required.Capability,
			requiredScope:      required.Scope,
			errorCode:          codeDeniedPipelinePurpose,
			errorMessage:       msg,
			outcome:            "denied_by_policy",
		}
	}

	// WHO. Only the engine's own pipeline executor dispatches for a pipeline,
	// and only Go the engine runs can stamp internal origin -- a request
	// handler cannot (component/auth/call_origin.go, and the gate that holds
	// it). Without this, the purpose would be a field any caller of the
	// dispatcher could set to switch the agent gates off.
	if !auth.OriginFromContext(ctx).IsInternal() {
		return refuse("the pipeline purpose belongs to the engine's own pipeline executor; a call that did not originate in the engine cannot claim it")
	}

	// WHOSE. The run is what the dispatch belongs to and what its record is
	// filed under; the owner is whose machines are routed over and whose off
	// switch is read. There is no agent, and none is asked for.
	if strings.TrimSpace(req.RunId) == "" || strings.TrimSpace(req.OwnerUserId) == "" {
		return refuse("a pipeline dispatch names the run it belongs to and the person whose machine runs it")
	}

	// WHERE. The machine owner's consent is their machine's own policy, which
	// the cockpit advertises as pipelines=allowed. The requirement is checked
	// HERE rather than trusted to the caller, because its absence is not a
	// narrower request: with no requirement the router would offer every
	// machine the owner has, and the first of them is usually a laptop.
	if req.RequireLabels[PipelinesLabel] != PipelinesAllowed {
		return refuse(fmt.Sprintf("a pipeline dispatch requires %s=%s: a machine runs pipeline steps only when its own policy says so",
			PipelinesLabel, PipelinesAllowed))
	}

	// WHAT IT CARRIES. The record and the result are masked for every
	// credential the step carries, so the credentials must be in a shape whose
	// every value is known; anything else is refused rather than dispatched
	// with values a mask might miss. Checked before the off switch is read: a
	// malformed request costs no database read.
	if err := pipelineCredentialShape(req.Args); err != nil {
		return gateResult{
			deny:               true,
			requiredCapability: required.Capability,
			requiredScope:      required.Scope,
			errorCode:          codeBadRequest,
			errorMessage:       err.Error(),
			outcome:            "failure",
		}
	}

	// THE OFF SWITCH. Unlike the agent path, an UNREADABLE switch refuses. The
	// agent path reads on to the agent's authorization, which fails closed
	// when the database does; this path makes no second read, so a failed read
	// here would be the one moment the owner's off switch was not consulted.
	if d.store != nil {
		prefs, err := d.store.UserPreferences(ctx, req.OwnerUserId)
		if err != nil {
			d.logger.Warn("pipeline dispatch: the owner's kill switch could not be read; refusing",
				"owner_user_id", req.OwnerUserId, "run_id", req.RunId, "error", err)
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          codePreferencesLookupFailed,
				errorMessage:       "the owner's computer-use setting could not be read, so no pipeline step runs on their machines: " + err.Error(),
				outcome:            "failure",
			}
		}
		if prefs.KillSwitchEngaged {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "kill_switch_engaged",
				errorMessage:       "computer use is currently disabled by the user",
				outcome:            "kill_switch_engaged",
			}
		}
	}

	// NOT CONSULTED, BY DESIGN.
	//
	// AgentAuthorization: there is no agent. Standing scope is what a person
	// lets an agent do, and reading it here would either refuse every pipeline
	// (no agent, no scope) or invent an agent to hold one.
	//
	// The classifier: it judges commands a MODEL composed, and nothing here
	// did. The command is the repository's own manifest at a pinned SHA; a fork
	// pull request is refused upstream before a step exists; and both owner
	// consents above were given for exactly this. Classifying a test suite
	// would refuse ordinary build steps on their resemblance to dangerous ones
	// -- and its rules have no opinion that either owner asked for.
	return gateResult{requiredCapability: required.Capability, requiredScope: required.Scope}
}

// auditActor names who a denial is attributed to: the agent, or -- when there
// is none -- the pipeline run.
func auditActor(req Request) string {
	if req.Purpose == PurposePipeline {
		return "pipeline:" + req.RunId
	}
	return "agent:" + req.AgentId
}

// -- a pipeline step's credentials ------------------------------------------
//
// A pipeline_step's arguments carry two kinds of credential the machine
// needs and the record must never hold: the clone token and the resolved
// secrets. redactArgs already replaces both by KEY. They are also masked by
// VALUE, wherever they appear, because the key is not the only place a value
// can be: a token embedded in a clone URL, or a secret a command echoes into
// the output preview, is the same credential under a key nothing scrubs. The
// masking is the seam's (pl.MaskSecrets), as the step capture's is, so what
// leaves here is what the capture and the check run would make of it.

// pipelineCredentialShape refuses credentials the gate cannot be sure it has
// read in full: a token that is not a string, or secrets that are not a map of
// names to string values. The error names the offending shape and the secret's
// NAME, never a value -- the value may be the very thing being protected.
func pipelineCredentialShape(args map[string]any) error {
	switch token := args["token"].(type) {
	case nil, string:
	default:
		return fmt.Errorf("pipeline_step token must be a string, not %T", token)
	}
	switch secrets := args["secrets"].(type) {
	case nil, map[string]string:
	case map[string]any:
		for name, v := range secrets {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("pipeline_step secret %q must be a string, not %T", name, v)
			}
		}
	default:
		return fmt.Errorf("pipeline_step secrets must be a map of names to string values, not %T", secrets)
	}
	return nil
}

// pipelineCredentialMasker masks every credential a pipeline_step's arguments
// carry, or is nil when the call is not a pipeline_step or carries no value
// long enough to mask.
//
// It reads every string under the token and the secrets, WHATEVER their shape:
// the gate refuses a shape it cannot read (pipelineCredentialShape), but the
// refused call is still recorded, and its record must be masked too. It masks
// as the seam does (pl.MaskSecrets over pl.MaskForms): a value as stored,
// without the whitespace around it, and each line of it -- at a newline or a
// carriage return -- without its own, four bytes or more, so a line blank once
// trimmed, an indent inside a key, never masks every run of spaces in the
// output, and an indented line still masks when printed with other
// indentation; and values that overlap where printed are one span, with no
// remnant of either beside the mask.
func pipelineCredentialMasker(req Request) func(string) string {
	if req.Tool != "workerHost" || req.Action != PipelineStepAction {
		return nil
	}
	var values []string
	add := func(v string) { values = append(values, v) }
	eachString(req.Args["token"], add)
	eachString(req.Args["secrets"], add)
	forms := pl.MaskForms(values)
	if len(forms) == 0 {
		return nil
	}
	return func(s string) string { return pl.MaskSecrets(s, forms) }
}

// eachString hands every string inside v to fn, whatever the nesting.
func eachString(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, x := range t {
			eachString(x, fn)
		}
	case map[string]string:
		for _, x := range t {
			fn(x)
		}
	case []any:
		for _, x := range t {
			eachString(x, fn)
		}
	case []string:
		for _, x := range t {
			fn(x)
		}
	}
}

// maskPipelineResult masks a pipeline_step's credentials out of the text a
// result carries back to its caller: the machine's output preview and error
// message, which a failed clone or an echoing command can fill with them. The
// record masks its own copy (recordInvocation), so neither depends on the
// other having run.
func maskPipelineResult(req Request, res Result) Result {
	if mask := pipelineCredentialMasker(req); mask != nil {
		res.OutputPreview = mask(res.OutputPreview)
		res.ErrorMessage = mask(res.ErrorMessage)
	}
	return res
}

// maskCredentials returns v with every string inside it masked. Maps and
// slices are copied, never edited in place.
func maskCredentials(v any, mask func(string) string) any {
	switch t := v.(type) {
	case string:
		return mask(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = maskCredentials(x, mask)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, x := range t {
			out[k] = mask(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = maskCredentials(x, mask)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, x := range t {
			out[i] = mask(x)
		}
		return out
	}
	return v
}

// These v2 refusals occur before checkout/command execution. No transport
// failure, timeout, uncertain cleanup or ordinary command failure can enter
// this list: those might already have performed external effects.
func pipelineRefusedBeforeStart(purpose string, result Result, dispatchErr error) bool {
	if purpose != PurposePipeline || dispatchErr != nil || result.OK {
		return false
	}
	switch result.ErrorCode {
	case "pipeline_capacity_busy", "pipeline_capacity_unavailable", "pipeline_runtime_unavailable", "denied_by_policy":
		return true
	default:
		return false
	}
}
