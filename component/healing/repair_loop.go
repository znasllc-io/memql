// repair_loop.go implements the LLM repair loop for self-healing (Epic 4 /
// memql#2142, E4.4).
//
// On a precondition-miss (E4.1 -- the healing.precondition.missed signal),
// the repair loop asks an LLM to PROPOSE one or more TYPED patches (E4.3)
// that would heal the construct so the precondition holds. The proposals
// are NOT auto-applied: they are validated, deploy-spine constructs are
// refused, and the caller (E4.5 human validation) decides what becomes a
// live overlay override. The deploy spine stays authored/deterministic --
// it is guarded by preconditions but never LLM-healed.
//
// Built on the actions substrate (#1734): the loop accepts an optional
// RemediationFeeder (backed by searchActions) that surfaces prior
// remediations for similar misses as additional grounding for the model --
// the "learned/remediation feeder" the meta-epic calls for. The feeder is
// optional and injected, so the loop is testable with a STUB model and no
// DB.
//
// Determinism + testability: the loop depends only on a
// common.ChatStructuredProvider (the engine's structured-output surface)
// and the injected feeder. A test injects a stub provider returning canned
// typed-patch JSON, so the whole loop runs deterministically with no real
// model and no engine. The structured-output schema constrains the model to
// the four patch kinds; every returned patch is re-validated with
// Patch.Validate() (the model is never trusted to be well-formed) and, when
// a base construct is supplied, dry-run Apply'd so an unapplyable proposal
// is dropped before a human ever sees it.

package healing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// PreconditionMiss is the structured precondition-miss signal the repair
// loop consumes. It mirrors the healing.precondition.missed event payload
// (component/automations: emitPreconditionMiss) -- the fields the loop needs
// to ground a repair proposal: which construct + precondition missed, the
// deterministic check that failed, the machine-specific literal asserted,
// and the triggering event payload (the concrete value that did not satisfy
// the check on this machine).
type PreconditionMiss struct {
	AutomationName  string         `json:"automationName"`
	BaseConstructId string         `json:"baseConstructId"`
	PreconditionId  string         `json:"preconditionId"`
	Check           string         `json:"check"`
	Literal         string         `json:"literal,omitempty"`
	Description     string         `json:"description,omitempty"`
	TriggerPayload  map[string]any `json:"triggerPayload,omitempty"`
}

// MissFromEventPayload builds a PreconditionMiss from a
// healing.precondition.missed event payload map (the shape
// emitPreconditionMiss publishes). The baseConstructId defaults to the
// automation name when the event does not carry an explicit one -- an
// automation-level precondition heals the automation construct.
func MissFromEventPayload(payload map[string]any) PreconditionMiss {
	m := PreconditionMiss{
		AutomationName:  stringField(payload, "automationName"),
		BaseConstructId: stringField(payload, "baseConstructId"),
		PreconditionId:  stringField(payload, "preconditionId"),
		Check:           stringField(payload, "check"),
		Literal:         stringField(payload, "literal"),
		Description:     stringField(payload, "preconditionDescription"),
	}
	if m.BaseConstructId == "" {
		m.BaseConstructId = m.AutomationName
	}
	if tp, ok := payload["triggerPayload"].(map[string]any); ok {
		m.TriggerPayload = tp
	}
	return m
}

// Remediation is a prior healing the feeder surfaces as grounding for a new
// proposal -- a learned remediation from the actions substrate.
type Remediation struct {
	// Intent is the natural-language description of the prior remediation.
	Intent string
	// Summary is an optional short note (e.g. the patch kind that worked).
	Summary string
}

// RemediationFeeder surfaces prior remediations relevant to a miss (built on
// the actions substrate -- searchActions over remediation intents). Optional:
// a nil feeder simply omits the grounding. Injected so the loop is testable
// without a DB.
type RemediationFeeder interface {
	Remediations(ctx context.Context, miss PreconditionMiss) ([]Remediation, error)
}

// DeploySpineGuard reports whether a baseConstructId belongs to the
// authored/deterministic deploy spine, which is NEVER LLM-healed. The repair
// loop refuses to propose for a spine construct. Injected so the spine set is
// defined by the caller (the deploy pack) rather than hardcoded here.
type DeploySpineGuard func(baseConstructId string) bool

// RepairLoop proposes typed patches for a precondition-miss via an LLM.
// Construct once with the structured-output provider + optional feeder +
// optional spine guard, then call Propose per miss.
type RepairLoop struct {
	provider common.ChatStructuredProvider
	feeder   RemediationFeeder
	isSpine  DeploySpineGuard
	// maxPatches caps how many proposals the loop returns (defense against a
	// runaway model). Zero => default of 4 (one per patch kind).
	maxPatches int
}

// RepairLoopOption configures a RepairLoop.
type RepairLoopOption func(*RepairLoop)

// WithRemediationFeeder wires the actions-substrate remediation feeder.
func WithRemediationFeeder(f RemediationFeeder) RepairLoopOption {
	return func(r *RepairLoop) { r.feeder = f }
}

// WithDeploySpineGuard wires the deploy-spine guard so the loop refuses to
// propose for an authored/deterministic spine construct.
func WithDeploySpineGuard(g DeploySpineGuard) RepairLoopOption {
	return func(r *RepairLoop) { r.isSpine = g }
}

// WithMaxPatches caps the number of returned proposals.
func WithMaxPatches(n int) RepairLoopOption {
	return func(r *RepairLoop) { r.maxPatches = n }
}

// NewRepairLoop constructs a repair loop over the given structured-output
// provider (the engine's ChatStructuredProvider). The provider is required;
// a test injects a stub.
func NewRepairLoop(provider common.ChatStructuredProvider, opts ...RepairLoopOption) *RepairLoop {
	r := &RepairLoop{provider: provider, maxPatches: 4}
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	return r
}

// Propose runs the repair loop for a precondition-miss and returns the
// validated typed-patch proposals. `base` is the base construct (the
// automation/precondition JSON the resolver returns) used to DRY-RUN each
// proposed patch; pass nil to skip the apply check (validation still runs).
//
// Contract:
//   - The deploy spine is refused: if isSpine(miss.BaseConstructId) is true,
//     Propose returns (nil, nil) -- no proposal, the spine stays
//     deterministic.
//   - The model output is never trusted: every returned patch is
//     Patch.Validate()'d, and (when base != nil) dry-run Apply'd; a patch
//     that fails either is DROPPED. A model that returns only bad patches
//     yields an empty proposal set, not an error.
//   - Proposals are NOT applied -- they are returned for human validation
//     (E4.5). The repair loop has no write side effect.
func (r *RepairLoop) Propose(ctx context.Context, miss PreconditionMiss, base map[string]any) ([]Patch, error) {
	if r == nil || r.provider == nil {
		return nil, fmt.Errorf("healing: repair loop has no structured-output provider")
	}
	// Deploy-spine constructs are never LLM-healed.
	if r.isSpine != nil && r.isSpine(miss.BaseConstructId) {
		return nil, nil
	}

	var remediations []Remediation
	if r.feeder != nil {
		// A feeder error is non-fatal -- grounding is best-effort; the loop
		// still proposes without it.
		remediations, _ = r.feeder.Remediations(ctx, miss)
	}

	messages := buildRepairMessages(miss, remediations)
	raw, err := r.provider.CallChatStructured(ctx, messages, patchProposalSchema())
	if err != nil {
		return nil, fmt.Errorf("healing: repair loop model call: %w", err)
	}

	proposed, err := parsePatchProposals(raw)
	if err != nil {
		return nil, fmt.Errorf("healing: parse repair proposals: %w", err)
	}

	out := make([]Patch, 0, len(proposed))
	for i := range proposed {
		p := proposed[i]
		if err := p.Validate(); err != nil {
			// The model is never trusted to be well-formed; drop a bad patch.
			continue
		}
		if base != nil {
			if _, err := p.Apply(base); err != nil {
				// An unapplyable proposal never reaches a human.
				continue
			}
		}
		out = append(out, p)
		if r.maxPatches > 0 && len(out) >= r.maxPatches {
			break
		}
	}
	return out, nil
}

// patchProposal is the model's structured output: a list of typed patches.
// The schema constrains the model to the four patch kinds; parsePatchProposals
// re-validates each before it is trusted.
type patchProposal struct {
	Patches []Patch `json:"patches"`
}

// parsePatchProposals unmarshals the model's structured output into patches.
func parsePatchProposals(raw string) ([]Patch, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var pp patchProposal
	if err := json.Unmarshal([]byte(raw), &pp); err != nil {
		return nil, fmt.Errorf("invalid patch-proposal JSON: %w", err)
	}
	return pp.Patches, nil
}

// patchProposalSchema is the JSON schema the structured-output call enforces.
// Top-level object with a `patches` array (OpenAI json_schema mode requires a
// top-level object, not a bare array). Each patch enumerates the four kinds;
// per-kind required fields are enforced post-hoc by Patch.Validate() since
// JSON schema oneOf is awkward across providers.
func patchProposalSchema() common.StructuredSchema {
	return common.StructuredSchema{
		Name:        "healingPatchProposals",
		Description: "Typed patches that would heal the construct so the missed precondition holds.",
		Strict:      false,
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "patches": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["add-precondition", "insert-guard", "relativize-literal", "rebind-param"]},
          "target": {"type": "string"},
          "guard": {"type": "string"},
          "literal": {"type": "string"},
          "replacement": {"type": "string"},
          "reason": {"type": "string"},
          "precondition": {
            "type": "object",
            "properties": {
              "id": {"type": "string"},
              "check": {"type": "string"},
              "literal": {"type": "string"},
              "description": {"type": "string"}
            }
          }
        },
        "required": ["kind"]
      }
    }
  },
  "required": ["patches"]
}`),
	}
}

// buildRepairMessages constructs the system + user messages grounding the
// model in the miss + any prior remediations. The system message pins the
// task + the typed-patch vocabulary; the user message carries the concrete
// miss. Deterministic given its inputs (no clocks / randomness) so a stub
// provider can assert on the exact messages.
func buildRepairMessages(miss PreconditionMiss, remediations []Remediation) []common.ChatMessage {
	var sys strings.Builder
	sys.WriteString("You are a self-healing repair planner for a deterministic automation harness. ")
	sys.WriteString("A precondition (a deterministic boolean check on an automation) MISSED: it evaluated false, ")
	sys.WriteString("which means a literal that holds on the authoring machine does not hold here. ")
	sys.WriteString("Propose one or more TYPED patches that would heal the construct so the precondition holds, ")
	sys.WriteString("preferring the smallest change. The ONLY patch kinds are: ")
	sys.WriteString("add-precondition (append a deterministic guard), ")
	sys.WriteString("insert-guard (gate a step with a boolean condition), ")
	sys.WriteString("relativize-literal (replace a machine-specific literal with a relative reference like $config.X or $event.payload.X), ")
	sys.WriteString("rebind-param (rebind a parameter reference to the correct source). ")
	sys.WriteString("relativize-literal is the portability heal and is usually the right choice when a precondition asserts a machine-specific literal. ")
	sys.WriteString("Do not invent other kinds. Return only the patches.")

	var usr strings.Builder
	usr.WriteString("Precondition miss:\n")
	fmt.Fprintf(&usr, "- construct: %s\n", miss.BaseConstructId)
	fmt.Fprintf(&usr, "- precondition id: %s\n", miss.PreconditionId)
	fmt.Fprintf(&usr, "- failed check: %s\n", miss.Check)
	if miss.Literal != "" {
		fmt.Fprintf(&usr, "- asserted literal (the machine-specific value that did not hold): %s\n", miss.Literal)
	}
	if miss.Description != "" {
		fmt.Fprintf(&usr, "- description: %s\n", miss.Description)
	}
	if len(miss.TriggerPayload) > 0 {
		if b, err := json.Marshal(miss.TriggerPayload); err == nil {
			fmt.Fprintf(&usr, "- triggering event payload: %s\n", string(b))
		}
	}
	if len(remediations) > 0 {
		usr.WriteString("\nPrior remediations for similar misses (grounding, may not apply):\n")
		for _, rem := range remediations {
			if rem.Summary != "" {
				fmt.Fprintf(&usr, "- %s (%s)\n", rem.Intent, rem.Summary)
			} else {
				fmt.Fprintf(&usr, "- %s\n", rem.Intent)
			}
		}
	}

	return []common.ChatMessage{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: usr.String()},
	}
}
