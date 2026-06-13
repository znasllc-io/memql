package memql

import (
	"reflect"
	"testing"
)

// TestUnionPreservingOrder_MergesMissingCanonical pins the core
// merge-not-clobber contract: canonical skills missing from the
// existing set are appended (in canonical order) while existing skills
// keep their position. This is the behavior the #1443 reconcile relies
// on to backfill todos/notes/calendar without disturbing the operator
// suite a user's assistant already carries.
func TestUnionPreservingOrder_MergesMissingCanonical(t *testing.T) {
	existing := []string{"copresent-takeover", "copresent-ui", "workbench-baseline"}
	canonical := []string{
		"copresent-takeover", "copresent-guide", "copresent-ui",
		"copresent-canvas", "workbench-baseline", "operator-computer-use",
		"delegation-baseline", "todos", "notes", "calendar",
	}

	merged, added := unionPreservingOrder(existing, canonical)

	// Existing skills keep their leading position + order.
	if got := merged[:3]; !reflect.DeepEqual(got, existing) {
		t.Fatalf("existing skills not preserved at front: got %v, want %v", got, existing)
	}
	// Only the missing canonical ids are reported as added.
	wantAdded := []string{
		"copresent-guide", "copresent-canvas", "operator-computer-use",
		"delegation-baseline", "todos", "notes", "calendar",
	}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %v, want %v", added, wantAdded)
	}
	// Every canonical id ends up present exactly once.
	assertContainsExactlyOnce(t, merged, canonical)
}

// TestUnionPreservingOrder_Idempotent pins idempotency: a second pass
// over an already-merged set adds nothing and produces an identical
// list. This is what makes the boot-time reconcile safe to run on
// every cluster start.
func TestUnionPreservingOrder_Idempotent(t *testing.T) {
	canonical := []string{"todos", "notes", "calendar", "workbench-baseline"}
	existing := []string{"workbench-baseline", "todos", "notes", "calendar"}

	merged, added := unionPreservingOrder(existing, canonical)
	if len(added) != 0 {
		t.Fatalf("expected no additions for an already-complete set; added %v", added)
	}

	// Re-run over the result -- still no change.
	merged2, added2 := unionPreservingOrder(merged, canonical)
	if len(added2) != 0 {
		t.Fatalf("second pass added %v, want none", added2)
	}
	if !reflect.DeepEqual(merged, merged2) {
		t.Fatalf("reconcile not idempotent: %v != %v", merged, merged2)
	}
}

// TestUnionPreservingOrder_PreservesUserAddedSkills ensures a skill the
// user (or planner) attached that is NOT in the canonical set survives
// the merge -- the reconcile is additive, never a clobber.
func TestUnionPreservingOrder_PreservesUserAddedSkills(t *testing.T) {
	existing := []string{"copresent-ui", "user-custom-specialty", "todos"}
	canonical := []string{"todos", "notes", "calendar"}

	merged, added := unionPreservingOrder(existing, canonical)

	found := false
	for _, s := range merged {
		if s == "user-custom-specialty" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user-added skill was dropped during merge: %v", merged)
	}
	// notes + calendar added; todos already present.
	wantAdded := []string{"notes", "calendar"}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added = %v, want %v", added, wantAdded)
	}
}

// TestUnionPreservingOrder_DedupesExisting guards against a row whose
// stored skillIds already contains duplicates (or blanks): the merge
// emits each id once and drops empties.
func TestUnionPreservingOrder_DedupesExisting(t *testing.T) {
	existing := []string{"todos", "todos", "", "notes"}
	canonical := []string{"todos", "notes", "calendar"}

	merged, added := unionPreservingOrder(existing, canonical)

	want := []string{"todos", "notes", "calendar"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	if !reflect.DeepEqual(added, []string{"calendar"}) {
		t.Fatalf("added = %v, want [calendar]", added)
	}
}

// TestIsAssistantAgentPayload pins the per-user-Assistant classifier
// used to pick reconcile targets: roleSlug "assistant" (canonical),
// legacy role "assistant", or the canonical id shape + kind. Excludes
// soft-deleted rows and unrelated agents.
func TestIsAssistantAgentPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name:    "roleSlug assistant",
			payload: map[string]any{"roleSlug": "assistant"},
			want:    true,
		},
		{
			name:    "legacy role assistant, no roleSlug",
			payload: map[string]any{"role": "assistant"},
			want:    true,
		},
		{
			name:    "canonical id + kind, no slug/role",
			payload: map[string]any{"id": "assistant-1c65cb0c", "kind": "assistant"},
			want:    true,
		},
		{
			name:    "specialist row excluded",
			payload: map[string]any{"roleSlug": "accounting-finance", "kind": "specialist"},
			want:    false,
		},
		{
			name:    "soft-deleted assistant excluded",
			payload: map[string]any{"roleSlug": "assistant", "deleted": true},
			want:    false,
		},
		{
			name:    "assistant-prefixed id but specialist kind excluded",
			payload: map[string]any{"id": "assistant-x", "kind": "specialist"},
			want:    false,
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAssistantAgentPayload(tc.payload); got != tc.want {
				t.Errorf("isAssistantAgentPayload(%v) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// TestCanonicalAssistantSkillIds_ReadsSeed verifies the canonical set
// is sourced from the live `assistant` SeedDefinition (single source of
// truth) and that it carries the three #1443 personal-productivity
// skills plus the operator suite. The seed body is built by hand here
// to keep the test independent of the DSL loader; the field shape
// mirrors what the parser produces (capabilities is a nested block).
func TestCanonicalAssistantSkillIds_ReadsSeed(t *testing.T) {
	reg := NewSeedRegistry()
	caps := newSeedBlock()
	caps.fields["skillIds"] = seedValue{
		kind: seedStringArray,
		stringsV: []string{
			"copresent-takeover", "copresent-guide", "copresent-ui",
			"copresent-canvas", "workbench-baseline", "operator-computer-use",
			"delegation-baseline", "todos", "notes", "calendar",
		},
	}
	body := newSeedBlock()
	body.nested["capabilities"] = caps

	if err := reg.Upsert(&SeedDefinition{
		Name:       "assistant",
		Scope:      "perUser",
		UseConcept: "agent",
		Body:       body,
	}); err != nil {
		t.Fatalf("seed registry upsert: %v", err)
	}

	m := &SeedMaterializer{registry: reg}
	got := m.canonicalAssistantSkillIds()

	for _, want := range []string{"todos", "notes", "calendar"} {
		found := false
		for _, s := range got {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("canonical skillIds missing %q: %v", want, got)
		}
	}
	if len(got) != 10 {
		t.Errorf("canonical skillIds count = %d, want 10: %v", len(got), got)
	}
}

// TestCanonicalAssistantSkillIds_AbsentSeed returns nil (not a panic /
// error) when the assistant seed isn't registered -- the reconcile
// then no-ops, which is the documented behavior for a DSL tree without
// the assistant seed.
func TestCanonicalAssistantSkillIds_AbsentSeed(t *testing.T) {
	m := &SeedMaterializer{registry: NewSeedRegistry()}
	if got := m.canonicalAssistantSkillIds(); got != nil {
		t.Errorf("expected nil canonical set with no assistant seed, got %v", got)
	}
}

// TestReconcileCapsTransform_PreservesSiblingFlags pins the
// clobber-safety contract: mutationUpdateAgent does top-level replace
// (no @mergeFields), so the reconcile must send the FULL capabilities
// object with only skillIds changed. This test mirrors the exact
// copy-then-overwrite transform the reconcile loop performs and asserts
// every sibling capability flag survives while skillIds gains the
// canonical additions.
func TestReconcileCapsTransform_PreservesSiblingFlags(t *testing.T) {
	// A decoded prior row's capabilities, as it would come off the DB.
	caps := map[string]any{
		"avatar":       true,
		"lipSync":      true,
		"vision":       true,
		"voiceToVoice": true,
		"claw":         false,
		"tools":        []any{},
		"domains":      []any{},
		"keywords":     []any{},
		"skillIds":     []any{"copresent-ui", "workbench-baseline"},
	}
	canonical := []string{
		"copresent-ui", "workbench-baseline", "todos", "notes", "calendar",
	}

	// --- the exact transform from reconcileAssistantSkills ---
	existing := stringSliceFromAny(caps["skillIds"])
	merged, added := unionPreservingOrder(existing, canonical)
	newCaps := make(map[string]any, len(caps))
	for k, v := range caps {
		newCaps[k] = v
	}
	newCaps["skillIds"] = toAnySlice(merged)
	// --------------------------------------------------------

	if !reflect.DeepEqual(added, []string{"todos", "notes", "calendar"}) {
		t.Fatalf("added = %v, want [todos notes calendar]", added)
	}
	// Every sibling flag preserved.
	for _, k := range []string{"avatar", "lipSync", "vision", "voiceToVoice"} {
		if newCaps[k] != true {
			t.Errorf("sibling capability %q not preserved: %v", k, newCaps[k])
		}
	}
	if newCaps["claw"] != false {
		t.Errorf("claw flag not preserved: %v", newCaps["claw"])
	}
	// skillIds carries the merged set.
	gotSkills := stringSliceFromAny(newCaps["skillIds"])
	if !reflect.DeepEqual(gotSkills, []string{"copresent-ui", "workbench-baseline", "todos", "notes", "calendar"}) {
		t.Errorf("merged skillIds = %v", gotSkills)
	}
	// The original caps map is untouched (defensive copy).
	if origSkills := stringSliceFromAny(caps["skillIds"]); len(origSkills) != 2 {
		t.Errorf("original caps mutated: skillIds = %v", origSkills)
	}
}

// assertContainsExactlyOnce fails if any id in want is missing from
// got, or appears more than once.
func assertContainsExactlyOnce(t *testing.T, got, want []string) {
	t.Helper()
	counts := map[string]int{}
	for _, s := range got {
		counts[s]++
	}
	for _, w := range want {
		switch counts[w] {
		case 1:
			// ok
		case 0:
			t.Errorf("merged set missing %q: %v", w, got)
		default:
			t.Errorf("merged set has %q %d times (want 1): %v", w, counts[w], got)
		}
	}
}
