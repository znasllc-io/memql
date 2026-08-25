package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
)

// user_preferences_self_scoped_4522_db_test.go -- memql#4522, against a real
// database. The sibling non-DB file pins the mutation's SHAPE; this one pins
// what a round-trip actually leaves on the row, which is the only place the
// deep-merge claim can be checked rather than asserted.

// seededPrefs is the preference state every test here starts from: a sibling
// value, the app-managed pointer, and an ENGAGED kill switch.
const (
	seededAssistantID = "v1:agents:agent:seeded-4522"
	seededLanguage    = "en-GB"
)

// latestUserPrefs reads the preferences block off the newest version of a user
// row. Stored ids are CANONICAL while the create helpers hand back the bare
// short id, and comparing the two spellings is how a read silently finds
// nothing (memql#3967's note on the same trap).
func latestUserPrefs(t *testing.T, ctx context.Context, db *bun.DB, userID string) map[string]any {
	t.Helper()
	canonical := userID
	if len(userID) < len(conceptIdentityUser)+1 || userID[:len(conceptIdentityUser)+1] != conceptIdentityUser+":" {
		canonical = conceptIdentityUser + ":" + userID
	}
	payload := latestPayload(t, ctx, db, conceptIdentityUser, canonical)
	prefs, ok := payload["preferences"].(map[string]any)
	require.True(t, ok, "user %s carries no preferences object", userID)
	return prefs
}

// seedPreferenceFixture creates a user and puts it in the state described
// above, using the REAL paths for both halves: updateUser (@serverOnly, hence
// the internal origin) for the app-managed keys, then toggleComputerUseEnabled
// for the switch -- which also demonstrates that mutation's own deep merge,
// since the sibling keys have to survive it.
func seedPreferenceFixture(t *testing.T, ctx context.Context, eng *MemQLEngine, db *bun.DB, suffix string) (string, context.Context) {
	t.Helper()
	userID := newReader(t, eng, ctx, suffix)
	callerCtx := rowAuthzCallerCtx(userID)

	require.NoError(t, tryMutation(t, auth.ContextWithInternalOrigin(callerCtx), eng, "updateUser", map[string]any{
		"userId": userID,
		"payload": map[string]any{
			"preferences": map[string]any{
				"language":          seededLanguage,
				"activeAssistantId": seededAssistantID,
			},
		},
	}), "fixture seed via updateUser")

	require.NoError(t, tryMutation(t, callerCtx, eng, "toggleComputerUseEnabled", map[string]any{
		"enabled": false,
	}), "engaging the kill switch through its own mutation")

	prefs := latestUserPrefs(t, ctx, db, userID)
	require.Equal(t, false, prefs["computerUseEnabled"], "fixture precondition: the kill switch must be ENGAGED")
	require.Equal(t, seededAssistantID, prefs["activeAssistantId"], "fixture precondition: the pointer must be set")
	require.Equal(t, seededLanguage, prefs["language"], "fixture precondition: a sibling preference must be set")
	return userID, callerCtx
}

// TestUpdateMyPreferences_LeavesProtectedKeysAlone is the headline claim of
// memql#4522: an ordinary preferences save must not disturb the computer-use
// kill switch or the assistant pointer.
//
// The failure this exists to catch is not an error -- it is SILENCE. Without
// @mergeFields the write replaces the preferences block wholesale, the engaged
// switch comes back ABSENT rather than false, and the @default("true") that is
// never applied on write means downstream Go reads the user as having
// computer-use enabled again. Everything reports success.
func TestUpdateMyPreferences_LeavesProtectedKeysAlone(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	userID, callerCtx := seedPreferenceFixture(t, ctx, eng, db, "protected")

	require.NoError(t, tryMutation(t, callerCtx, eng, "updateMyPreferences", map[string]any{
		"theme":         "dark",
		"timezone":      "America/Los_Angeles",
		"cursorTweenMs": 1500,
	}), "an ordinary preferences save must succeed")

	prefs := latestUserPrefs(t, ctx, db, userID)

	// The protected pair, unchanged -- PRESENT and still false, not absent.
	require.Contains(t, prefs, "computerUseEnabled",
		"the kill switch key was ERASED: absent reads as the @default(\"true\") that is never "+
			"applied on write, so a user who disabled computer use has it back on (memql#4522)")
	require.Equal(t, false, prefs["computerUseEnabled"], "the engaged kill switch must stay engaged")
	require.Equal(t, seededAssistantID, prefs["activeAssistantId"],
		"the app-managed assistant pointer must survive an unrelated preferences save (memql#406)")

	// The sibling the call did not carry.
	require.Equal(t, seededLanguage, prefs["language"],
		"a preference the call omitted must keep its stored value -- that is what makes the save partial")

	// And the fields the call DID carry actually landed.
	require.Equal(t, "dark", prefs["theme"])
	require.Equal(t, "America/Los_Angeles", prefs["timezone"])
	require.EqualValues(t, 1500, prefs["cursorTweenMs"])
}

// TestUpdateMyPreferences_TargetsOnlyTheCallersRow: two users, two rows. The
// mutation declares no target argument at all, so this is really asking
// whether actor.userId resolves per-caller rather than being captured once.
func TestUpdateMyPreferences_TargetsOnlyTheCallersRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	userA, ctxA := seedPreferenceFixture(t, ctx, eng, db, "targetA")
	userB, ctxB := seedPreferenceFixture(t, ctx, eng, db, "targetB")

	require.NoError(t, tryMutation(t, ctxA, eng, "updateMyPreferences", map[string]any{"theme": "dark"}))
	require.NoError(t, tryMutation(t, ctxB, eng, "updateMyPreferences", map[string]any{"theme": "light"}))

	require.Equal(t, "dark", latestUserPrefs(t, ctx, db, userA)["theme"])
	require.Equal(t, "light", latestUserPrefs(t, ctx, db, userB)["theme"],
		"each caller must write their OWN row; a shared value here means the target was resolved once")

	// A caller supplying somebody else's id must still only reach their own
	// row. Undeclared args are dropped leniently on the engine path, so this
	// is exactly the shape a stale client takes.
	require.NoError(t, tryMutation(t, ctxB, eng, "updateMyPreferences", map[string]any{
		"theme":  "system",
		"userId": userA,
		"id":     conceptIdentityUser + ":" + userA,
	}))
	require.Equal(t, "dark", latestUserPrefs(t, ctx, db, userA)["theme"],
		"a supplied id must not retarget the write: user A's row was changed by user B")
	require.Equal(t, "system", latestUserPrefs(t, ctx, db, userB)["theme"])
}

// TestUpdateMyPreferences_RefusesOutOfContractValues covers the validation
// side, and asserts the ROW as well as the error: a refusal that half-applied
// would be worse than one that never fired, and only reading the row back can
// tell those apart.
func TestUpdateMyPreferences_RefusesOutOfContractValues(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	userID, callerCtx := seedPreferenceFixture(t, ctx, eng, db, "refusals")

	// A known-good baseline to compare the row against after each refusal.
	require.NoError(t, tryMutation(t, callerCtx, eng, "updateMyPreferences", map[string]any{
		"theme":                "dark",
		"cursorTweenMs":        1000,
		"archiveRetentionDays": 30,
	}))
	before := latestUserPrefs(t, ctx, db, userID)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"theme outside its enum", map[string]any{"theme": "chartreuse"}},
		{"rollover action outside its enum", map[string]any{"dailySpaceRolloverAction": "shred"}},
		{"takeover mode outside its enum", map[string]any{"takeoverMode": "strobe"}},
		{"interactive pace outside its enum", map[string]any{"interactivePace": "instant"}},
		{"voice mode outside its enum", map[string]any{"voiceMode": "telepathy"}},
		{"cursor tween below the range", map[string]any{"cursorTweenMs": 10}},
		{"cursor tween above the range", map[string]any{"cursorTweenMs": 9999}},
		{"retention below the documented window", map[string]any{"archiveRetentionDays": 7}},
		{"retention above the documented window", map[string]any{"archiveRetentionDays": 365}},
		{"language that is not a BCP 47 shape", map[string]any{"language": "not a tag!!"}},
		{"timezone that is not an IANA shape", map[string]any{"timezone": "Somewhere Nice!"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tryMutation(t, callerCtx, eng, "updateMyPreferences", tc.args)
			require.Error(t, err, "%s must be REFUSED, not coerced -- a preference silently "+
				"changed to a value the user did not choose is worse than an error they can see", tc.name)

			after := latestUserPrefs(t, ctx, db, userID)
			require.Equal(t, before, after,
				"a refused write must leave the row untouched; %s changed it", tc.name)
		})
	}
}

// TestUpdateMyPreferences_AcceptsTheDocumentedEdges is the positive control for
// the test above: the bounds are INCLUSIVE, so the endpoints must be accepted.
// Without it, a mutation that refused everything would pass every refusal case.
func TestUpdateMyPreferences_AcceptsTheDocumentedEdges(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	userID, callerCtx := seedPreferenceFixture(t, ctx, eng, db, "edges")

	require.NoError(t, tryMutation(t, callerCtx, eng, "updateMyPreferences", map[string]any{
		"cursorTweenMs":        250,
		"archiveRetentionDays": 30,
		"language":             "en",
		"timezone":             "UTC",
	}), "the low endpoints are inside the documented range and must be accepted")

	require.NoError(t, tryMutation(t, callerCtx, eng, "updateMyPreferences", map[string]any{
		"cursorTweenMs":        2500,
		"archiveRetentionDays": 60,
		"language":             "es-MX",
		"timezone":             "America/Argentina/Buenos_Aires",
	}), "the high endpoints are inside the documented range and must be accepted")

	prefs := latestUserPrefs(t, ctx, db, userID)
	require.EqualValues(t, 2500, prefs["cursorTweenMs"])
	require.EqualValues(t, 60, prefs["archiveRetentionDays"])
	require.Equal(t, "es-MX", prefs["language"])
	require.Equal(t, "America/Argentina/Buenos_Aires", prefs["timezone"])

	// Clearing a text preference back to unset stays possible: the concept
	// documents an empty timezone as "falls back to UTC", so the pattern
	// admits the empty string deliberately.
	require.NoError(t, tryMutation(t, callerCtx, eng, "updateMyPreferences", map[string]any{"timezone": ""}))
	require.Equal(t, "", latestUserPrefs(t, ctx, db, userID)["timezone"])
}
