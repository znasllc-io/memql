package main

import (
	"strings"
	"testing"
)

// The emitted model carries what the plan knows about a field's value: its
// Admin type on every field, and the mirrored types an object keeps the GID
// of where it keeps any. The connector decides what a customer's erasure keeps
// from them (integrations/shopify/erasure_fields.go), so a field emitted
// without its Admin type would be a date the erasure takes for a name.
func TestTheModelCarriesWhatAFieldsValueIs(t *testing.T) {
	spec := emitTypeSpec(planFor(t, "thing"), "", "", nil)
	for _, want := range []string{
		`{Name: "sourceCreatedAt", GraphQL: "createdAt", Kind: "scalar", DSLType: "string", Extract: "createdAt", AdminType: "DateTime"},`,
		`{Name: "currency", GraphQL: "currency", Kind: "scalar", DSLType: "string", Extract: "currency", AdminType: "BigEnum"},`,
		`{Name: "widgetGid", GraphQL: "widget", Kind: "gid", DSLType: "string", Extract: "widget.id", AdminType: "Widget"},`,
		`{Name: "sticker", GraphQL: "sticker", Kind: "object", DSLType: "object", Extract: "sticker", AdminType: "Sticker", GidsInside: []string{"Widget"}},`,
		`{Name: "address", GraphQL: "address", Kind: "object", DSLType: "object", Extract: "address", AdminType: "Address"},`,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("the emitted spec lacks:\n  %s", want)
		}
	}
	model := planSetFor(t).EmitModelFile("2026-07", testAllowlist(t), nil, nil, nil)
	for _, want := range []string{"\tAdminType string\n", "\tGidsInside []string\n"} {
		if !strings.Contains(model, want) {
			t.Errorf("FieldSpec does not declare %q", strings.TrimSpace(want))
		}
	}
}
