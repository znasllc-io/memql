// patch.go implements the typed-patch model for self-healing (Epic 4 /
// memql#2141, E4.3).
//
// A Patch is a TYPED transform that takes a BASE construct (the automation
// or precondition definition, as a JSON object) and produces the healed
// OVERLAY overrideData (E4.2): the construct definition the two-tier
// resolver returns when the override wins. There are exactly four patch
// kinds -- the locked vocabulary of how the repair loop (E4.4) heals a
// construct:
//
//   - add-precondition : append a deterministic guard (a Precondition) the
//     construct was missing. Heals "this needed a guard that did not exist."
//   - insert-guard     : set / AND a boolean condition onto a step (or a
//     precondition's check). Heals "a step ran when it should not have."
//   - relativize-literal: replace a machine-specific literal (a path, an id,
//     an endpoint) with a relative reference ($config.X / $event.payload.X).
//     The PORTABILITY heal: the literal that does not hold on this machine is
//     made relative so the construct travels. This is the literal-that-does-
//     not-hold-elsewhere lever the whole epic turns on.
//   - rebind-param     : rebind a param / arg reference from one source to
//     another ($steps.a.result -> $steps.b.result, or a renamed field).
//     Heals "the value came from the wrong place."
//
// A patch is Apply'd to a deep COPY of the base (the base is immutable -- it
// is never LLM-healed), producing overrideData; Validate checks the patch is
// well-formed; and the Apply result is checked to still LOAD as a structurally
// valid construct (validateConstructLoads). An invalid patch, or a patch that
// produces an unloadable construct, is rejected -- a heal can never blank or
// corrupt a construct.
//
// The model is pure data: no engine, no LLM, no DB. The repair loop (E4.4)
// produces Patch values (via a stub model in tests); this package applies +
// validates them deterministically.

package healing

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PatchKind is the (closed) set of typed-patch kinds.
type PatchKind string

const (
	// PatchAddPrecondition appends a Precondition to the construct's
	// preconditions[].
	PatchAddPrecondition PatchKind = "add-precondition"
	// PatchInsertGuard sets / ANDs a boolean condition onto a target step
	// (or a precondition check).
	PatchInsertGuard PatchKind = "insert-guard"
	// PatchRelativizeLiteral replaces a machine-specific literal at Target
	// with a relative reference.
	PatchRelativizeLiteral PatchKind = "relativize-literal"
	// PatchRebindParam rebinds a param / arg reference at Target.
	PatchRebindParam PatchKind = "rebind-param"
)

// Patch is one typed transform from a base construct to a healed overlay
// override. The kind selects which fields are load-bearing; Validate enforces
// the per-kind requirements.
type Patch struct {
	// Kind is the typed-patch kind. Required; must be one of the four.
	Kind PatchKind `json:"kind"`

	// Target is a dot-path into the construct identifying WHERE the patch
	// applies. Required for every kind except add-precondition (which always
	// appends to preconditions[]). Examples:
	//   insert-guard:        "steps.run"            (the step to guard)
	//   relativize-literal:  "steps.run.input.path" (the literal to relativize)
	//   rebind-param:        "steps.run.input.from" (the ref to rebind)
	Target string `json:"target,omitempty"`

	// Precondition is the guard to append (add-precondition only).
	Precondition *PatchPrecondition `json:"precondition,omitempty"`

	// Guard is the boolean condition to set/AND onto the target
	// (insert-guard only).
	Guard string `json:"guard,omitempty"`

	// Literal is the machine-specific literal being relativized
	// (relativize-literal only) -- the value expected at Target. Optional;
	// when set it is a safety check (the patch only relativizes if the
	// current value matches), otherwise the value at Target is relativized
	// unconditionally.
	Literal string `json:"literal,omitempty"`

	// Replacement is the relative reference that replaces the literal
	// (relativize-literal) or the new binding (rebind-param). Required for
	// both. Examples: "$config.MEMQL_ENGINE_DIGEST", "$event.payload.path",
	// "$steps.fetch.result.id".
	Replacement string `json:"replacement,omitempty"`

	// Reason is human-readable context carried into the healed override's
	// `reason` field for audit + the cockpit (E4.6). Optional.
	Reason string `json:"reason,omitempty"`
}

// PatchPrecondition is the precondition payload an add-precondition patch
// appends. Mirrors component/automations.Precondition (id/check/literal/
// description) without importing the automations package (which would couple
// the pure patch model to the harness). Marshalled into the construct's
// preconditions[] verbatim.
type PatchPrecondition struct {
	ID          string `json:"id"`
	Check       string `json:"check"`
	Literal     string `json:"literal,omitempty"`
	Description string `json:"description,omitempty"`
}

// Validate reports whether the patch is well-formed: the kind is one of the
// four, and the per-kind required fields are present. A well-formed patch is
// not guaranteed to APPLY (the target may not exist) -- that surfaces from
// Apply -- but a malformed patch is rejected up front.
func (p *Patch) Validate() error {
	if p == nil {
		return fmt.Errorf("patch: nil")
	}
	switch p.Kind {
	case PatchAddPrecondition:
		if p.Precondition == nil {
			return fmt.Errorf("patch %s: precondition is required", p.Kind)
		}
		if strings.TrimSpace(p.Precondition.ID) == "" {
			return fmt.Errorf("patch %s: precondition.id is required", p.Kind)
		}
		if strings.TrimSpace(p.Precondition.Check) == "" {
			return fmt.Errorf("patch %s: precondition.check is required", p.Kind)
		}
	case PatchInsertGuard:
		if strings.TrimSpace(p.Target) == "" {
			return fmt.Errorf("patch %s: target is required", p.Kind)
		}
		if strings.TrimSpace(p.Guard) == "" {
			return fmt.Errorf("patch %s: guard expression is required", p.Kind)
		}
	case PatchRelativizeLiteral:
		if strings.TrimSpace(p.Target) == "" {
			return fmt.Errorf("patch %s: target is required", p.Kind)
		}
		if strings.TrimSpace(p.Replacement) == "" {
			return fmt.Errorf("patch %s: replacement (the relative reference) is required", p.Kind)
		}
	case PatchRebindParam:
		if strings.TrimSpace(p.Target) == "" {
			return fmt.Errorf("patch %s: target is required", p.Kind)
		}
		if strings.TrimSpace(p.Replacement) == "" {
			return fmt.Errorf("patch %s: replacement (the new binding) is required", p.Kind)
		}
	default:
		return fmt.Errorf("patch: unknown kind %q (want one of add-precondition, insert-guard, relativize-literal, rebind-param)", p.Kind)
	}
	return nil
}

// Apply transforms the base construct into the healed overlay overrideData.
// It NEVER mutates base -- it deep-copies first, so the immutable base tier
// is untouched. The returned map is the overrideData the proposeOverride
// mutation (E4.2) stores and the resolver returns when the override wins.
//
// Apply validates the patch, applies the kind-specific transform to the copy,
// and then checks the result still LOADS as a structurally valid construct
// (validateConstructLoads). A patch whose target is absent, or that produces
// an unloadable construct, is rejected.
func (p *Patch) Apply(base map[string]any) (map[string]any, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf("patch %s: base construct is nil", p.Kind)
	}

	out, err := deepCopyMap(base)
	if err != nil {
		return nil, fmt.Errorf("patch %s: deep-copy base: %w", p.Kind, err)
	}

	switch p.Kind {
	case PatchAddPrecondition:
		err = p.applyAddPrecondition(out)
	case PatchInsertGuard:
		err = p.applyInsertGuard(out)
	case PatchRelativizeLiteral:
		err = p.applyRelativizeLiteral(out)
	case PatchRebindParam:
		err = p.applyRebindParam(out)
	}
	if err != nil {
		return nil, err
	}

	if err := validateConstructLoads(out); err != nil {
		return nil, fmt.Errorf("patch %s: result does not load as a valid construct: %w", p.Kind, err)
	}
	return out, nil
}

// applyAddPrecondition appends the patch's precondition to the construct's
// preconditions[]. A duplicate id (a precondition with the same id already
// present) is rejected -- a heal must not shadow an existing guard.
func (p *Patch) applyAddPrecondition(out map[string]any) error {
	existing, _ := out["preconditions"].([]any)
	for _, e := range existing {
		if m, ok := e.(map[string]any); ok {
			if strings.EqualFold(stringField(m, "id"), p.Precondition.ID) {
				return fmt.Errorf("patch %s: a precondition with id %q already exists", p.Kind, p.Precondition.ID)
			}
		}
	}
	pc := map[string]any{
		"id":    p.Precondition.ID,
		"check": p.Precondition.Check,
	}
	if p.Precondition.Literal != "" {
		pc["literal"] = p.Precondition.Literal
	}
	if p.Precondition.Description != "" {
		pc["description"] = p.Precondition.Description
	}
	out["preconditions"] = append(existing, pc)
	return nil
}

// applyInsertGuard sets / ANDs the guard expression onto the target. The
// target resolves to a node in the construct; the guard is stored under its
// "condition" key (the Step.Condition field) -- if a condition already
// exists, the new guard is AND-composed so the heal STRENGTHENS rather than
// replaces the existing gate.
func (p *Patch) applyInsertGuard(out map[string]any) error {
	node, err := resolveTargetMap(out, p.Target)
	if err != nil {
		return fmt.Errorf("patch %s: %w", p.Kind, err)
	}
	if existing := strings.TrimSpace(stringField(node, "condition")); existing != "" {
		if existing == p.Guard {
			return nil // idempotent
		}
		node["condition"] = "(" + existing + ") && (" + p.Guard + ")"
	} else {
		node["condition"] = p.Guard
	}
	return nil
}

// applyRelativizeLiteral replaces the literal value at Target with the
// relative Replacement reference. When Literal is set it is a safety check:
// the relativization only proceeds if the current value equals the named
// literal (so the patch cannot silently rewrite an already-changed value).
func (p *Patch) applyRelativizeLiteral(out map[string]any) error {
	parent, key, err := resolveTargetField(out, p.Target)
	if err != nil {
		return fmt.Errorf("patch %s: %w", p.Kind, err)
	}
	if p.Literal != "" {
		if cur := stringField(parent, key); cur != p.Literal {
			return fmt.Errorf("patch %s: literal mismatch at %q -- expected %q, found %q (the construct changed under the patch)", p.Kind, p.Target, p.Literal, cur)
		}
	}
	parent[key] = p.Replacement
	return nil
}

// applyRebindParam rebinds the reference at Target to the Replacement binding.
// Structurally identical to relativize-literal (it overwrites a leaf value)
// but kept a distinct kind: relativize-literal is the portability heal (a
// machine-specific constant -> a relative ref), while rebind-param is the
// data-flow heal (a wrong source ref -> the right one). Keeping them distinct
// preserves the typed vocabulary the repair loop reasons in and the audit
// trail reads.
func (p *Patch) applyRebindParam(out map[string]any) error {
	parent, key, err := resolveTargetField(out, p.Target)
	if err != nil {
		return fmt.Errorf("patch %s: %w", p.Kind, err)
	}
	parent[key] = p.Replacement
	return nil
}

// --- construct navigation -------------------------------------------------

// resolveTargetMap walks a dot-path to a map node within the construct,
// descending into nested maps and into the steps[] array by step id (a path
// segment "steps.run" selects the step whose id is "run"). Returns the map at
// the path or an error if any segment is absent / not a map.
func resolveTargetMap(root map[string]any, target string) (map[string]any, error) {
	cur := any(root)
	segs := strings.Split(target, ".")
	for i, seg := range segs {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("target %q: segment %q is not a map", target, seg)
		}
		// Special-case stepwise descent: "steps.<id>" selects the array
		// element whose "id" matches.
		if seg == "steps" && i+1 < len(segs) {
			step, ok := findStepById(m["steps"], segs[i+1])
			if !ok {
				return nil, fmt.Errorf("target %q: no step with id %q", target, segs[i+1])
			}
			cur = step
			// consume the id segment too
			if i+1 == len(segs)-1 {
				return step, nil
			}
			// continue from after the id segment
			return resolveTargetMap(step, strings.Join(segs[i+2:], "."))
		}
		next, present := m[seg]
		if !present {
			return nil, fmt.Errorf("target %q: segment %q absent", target, seg)
		}
		cur = next
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("target %q does not resolve to a map", target)
	}
	return m, nil
}

// resolveTargetField walks a dot-path to a LEAF field: it resolves the parent
// map and returns it plus the final key, so the caller can overwrite the leaf.
// The leaf must already exist (a heal rewrites a value, it does not invent a
// field).
func resolveTargetField(root map[string]any, target string) (map[string]any, string, error) {
	idx := strings.LastIndex(target, ".")
	if idx < 0 {
		// Leaf at the root.
		if _, ok := root[target]; !ok {
			return nil, "", fmt.Errorf("target %q: field absent", target)
		}
		return root, target, nil
	}
	parentPath := target[:idx]
	key := target[idx+1:]
	parent, err := resolveTargetMap(root, parentPath)
	if err != nil {
		return nil, "", err
	}
	if _, ok := parent[key]; !ok {
		return nil, "", fmt.Errorf("target %q: field %q absent", target, key)
	}
	return parent, key, nil
}

// findStepById returns the step map in a steps[] value whose "id" matches.
func findStepById(steps any, id string) (map[string]any, bool) {
	arr, ok := steps.([]any)
	if !ok {
		return nil, false
	}
	for _, s := range arr {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(stringField(m, "id"), id) {
			return m, true
		}
	}
	return nil, false
}

// --- result-loads validation ----------------------------------------------

// validateConstructLoads checks the patched construct is structurally valid
// enough to load: every precondition has a non-empty id + check with no
// duplicate ids, and (when present) every step is a map with an id. This is
// the "result still loads" gate -- it catches a patch that blanks a required
// field or introduces a duplicate guard, without taking a heavy dependency on
// the full automations compiler (which would couple the pure patch model to
// the harness and risk an import cycle). The harness's own loader re-validates
// on materialization (E4.6); this is the cheap, pure pre-check.
func validateConstructLoads(construct map[string]any) error {
	if construct == nil {
		return fmt.Errorf("nil construct")
	}
	if pcs, ok := construct["preconditions"].([]any); ok {
		seen := make(map[string]struct{}, len(pcs))
		for _, e := range pcs {
			m, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("precondition is not an object")
			}
			id := strings.TrimSpace(stringField(m, "id"))
			if id == "" {
				return fmt.Errorf("precondition missing id")
			}
			if strings.TrimSpace(stringField(m, "check")) == "" {
				return fmt.Errorf("precondition %q missing check", id)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("duplicate precondition id %q", id)
			}
			seen[id] = struct{}{}
		}
	}
	if steps, ok := construct["steps"].([]any); ok {
		for i, e := range steps {
			m, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("step %d is not an object", i)
			}
			if strings.TrimSpace(stringField(m, "id")) == "" {
				return fmt.Errorf("step %d missing id", i)
			}
		}
	}
	return nil
}

// --- helpers --------------------------------------------------------------

// deepCopyMap returns a deep copy of m via a JSON round-trip. The patch model
// only ever deals with JSON-shaped construct data (the loader produces it via
// json.Marshal), so a round-trip is a faithful, allocation-isolated copy that
// guarantees Apply never aliases into the immutable base.
func deepCopyMap(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// stringField reads a string field from a map, returning "" when absent or
// not a string.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
