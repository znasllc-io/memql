package memoryNodes

import (
	"sort"
	"testing"
)

// concept_secret_test.go is the schema-layer half of memql#3036: the @secret
// field annotation must surface as the x-secret custom JSON-Schema keyword AND
// be enumerable through Concept.SecretFields(), so the validation-error
// redaction path can ask a concept which of its fields must never have their
// values quoted into an operator-visible string.
//
// Deliberately the same shape as concept_pii_test.go. @pii is the working
// template the ruling names: PIIFields() drives the @scrubPii update path, and
// SecretFields() drives redaction the same generic way -- annotate a field and
// the behaviour follows, with no hand-maintained list to drift.

func TestConcept_SecretFields_EnumeratesAnnotatedFields(t *testing.T) {
	content := []byte(`
@description("A credential record.")
concept Credential {
  apiKey       string  @required  @secret  @description("Vendor key.")
  refreshToken string  @secret
  label        string  @description("Not secret.")
}
`)

	c, err := ParseConceptMemQL(content, "v1/test/credential")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	got := c.SecretFields()
	sort.Strings(got)
	want := []string{"apiKey", "refreshToken"}
	if len(got) != len(want) {
		t.Fatalf("SecretFields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SecretFields() = %v, want %v", got, want)
		}
	}
}

// A newly-annotated field must flow through with no other code change -- the
// property that makes this generic rather than a list someone has to remember
// to update.
func TestConcept_SecretFields_NewlyAnnotatedFieldAutoIncluded(t *testing.T) {
	before := []byte(`
concept Credential {
  apiKey  string  @required  @secret
  webhook string
}
`)
	after := []byte(`
concept Credential {
  apiKey  string  @required  @secret
  webhook string  @secret
}
`)

	cBefore, err := ParseConceptMemQL(before, "v1/test/credential")
	if err != nil {
		t.Fatalf("parse before: %v", err)
	}
	cAfter, err := ParseConceptMemQL(after, "v1/test/credential")
	if err != nil {
		t.Fatalf("parse after: %v", err)
	}

	if containsStr(cBefore.SecretFields(), "webhook") {
		t.Fatal("webhook must not be secret before it is annotated")
	}
	if !containsStr(cAfter.SecretFields(), "webhook") {
		t.Fatal("webhook must be auto-included in SecretFields() once @secret-annotated")
	}
}

// The common case: a concept with no @secret field enumerates none. Guards
// against a nil-schema or unmarshal slip returning every field.
func TestConcept_SecretFields_NoneWhenUnannotated(t *testing.T) {
	content := []byte(`
concept Plain {
  name   string  @required
  status string
}
`)
	c, err := ParseConceptMemQL(content, "v1/test/plain")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}
	if len(c.SecretFields()) != 0 {
		t.Fatalf("SecretFields() = %v, want none", c.SecretFields())
	}
}

// A nil concept must not panic -- SecretFields is called from the function
// loader against a concept the registry may not resolve.
func TestConcept_SecretFields_NilConcept(t *testing.T) {
	var c *Concept
	if got := c.SecretFields(); got != nil {
		t.Fatalf("SecretFields() on nil concept = %v, want nil", got)
	}
}
