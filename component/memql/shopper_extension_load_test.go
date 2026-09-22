package memql

import (
	"strings"
	"testing"
)

// shopper_extension_load_test.go -- turning a loaded mutation into a
// registered extension (design record 2026-09-21, section 4.2).

func extensionMutation() *Function {
	optional := true
	return &Function{
		Name:         "recordFyloApplicationDetail",
		FunctionKind: "mutation",
		BoundConcept: "v1:fylo:applicationDetail",
		ShopperFormExtension: &ShopperFormExtensionDecl{
			Pack: "wholesale", Form: "application",
		},
		ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{
			// THE STAMPED THREE. Declared so the body can read them, and
			// never offered to a shopper.
			{Name: "submissionId", Type: "string"},
			{Name: "storeId", Type: "string"},
			{Name: "siteId", Type: "string", Optional: optional},
			// The client's own.
			{Name: "address", Type: "string", MaxLength: 200},
			{Name: "ein", Type: "string", Optional: optional, MaxLength: 20},
			{Name: "locations", Type: "number", Optional: optional},
			{Name: "volume", Type: "string", Enum: []any{"under500", "over500"}},
		}},
	}
}

// THE FIELD LIST IS THE ARGS BLOCK MINUS THE STAMPED NAMES (D7). Derived
// rather than restated, which removes the class of bug where a Go pack's
// two lists drift apart.
func TestAnExtensionsFieldsAreItsArgsMinusTheStampedNames(t *testing.T) {
	ext, err := shopperExtensionFromFunction(extensionMutation())
	if err != nil {
		t.Fatalf("a valid extension mutation was refused: %v", err)
	}

	got := map[string]ShopperField{}
	for _, f := range ext.Fields {
		got[f.Name] = f
	}
	for _, stamped := range []string{"submissionId", "storeId", "siteId"} {
		if _, offered := got[stamped]; offered {
			t.Errorf("%q is offered to a shopper; it is stamped server-side", stamped)
		}
	}
	if len(got) != 4 {
		t.Fatalf("derived %d fields, want 4 (address, ein, locations, volume): %v", len(got), got)
	}
	if !got["address"].Required {
		t.Error("address is declared without @optional and must be Required")
	}
	if got["ein"].Required {
		t.Error("ein is optional in the args block and must not be Required")
	}
	if got["ein"].MaxLength != 20 {
		t.Errorf("ein MaxLength = %d, want 20", got["ein"].MaxLength)
	}
	// A form always sends a string; Numeric is what makes @minimum see a
	// number rather than refusing every value.
	if !got["locations"].Numeric {
		t.Error("a number-typed arg did not become a Numeric field")
	}
	if len(got["volume"].Enum) != 2 {
		t.Errorf("volume Enum = %v, want two members", got["volume"].Enum)
	}
}

// THE DECLARING DOMAIN IS THE DOMAIN OF THE CONCEPT IT WRITES, which is
// also what stops an extension writing somebody else's concept.
func TestAnExtensionsDomainComesFromTheConceptItWrites(t *testing.T) {
	ext, err := shopperExtensionFromFunction(extensionMutation())
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if ext.Domain != "fylo" {
		t.Errorf("Domain = %q, want fylo (from v1:fylo:applicationDetail)", ext.Domain)
	}
	if ext.Construct != "recordFyloApplicationDetail" {
		t.Errorf("Construct = %q", ext.Construct)
	}
	if ext.Pack != "wholesale" || ext.Form != "application" {
		t.Errorf("route = %s/%s, want wholesale/application", ext.Pack, ext.Form)
	}
}

// D2. A logic may call builtins, and pointing a public form at one would
// put arbitrary builtin calls within reach of the internet.
func TestAnExtensionOnAnythingButAMutationIsRefused(t *testing.T) {
	for _, kind := range []string{"logic", "query", "builtin", ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			fn := extensionMutation()
			fn.FunctionKind = kind
			_, err := shopperExtensionFromFunction(fn)
			if err == nil {
				t.Fatalf("@shopperFormExtension on a %q was accepted", kind)
			}
			// Case-insensitive: the refusal shouts the word ("a MUTATION
			// alone"), and what matters is that it names the requirement.
			if !strings.Contains(strings.ToLower(err.Error()), "mutation") {
				t.Errorf("the refusal does not say a mutation is required: %v", err)
			}
		})
	}
}

func TestAnExtensionWithNoBoundConceptIsRefused(t *testing.T) {
	fn := extensionMutation()
	fn.BoundConcept = ""
	if _, err := shopperExtensionFromFunction(fn); err == nil {
		t.Fatal("an extension whose mutation binds no concept was accepted; its domain is unknowable")
	}
}

// An args block of nothing but the stamped names declares no shopper field
// at all, which is a route addition that adds nothing.
func TestAnExtensionOfferingOnlyStampedFieldsIsRefused(t *testing.T) {
	fn := extensionMutation()
	fn.ArgsSchema = &ArgsSchemaConfig{Fields: []*FunctionArgsField{
		{Name: "submissionId", Type: "string"},
		{Name: "storeId", Type: "string"},
	}}
	if _, err := shopperExtensionFromFunction(fn); err == nil {
		t.Fatal("an extension declaring no fields of its own was accepted")
	}
}
