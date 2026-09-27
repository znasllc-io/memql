package memql

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// construct_ladder_write_guard_test.go -- the construct ladder guard's rules,
// without a database (epic memql#5408, issue #5409). The db-gated file drives
// the guard through the whole write path; this one pins what it judges.

// THE GUARD'S TWO LISTS ARE WHAT THE FOUR @serverOnly WRITERS WRITE. They are
// a restatement of dsl/authoring/mutations.memql, and a restated list is wrong
// the day a fifth ladder field is added to recordConstructLadder and not here:
// that field would be writable by any owner, and nothing else would notice.
// Both directions are held, so a field the DSL stops writing does not linger
// here either.
//
// ONE EXEMPTION, AND WHY: recordProcedure also writes `source`, an ordinary
// field of every authored construct, which its owner edits. It is guarded on
// a LEARNED procedure only (constructLearnedSource, pinned below), so it is not
// in the unconditional list.
func TestConstructLadderWriteGuard_FieldListsAreWhatTheServerOnlyMutationsWrite(t *testing.T) {
	templates := loadAllMutationTemplates(t)
	written := func(names ...string) []string {
		t.Helper()
		seen := map[string]bool{}
		for _, name := range names {
			tmpl := templates[name]
			require.NotNil(t, tmpl, "%s is not a mutation in the shipped tree", name)
			require.Equal(t, constructConceptID, tmpl.Concept, "%s writes %s, not a construct", name, tmpl.Concept)
			for _, layer := range []any{tmpl.PayloadTemplate, tmpl.PayloadOverlayTemplate} {
				fields, _ := layer.(map[string]any)
				for field := range fields {
					seen[field] = true
				}
			}
		}
		delete(seen, "id")
		out := make([]string, 0, len(seen))
		for field := range seen {
			out = append(out, field)
		}
		sort.Strings(out)
		return out
	}
	sorted := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return out
	}

	ladder := written("recordProcedure", "recordConstructLadder")
	var withoutSource []string
	for _, field := range ladder {
		if field != "source" {
			withoutSource = append(withoutSource, field)
		}
	}
	require.Equal(t, withoutSource, sorted(constructLadderFields),
		"constructLadderFields must be exactly what recordProcedure and recordConstructLadder write (source aside)")
	require.Equal(t, written("recordConstructGoalSignature", "recordConstructReliability"), sorted(constructLearnedFields),
		"constructLearnedFields must be exactly what recordConstructGoalSignature and recordConstructReliability write")
}

// A CHANGE IS A VALUE, NOT A SPELLING. The read-merge hands the guard a row
// whose values came through JSON on one side and possibly through Go on the
// other, so a number is one value however it was typed and an object is its
// content in any key order; and absent, null and "" are the language's one
// unset value, so none of them is a change from another.
func TestConstructLadderWriteGuard_AChangeIsAValueNotASpelling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prior, final map[string]any
		changed      bool
	}{
		{"absent to absent", map[string]any{}, map[string]any{}, false},
		{"absent to empty", map[string]any{}, map[string]any{"f": ""}, false},
		{"null to empty", map[string]any{"f": nil}, map[string]any{"f": ""}, false},
		{"create with a value", nil, map[string]any{"f": "trusted"}, true},
		{"create with nothing", nil, map[string]any{"f": ""}, false},
		{"set to cleared", map[string]any{"f": "appr-1"}, map[string]any{"f": ""}, true},
		{"cleared to set", map[string]any{"f": ""}, map[string]any{"f": "appr-1"}, true},
		{"same string", map[string]any{"f": "shadow"}, map[string]any{"f": "shadow"}, false},
		{"another string", map[string]any{"f": "shadow"}, map[string]any{"f": "trusted"}, true},
		{"float and int", map[string]any{"f": float64(3)}, map[string]any{"f": 3}, false},
		{"another number", map[string]any{"f": float64(3)}, map[string]any{"f": 4}, true},
		{"zero is a value", map[string]any{}, map[string]any{"f": 0}, true},
		{"object in another order", map[string]any{"f": map[string]any{"a": 1.0, "b": []any{"x"}}}, map[string]any{"f": map[string]any{"b": []any{"x"}, "a": 1}}, false},
		{"object with another member", map[string]any{"f": map[string]any{"a": 1.0}}, map[string]any{"f": map[string]any{"a": 1.0, "b": 2.0}}, true},
		{"an empty object is a value", map[string]any{}, map[string]any{"f": map[string]any{}}, true},
	} {
		if got := constructFieldChanged(tc.prior, tc.final, "f"); got != tc.changed {
			t.Errorf("%s: changed = %v, want %v", tc.name, got, tc.changed)
		}
	}
}

// INTERNAL ORIGIN IS THE ONLY WAY IN, and it is the only thing asked: a
// cluster owner without it is refused like anybody else, and the refusal
// names the field it refused.
func TestConstructLadderWriteGuard_InternalOriginIsTheOnlyWayIn(t *testing.T) {
	learned := map[string]any{"ladder": "shadow", "goalSignature": "sig-1"}
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "an-owner", Role: auth.RoleOwner})

	err := validateConstructLadderServerOnly(owner, learned, map[string]any{"ladder": "trusted", "goalSignature": "sig-1"})
	require.Error(t, err, "a cluster owner moved the rung without internal origin")
	require.True(t, strings.Contains(err.Error(), "`ladder`") && strings.Contains(err.Error(), "construct_ladder_write_guard.go"), err.Error())

	err = validateConstructLadderServerOnly(owner, learned, map[string]any{"ladder": "shadow", "goalSignature": "sig-2"})
	require.ErrorContains(t, err, "`goalSignature`", "on a learned procedure the signature is the ladder's")

	require.NoError(t, validateConstructLadderServerOnly(owner, map[string]any{"goalSignature": "sig-1"}, map[string]any{"goalSignature": "sig-2"}),
		"an authored construct -- no ladder -- is not a learned procedure")
	require.NoError(t, validateConstructLadderServerOnly(auth.ContextWithInternalOrigin(owner), learned, map[string]any{"ladder": "trusted", "goalSignature": "sig-2"}),
		"internal origin is integrations/procedure's own write")
}

// A LEARNED PROCEDURE'S SOURCE IS WHAT A PERSON APPROVES. An owner editing it
// would show one thing under a hash that never changed while the ladder served
// another, so on a learned row it is the ladder's; on an authored construct it
// stays the owner's to edit. Internal origin still writes it (recordProcedure).
func TestConstructLadderWriteGuard_ALearnedProceduresSourceIsTheLadders(t *testing.T) {
	ctx := context.Background()
	learned := map[string]any{"ladder": "trusted", "source": "automation a { }"}
	edited := map[string]any{"ladder": "trusted", "source": "automation a { mutation x() }"}
	err := validateConstructLadderServerOnly(ctx, learned, edited)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "`source`"), "the refusal names the field: %v", err)

	authored := map[string]any{"source": "automation a { }"}
	authoredEdit := map[string]any{"source": "automation a { mutation x() }"}
	require.NoError(t, validateConstructLadderServerOnly(ctx, authored, authoredEdit),
		"an authored construct's source is its owner's to edit")

	require.NoError(t, validateConstructLadderServerOnly(auth.ContextWithInternalOrigin(ctx), learned, edited),
		"recordProcedure writes a learned procedure's source under internal origin")
}
