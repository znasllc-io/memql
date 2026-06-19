package conformance

// dim_pii_test.go -- the @pii-scrub dimension (#1711). After a hard delete every
// field the concept marks @pii must be cleared to its type-zero value; non-PII
// fields are untouched. The scrub is annotation-driven, so this enumerates the
// live concept's PIIFields() rather than a hard-coded list -- a newly @pii-tagged
// field is covered automatically.

import (
	"testing"
)

func piiScrubCheck() check {
	return check{
		Issue:   "#1711",
		Dim:     "pii-scrubbed-on-hard-delete",
		NeedsDB: true,
		Run:     runPIIScrub,
	}
}

func runPIIScrub(t *testing.T, e *Env) {
	const conceptName = "v1:identity:user"
	concept, err := e.Registry.Get(conceptName)
	if err != nil || concept == nil {
		t.Fatalf("#1711: user concept not in registry: %v", err)
	}
	piiFields := concept.PIIFields()
	if len(piiFields) == 0 {
		t.Fatalf("#1711: %s declares no @pii fields; the scrub gate has nothing to enforce", conceptName)
	}

	userId := "user-pii-" + uniqueSuffix("scrub")
	storedId := storedID(t, e.runMutation(t, "mutationCreateUser", map[string]any{
		"userId":       userId,
		"displayName":  "Quinn Conformance",
		"primaryEmail": "quinn.conformance@example.com",
		"role":         "reader",
	}))

	before := e.latestPayload(t, conceptName, storedId)
	// Sanity: the PII we set is present pre-delete.
	if before["displayName"] == "" || before["primaryEmail"] == "" {
		t.Fatalf("#1711: PII not present before delete: %#v", before)
	}

	// Hard delete -> the engine scrubs every @pii field by annotation.
	e.runMutation(t, "mutationDeleteUserHard", map[string]any{"userId": userId})

	after := e.latestPayload(t, conceptName, storedId)
	if after["active"] != false {
		t.Errorf("#1711: hard delete must flip active=false; got %v", after["active"])
	}

	cleared := 0
	for _, f := range piiFields {
		v, present := after[f]
		if !present {
			// An unset field stays absent -- correct (the scrub doesn't
			// materialise zero values for fields that were never written).
			continue
		}
		if !isZeroish(v) {
			t.Errorf("#1711: @pii field %q NOT scrubbed after hard delete: %#v", f, v)
			continue
		}
		cleared++
	}
	// The two PII fields we explicitly set MUST be present-and-empty.
	for _, f := range []string{"displayName", "primaryEmail"} {
		if v, present := after[f]; !present || !isZeroish(v) {
			t.Errorf("#1711: set @pii field %q not scrubbed to empty (present=%v val=%#v)", f, present, v)
		}
	}
	t.Logf("#1711: hard delete scrubbed all %d enumerated @pii fields (%d materialised-and-cleared)", len(piiFields), cleared)
}

// isZeroish reports whether v is the type-zero the scrub writes: ""/false/0/
// empty-slice/empty-map/nil.
func isZeroish(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case int:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}
