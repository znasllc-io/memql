package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// FunctionMutationTemplate is a compiled representation of a mutation function body.
// It is evaluated at execution time using function call args and engine variable lookup.
//
// Its values are parsed edition-2026 expression nodes (epic memql#5363):
// IDTemplate, CreatedAtTemplate, ParentTemplate and AliasOfTemplate each an
// ast.ExpressionNode or nil; PayloadTemplate the block's fields in the layout
// the loader has always produced -- a map[string]any of field -> node, a
// nested object literal as a nested map and a list literal as a []any -- or
// one node for a `payload:` splat; PayloadOverlayTemplate the same layout.
// newMutationTemplateV1 (mutation_values_v1.go) is the one builder and EvalExpr
// the one evaluator. The layout is kept, rather than one map node, because
// three load-time gates read it (validateMutationCallerArgs,
// destructiveNestedObjectFields, OwnerFieldProvenance).
type FunctionMutationTemplate struct {
	// Kind = "insert" (full payload) or "update" (read-merge-validate-write).
	// Empty defaults to insert for backwards compatibility with templates
	// compiled before update() landed.
	Kind ast.MutationKind

	Concept string

	// IDTemplate is an optional template for the explicit ID.
	// If omitted or evaluates to empty, the engine will generate an ID (content-addressed).
	IDTemplate any

	// CreatedAtTemplate is an optional timestamp override for node creation time.
	// If provided, it must evaluate to an RFC3339/RFC3339Nano timestamp string.
	CreatedAtTemplate any

	// PayloadTemplate is the payload template (required).
	// It must evaluate to an object (map[string]any) at execution time.
	PayloadTemplate any

	// PayloadOverlayTemplate carries explicit insert-block fields that
	// must overlay on top of the splatted PayloadTemplate at render
	// time. Populated only when the insert block mixes `args.<arg>`
	// (which sets PayloadTemplate to the whole evaluated object) with
	// other explicit fields like `ownerUserId: actor.userId`. The
	// overlay wins on key collision so authz-relevant server-side
	// stamps cannot be displaced by a caller's payload (memql#401).
	// Each entry's value is laid out as the payload's are.
	PayloadOverlayTemplate map[string]any

	// ParentTemplate is an optional parent relationship hint.
	ParentTemplate any

	// AliasOfTemplate is an optional aliasOf relationship hint.
	AliasOfTemplate any

	// AppendFields carries the mutation's @appendFields("a", "b")
	// annotation: array-typed payload fields whose partial-write
	// elements the update executor APPENDS to the stored array instead
	// of replacing it wholesale. Only valid on update-kind mutations
	// (memql#2240).
	AppendFields []string

	// AddToSetFields and RemoveFromSetFields carry the mutation's
	// @addToSet / @removeFromSet annotations: array-typed payload fields
	// the update executor treats as SET MEMBERSHIP rather than as a value
	// to replace. Only valid on update-kind mutations (memql#4951).
	AddToSetFields      []string
	RemoveFromSetFields []string

	// MergeFields carries the mutation's @mergeFields("a", "b")
	// annotation: object-typed payload fields that the update executor
	// deep-merges into the stored object instead of replacing it
	// wholesale. Only valid on update-kind mutations (enforced at
	// load time in function_loader.go). See memql#1339.
	MergeFields []string

	// CreateOnlyFields carries the mutation's @createOnly("a", "b")
	// annotation: payload fields written ONLY on create. When the target
	// id already exists, executeWrite drops them from the delta before the
	// read-merge so the stored value is preserved. Only valid on
	// insert-kind mutations (enforced at load time in function_loader.go).
	// See fylo#63.
	CreateOnlyFields []string

	// NoUnsetFields carries the mutation's @noUnset("a", "b") annotation:
	// payload fields a write may set but may never blank. On the read-merge
	// path executeWrite drops a named field from the delta when the incoming
	// value is empty and the stored one is not. Valid on both mutation kinds.
	// See memql#3415.
	NoUnsetFields []string

	// ScrubPii carries the mutation's @scrubPii annotation: when set,
	// the update executor enumerates every @pii-annotated field on the
	// bound concept and zeroes it after the partial payload merges,
	// making the hard-delete PII scrub annotation-driven instead of a
	// hand-maintained field list. Only valid on update-kind mutations
	// (enforced at load time in function_loader.go). See memql#1711.
	ScrubPii bool
}

// renderMutationTemplate evaluates a mutation template into a concrete
// MutationNode: the template's parsed values rendered by EvalExpr over the
// call's arguments (renderMutationTemplateV1, mutation_values_v1.go).
func (e *MemQLEngine) renderMutationTemplate(ctx context.Context, tmpl *FunctionMutationTemplate, args map[string]any) (MutationNode, error) {
	if e == nil {
		return MutationNode{}, fmt.Errorf("engine is nil")
	}
	if tmpl == nil {
		return MutationNode{}, fmt.Errorf("mutation template is nil")
	}
	concept := strings.TrimSpace(tmpl.Concept)
	if concept == "" {
		return MutationNode{}, fmt.Errorf("mutation template concept is required")
	}
	return e.renderMutationTemplateV1(ctx, tmpl, concept, args)
}

// templateMutationNode assembles the MutationNode a rendered template
// produces: the rendered values plus every annotation-driven field the
// template carries, so a field added to the template reaches every write a
// mutation makes.
func templateMutationNode(tmpl *FunctionMutationTemplate, concept, id string, payloadJSON []byte, createdAtRef *time.Time, parent, aliasOf string) MutationNode {
	var parentRef *string
	if strings.TrimSpace(parent) != "" {
		copy := parent
		parentRef = &copy
	}

	var aliasRef *string
	if strings.TrimSpace(aliasOf) != "" {
		copy := aliasOf
		aliasRef = &copy
	}

	kind := tmpl.Kind
	if kind == "" {
		kind = ast.MutationKindInsert
	}

	return MutationNode{
		Kind: kind,
		// This node came from a DSL mutation template, so its `accept { }`
		// / `stamp { }` blocks have already run and the author has stated
		// who owns the row. The engine's raw-path owner stamp
		// (memql#3175) leaves it alone; every OTHER producer of a
		// MutationNode is treated as raw. See the field's doc comment for
		// why the flag points this way round.
		FromTemplate:     true,
		Concept:          concept,
		ID:               strings.TrimSpace(id),
		PayloadRaw:       string(payloadJSON),
		CreatedAt:        createdAtRef,
		ParentRef:        parentRef,
		AliasOfRef:       aliasRef,
		MergeFields:      tmpl.MergeFields,
		AppendFields:     tmpl.AppendFields,
		AddToSet:         tmpl.AddToSetFields,
		RemoveFromSet:    tmpl.RemoveFromSetFields,
		CreateOnlyFields: tmpl.CreateOnlyFields,
		NoUnsetFields:    tmpl.NoUnsetFields,
		ScrubPii:         tmpl.ScrubPii,
	}
}

func parseRFC3339Timestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("timestamp is empty")
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts.UTC(), nil
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return ts.UTC(), nil
}

// missingValue is a sentinel used to distinguish a missing optional argument
// from an explicit null value. Missing args are omitted from payload objects.
// EvalExpr's `??` hands an Absent arm to coalesceSelect as this sentinel
// (exprCoalesceArm, expr_eval.go).
type missingValue struct{}

func isMissing(v any) bool {
	_, ok := v.(missingValue)
	return ok
}

// resolveActorReference resolves an `actor.<path>` reference through the auth
// envelope on ctx, NOT through caller-passed args. A mutation value reads the
// envelope one field at a time through it (actorEnvelopeV1,
// mutation_values_v1.go).
//
// It is the ONE binding of an actor path into a write. The two evaluators that
// used to render mutation values resolved it independently until memql#2840,
// and only one of them implemented it -- which is why
// `update { id: actor.userId }` silently selected nothing.
//
// `actor.userId` against a context carrying no caller REFUSES rather than
// rendering "" (memql#3620). 75 mutations in the tree stamp an owner from this
// path; nothing downstream would have caught an empty one -- no concept schema
// declares a minLength on an owner field, and stampRowAuthzOwner (the create-side
// backstop that DOES refuse an empty caller) early-returns on FromTemplate, which
// is exactly what these writes are. The row it would mint is readable and
// writable by nobody, while wearing the value an unauthenticated owned-tier read
// compares against -- so the read half of this fix would hand it straight back.
func resolveActorReference(ctx context.Context, ref string) (any, error) {
	trimmed := strings.TrimSpace(ref)
	path := strings.TrimSpace(strings.TrimPrefix(trimmed, "actor."))
	if path == "" {
		return nil, fmt.Errorf("actor: actor path is required")
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			continue
		}
		return nil, fmt.Errorf("actor.<path>: invalid character %q in %q", c, trimmed)
	}
	ac, _ := auth.AccessFromContext(ctx)
	// One canonical envelope (#2623): auth.ActorEnvelopeFields.
	v, known, err := auth.ActorEnvelopeBind(ac, path)
	if err != nil {
		return nil, fmt.Errorf(
			"actor.%s cannot be bound into a write: %w. Writing the empty string would mint a "+
				"row owned by nobody, wearing the value an owned-tier read compares against. "+
				"Refused (memql#3620). Authenticate the call, or run the mutation from a "+
				"context carrying an AccessContext (auth.ContextWithUserActor stamps one)",
			path, err)
	}
	if known {
		return v, nil
	}
	return nil, fmt.Errorf("unsupported actor reference path %q (valid: %s)", path, auth.ActorEnvelopeValidNames())
}

// coalesceSelect is THE coalesce selection rule (memql#1614), driven over
// lazily-evaluated arms: EvalExpr's `??` selects through it (expr_eval.go), in
// every position a mutation value, a logic body or a condition reaches.
//
// The rule: the FINAL argument is the ultimate fallback and is returned even
// when it resolves to a blank string, while every NON-final argument is skipped
// when it is nil, missing, or blank. `coalesce(args.x, "")` must be able to land
// an explicit empty -- a null there fails JSON-schema validation on a non-
// required string field -- and a blank non-final arm must NOT win, or
// `id: args.roleId ?? args.slug` yields "" when roleId is the empty string
// clients send for an absent optional, which makes the engine mint a RANDOM id
// instead of the deterministic fallback and turns idempotent upserts into
// duplicate rows (memql#2772 review).
//
// The mutation values' two former evaluators disagreed on one input before
// memql#3627 -- a blank MIDDLE arm with nothing later resolving gave "" from
// the string form and nil from the AST form. Harmless where it sat, and that
// is the point: this file's history (memql#2840, memql#2925, memql#3618) is a
// list of exactly such divergences sitting harmless until a construct lands
// in the other slot, which is why there is one rule and one evaluator.
//
// A missing FINAL arm is normalised to nil rather than leaked as the sentinel:
// IsTruthy(missingValue{}) is TRUE, so leaking it would make an all-missing
// coalesce read as truthy inside And/Or/Not.
func coalesceSelect(n int, evalArm func(i int) (any, error)) (any, error) {
	for i := 0; i < n; i++ {
		ev, err := evalArm(i)
		if err != nil {
			return nil, err
		}
		if i == n-1 {
			if isMissing(ev) {
				return nil, nil
			}
			return ev, nil
		}
		if ev == nil || isMissing(ev) {
			continue
		}
		if s, isStr := ev.(string); isStr && strings.TrimSpace(s) == "" {
			continue
		}
		return ev, nil
	}
	return nil, nil
}

// mutationMergeFields extracts the @mergeFields("a", "b") annotation
// from a mutation definition into the field-name list the update
// executor consults. Only meaningful on update-kind mutations (insert
// writes a full payload -- there is nothing stored to merge against),
// so its presence on an insert mutation is a load-time error rather
// than a silent no-op. See memql#1339.
func mutationMergeFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrMergeFields {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@mergeFields requires at least one field name, e.g. @mergeFields("preferences")`)
		}
	}
	if len(fields) > 0 && kind != ast.MutationKindUpdate {
		return nil, fmt.Errorf("@mergeFields is only valid on update mutations (insert writes the full payload; there is nothing stored to merge against)")
	}
	return fields, nil
}

// mutationAppendFields extracts the @appendFields("a", "b") annotation
// from a mutation definition into the field-name list the update
// executor appends (rather than replaces). Only meaningful on update-kind
// mutations -- an insert writes the full payload, so there is no stored
// array to append to -- so its presence on an insert is a load-time error
// rather than a silent no-op. See memql#2240.
func mutationAppendFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrAppendFields {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@appendFields requires at least one field name, e.g. @appendFields("attachmentIds")`)
		}
	}
	if len(fields) > 0 && kind != ast.MutationKindUpdate {
		return nil, fmt.Errorf("@appendFields is only valid on update mutations (insert writes the full payload; there is nothing stored to append to)")
	}
	return fields, nil
}

// mutationSetFields extracts @addToSet("a", "b") / @removeFromSet("a", "b")
// into the field-name list the update executor treats as SET MEMBERSHIP
// (memql#4951).
//
// One extractor for both because the two annotations differ only in
// direction, and the update executor serves them from one routine for the
// same reason: what the engine sees is a membership change, what an author
// writes is a verb.
//
// Like @appendFields, only meaningful on an update -- an insert writes the
// full payload, so there is no stored set to change -- and its presence on an
// insert is a load-time error rather than a silent no-op.
func mutationSetFields(funcDef *languageParser.FunctionDef, attrName string, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != attrName {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@%s requires at least one field name, e.g. @%s("disabledDeployables")`, attrName, attrName)
		}
	}
	if len(fields) > 0 && kind != ast.MutationKindUpdate {
		return nil, fmt.Errorf("@%s is only valid on update mutations (insert writes the full payload; there is no stored set to change)", attrName)
	}
	return fields, nil
}

// validateSetMembershipFields refuses a field claimed by two of the
// array-rewriting annotations at once.
//
// Each of them rewrites the same key of the delta, and the executor applies
// them in a fixed order, so a field named by two is silently decided by that
// order -- which is an implementation detail nobody authoring the mutation can
// see. Refusing it at load costs nothing and removes the question.
func validateSetMembershipFields(appendFields, addToSet, removeFromSet []string) error {
	seen := map[string]string{}
	for _, group := range []struct {
		attr   string
		fields []string
	}{
		{"appendFields", appendFields},
		{"addToSet", addToSet},
		{"removeFromSet", removeFromSet},
	} {
		for _, f := range group.fields {
			if prior, dup := seen[f]; dup {
				return fmt.Errorf("field %q is named by both @%s and @%s; each rewrites the same "+
					"key of the write, so which one wins would be decided by the executor's "+
					"order rather than by anything written here", f, prior, group.attr)
			}
			seen[f] = group.attr
		}
	}
	return nil
}

// mutationCreateOnlyFields extracts the @createOnly("a", "b") annotation
// from a mutation definition into the field-name list executeWrite drops
// from the delta when the target id already exists (memql#1709 read-merge),
// so those fields keep their stored value on a re-stage instead of being
// clobbered. The inverse of @mergeFields/@appendFields: it is only
// meaningful on insert-kind (create-or-upsert) mutations -- an update()
// always targets an existing row, so a create-only field would ALWAYS be
// dropped and could never be written, a silent footgun. Its presence on an
// update mutation is therefore a load-time error. See fylo#63.
func mutationCreateOnlyFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrCreateOnly {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@createOnly requires at least one field name, e.g. @createOnly("status", "attempts")`)
		}
	}
	if len(fields) > 0 && kind == ast.MutationKindUpdate {
		return nil, fmt.Errorf("@createOnly is only valid on insert mutations (an update always targets an existing row, so a create-only field could never be written)")
	}
	return fields, nil
}

// mutationNoUnsetFields extracts the @noUnset("a", "b") annotation from a
// mutation definition into the field-name list executeWrite drops from the
// delta when the incoming value is empty and the stored row holds a non-empty
// one -- making those fields one-way (memql#3415).
//
// Unlike @createOnly this is valid on BOTH mutation kinds, and deliberately
// so: the field it exists for (clusterSettings.bootstrappedAt) is written by an
// insert-kind create AND by an update-kind admin edit, and either can carry an
// empty value. Neither may un-set it.
func mutationNoUnsetFields(funcDef *languageParser.FunctionDef) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrNoUnset {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@noUnset requires at least one field name, e.g. @noUnset("bootstrappedAt")`)
		}
	}
	return fields, nil
}

// mutationScrubPii reports whether the mutation carries the @scrubPii
// annotation, which opts an update-kind mutation into engine-side
// generic PII scrubbing (memql#1711). Only meaningful on update-kind
// mutations -- an insert writes the full payload and has nothing stored
// to scrub against -- so its presence on an insert mutation is a
// load-time error rather than a silent no-op. The bound concept supplies
// the field set at execution time via Concept.PIIFields(), so no field
// names are listed on the annotation itself: that is the whole point --
// the scrub stays in sync with the schema automatically.
func mutationScrubPii(funcDef *languageParser.FunctionDef, kind ast.MutationKind) (bool, error) {
	var present bool
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrScrubPii {
			continue
		}
		present = true
	}
	if present && kind != ast.MutationKindUpdate {
		return false, fmt.Errorf("@scrubPii is only valid on update mutations (insert writes the full payload; there is nothing stored to scrub against)")
	}
	return present, nil
}
