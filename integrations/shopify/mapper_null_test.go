package shopify

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// mapper_null_test.go -- a null from the origin is an ABSENT field, not a
// null in the payload.
//
// Every generated concept field is optional and typed (`string`, `object`,
// ...), and the engine's schema is `additionalProperties: false` with typed
// properties: a JSON null does not validate as a string. Shopify returns
// null for any nullable field that is simply unset -- an inventory level's
// deactivationAlert, an inventory item's countryCodeOfOrigin -- so a mirror
// write that carried the null was refused on every sweep
// ("expected string, but got null"), 357 times per ten-minute tick on one
// store. The mirror insert is wholesale, so omitting the key already means
// "cleared"; writing null adds nothing but the refusal.
func TestMapObjectOmitsNullValuesSoTheConceptSchemaAccepts(t *testing.T) {
	spec := &generated.TypeSpec{
		Concept: "inventoryLevel",
		Fields: []generated.FieldSpec{
			{Name: "deactivationAlert", GraphQL: "deactivationAlert", Kind: generated.KindScalar, DSLType: "string", Extract: "deactivationAlert"},
			{Name: "countryCodeOfOrigin", GraphQL: "countryCodeOfOrigin", Kind: generated.KindEnum, DSLType: "string", Extract: "countryCodeOfOrigin"},
			{Name: "location", GraphQL: "location", Kind: generated.KindGid, DSLType: "string", Extract: "location.id"},
			{Name: "quantities", GraphQL: "quantities", Kind: generated.KindObjectList, DSLType: "[]object", Extract: "quantities"},
			{Name: "canDeactivate", GraphQL: "canDeactivate", Kind: generated.KindScalar, DSLType: "bool", Extract: "canDeactivate"},
			{Name: "position", GraphQL: "position", Kind: generated.KindScalar, DSLType: "int", Extract: "position"},
		},
	}
	obj := map[string]any{
		"id":                  "gid://shopify/InventoryLevel/1",
		"deactivationAlert":   nil,
		"countryCodeOfOrigin": nil,
		"location":            nil,
		"quantities": []any{map[string]any{
			"name":      "available",
			"quantity":  float64(3),
			"updatedAt": nil,
		}},
		"canDeactivate": false,
		"position":      nil,
	}
	writes := mapObject(spec, "store-1", obj, "", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(writes))
	}
	payload := writes[0].Payload
	for _, cleared := range []string{"deactivationAlert", "countryCodeOfOrigin", "location"} {
		if v, present := payload[cleared]; !present || v != "" {
			t.Errorf("%s = %#v; a null string from the origin is written as \"\", which validates and clears the stored value under read-merge", cleared, v)
		}
	}
	if v, present := payload["quantities"]; !present || v == nil {
		t.Errorf("quantities = %#v", v)
	}
	if _, present := payload["position"]; present {
		t.Errorf("a null int must be omitted, not written as a zero that reads as a real value: %#v", payload["position"])
	}
	if v, ok := payload["canDeactivate"].(bool); !ok || v {
		t.Errorf("canDeactivate = %#v; a real false must survive, only null is dropped", payload["canDeactivate"])
	}
	items, _ := payload["quantities"].([]any)
	if len(items) != 1 {
		t.Fatalf("quantities = %#v", payload["quantities"])
	}
	first, _ := items[0].(map[string]any)
	if _, present := first["updatedAt"]; present {
		t.Errorf("a null INSIDE a nested object must be omitted too: %#v", first)
	}
	if first["name"] != "available" || first["quantity"] != float64(3) {
		t.Errorf("the nested object lost real values: %#v", first)
	}
}
