package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// executor_mutation_pii_test.go guards the annotation-driven hard-delete
// PII scrub (memql#1711). Two layers:
//
//   - Pure unit tests for scrubPIIFields / zeroValueFor: type-correct
//     zeroing of an arbitrary field set, with no DB.
//   - A Postgres-gated end-to-end compliance test reproducing the Run-4
//     evidence: after deleteUserHard, NO @pii-annotated field
//     remains populated on the tombstoned user row.

func TestScrubPIIFields_ZeroesByType(t *testing.T) {
	payload := map[string]any{
		"displayName": "Quinn Bee",
		"firstName":   "Quinn",
		"lastName":    "Bee",
		"phone":       "15550002222",
		"primaryRole": "Tester",
		"gender":      "nonbinary",
		"age":         float64(42), // JSON numbers decode to float64
		"verified":    true,
		"tags":        []any{"a", "b"},
		"nested":      map[string]any{"k": "v"},
		// Non-PII fields that MUST survive untouched.
		"active":   false,
		"tenantId": "tenant-keep",
	}

	pii := []string{
		"displayName", "firstName", "lastName", "phone", "primaryRole",
		"gender", "age", "verified", "tags", "nested",
		// A field absent from the payload: must be skipped, not added.
		"neverPresent",
	}
	scrubPIIFields(payload, pii)

	require.Equal(t, "", payload["displayName"])
	require.Equal(t, "", payload["firstName"])
	require.Equal(t, "", payload["lastName"])
	require.Equal(t, "", payload["phone"])
	require.Equal(t, "", payload["primaryRole"])
	require.Equal(t, "", payload["gender"])
	require.Equal(t, 0, payload["age"])
	require.Equal(t, false, payload["verified"])
	require.Equal(t, []any{}, payload["tags"])
	require.Equal(t, map[string]any{}, payload["nested"])

	// Absent PII field stays absent (no spurious "" key).
	_, present := payload["neverPresent"]
	require.False(t, present, "absent PII field must not be materialized")

	// Non-PII fields are untouched.
	require.Equal(t, false, payload["active"])
	require.Equal(t, "tenant-keep", payload["tenantId"])
}

func TestScrubPIIFields_NoPIIFieldsIsNoOp(t *testing.T) {
	payload := map[string]any{"label": "keep", "count": float64(3)}
	scrubPIIFields(payload, nil)
	require.Equal(t, "keep", payload["label"])
	require.Equal(t, float64(3), payload["count"])
}

func TestZeroValueFor_Types(t *testing.T) {
	require.Equal(t, "", zeroValueFor("x"))
	require.Equal(t, false, zeroValueFor(true))
	require.Equal(t, 0, zeroValueFor(float64(7)))
	require.Equal(t, 0, zeroValueFor(int(7)))
	require.Equal(t, []any{}, zeroValueFor([]any{1, 2}))
	require.Equal(t, map[string]any{}, zeroValueFor(map[string]any{"a": 1}))
	require.Equal(t, "", zeroValueFor(nil))
}

// TestHardDelete_ScrubsAllPIIFields is the compliance contract: boot a
// real engine, create a user with every PII field populated (the Run-4
// fixture: Quinn Bee, phone, Tester role, gender), run
// deleteUserHard, and assert NOT ONE @pii-annotated field
// survives on the tombstoned row. Before the fix, firstName / lastName /
// phone / primaryRole / gender were retained -- this test FAILS against
// that code and PASSES with the @scrubPii annotation-driven scrub.
//
// Postgres-gated (reuses readMergeTestEngine), like the sibling
// read-merge DB tests.
func TestHardDelete_ScrubsAllPIIFields(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:user"
	userId := "user-" + uniqueSuffix("piihard")

	storedId := runMutation(t, ctx, eng, "createUserOnFirstLogin", map[string]any{
		"userId":       userId,
		"displayName":  "Quinn Bee",
		"firstName":    "Quinn",
		"lastName":     "Bee",
		"primaryEmail": "quinn.bee@example.com",
		"phone":        "15550002222",
		"primaryRole":  "Tester",
		"gender":       "nonbinary",
		"birthdate":    "1990-01-02",
		"internal":     true,
	})

	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "Quinn", before["firstName"], "fixture sanity: PII populated before hard-delete")
	require.Equal(t, "Tester", before["primaryRole"])

	// The hard-delete: minimal call, only the userId.
	runMutation(t, ctx, eng, "deleteUserHard", map[string]any{
		"userId": userId,
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)

	// The soft-delete marker is set; the row survives for FK integrity.
	require.Equal(t, false, p["active"], "hard-delete must flip active=false")

	// Derive the PII field set from the live schema -- the same source
	// the engine scrub uses -- and assert EVERY one is cleared. This is
	// the generic compliance assertion: it covers any field the user
	// concept marks @pii, including ones added after this test is
	// written.
	userConcept, err := eng.concepts.Get(conceptName)
	require.NoError(t, err)
	piiFields := userConcept.PIIFields()
	require.NotEmpty(t, piiFields, "user concept must declare @pii fields")

	for _, field := range piiFields {
		val, present := p[field]
		if !present {
			continue // absent == cleared
		}
		require.Equal(t, "", val,
			"PII field %q must be scrubbed empty post hard-delete, got %v", field, val)
	}

	// Spot-check the exact fields from the Run-4 evidence.
	for _, field := range []string{"firstName", "lastName", "phone", "primaryRole", "gender", "displayName", "primaryEmail", "birthdate"} {
		require.Equal(t, "", p[field],
			"Run-4 PII field %q must be empty after hard-delete", field)
	}
}
