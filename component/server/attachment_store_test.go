package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDslCall_EscapesEmbeddedQuotes is the regression guard for the
// go/unsafe-quoting alert on the attachment query builders: a value that
// contains a double quote must NOT be able to break out of its enclosing
// JSON literal. dslCall marshals the whole argument object, so the result
// is always a single well-formed `fn({...})` call whose args round-trip
// back to the original values.
func TestDslCall_EscapesEmbeddedQuotes(t *testing.T) {
	malicious := `x", "partitionId": "evil`
	q, err := dslCall("attachmentById", map[string]any{
		"attachmentId": malicious,
		"partitionId":  "s1",
	})
	if err != nil {
		t.Fatalf("dslCall returned error: %v", err)
	}

	const prefix = "attachmentById("
	if !strings.HasPrefix(q, prefix) || !strings.HasSuffix(q, ")") {
		t.Fatalf("unexpected call shape: %q", q)
	}
	objJSON := strings.TrimSuffix(strings.TrimPrefix(q, prefix), ")")

	// The argument object must be valid JSON that decodes back to exactly
	// the inputs -- proving the embedded quote was escaped, not injected.
	var got map[string]any
	if err := json.Unmarshal([]byte(objJSON), &got); err != nil {
		t.Fatalf("argument object is not valid JSON (%v): %q", err, objJSON)
	}
	if got["attachmentId"] != malicious {
		t.Fatalf("attachmentId mangled: got %q, want %q", got["attachmentId"], malicious)
	}
	if got["partitionId"] != "s1" {
		t.Fatalf("partitionId broken out to %q; quote escaping failed", got["partitionId"])
	}
}
