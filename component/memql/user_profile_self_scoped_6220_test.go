package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// user_profile_self_scoped_6220_test.go -- memql#6220.
//
// The Profile's three writes -- updateMyName, setMyAvatar, clearMyAvatar --
// are self-only by construction: each stamps `id: actor.userId` and declares no
// target argument, so pointing one at somebody else is not a permission they
// refuse but a call that cannot be written. These tests keep it structural,
// the way user_preferences_self_scoped_4522_test.go keeps updateMyPreferences.
//
// The photo has a second contract, and it is the one a careless edit would
// break silently: only an INLINE base64 image of the three types the Profile
// produces may be stored. A remote URL would turn every reader's browser into a
// request somewhere else, and an SVG is a document that can carry script.

// profileFn loads one of the shipped mutations out of the real DSL tree.
func profileFn(t *testing.T, name string) *Function {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)

	fn, err := fnRegistry.Get(name)
	require.NoError(t, err, "%s must load from dsl/identity/mutations.memql", name)
	require.NotNil(t, fn.MutationTemplate)
	return fn
}

// webpAvatar is a well-formed photo value: the shape the Profile writes.
const webpAvatar = "data:image/webp;base64,UklGRiQAAABXRUJQVlA4IBgAAAAwAQCdASoBAAEAAwA0JaQAA3AA/vuUAAA="

func profileArgs(name string) map[string]any {
	switch name {
	case "updateMyName":
		return map[string]any{"firstName": "Ada", "lastName": "Lovelace", "displayName": "Ada Lovelace"}
	case "setMyAvatar":
		return map[string]any{"avatar": webpAvatar}
	}
	return map[string]any{}
}

var profileWrites = []string{"updateMyName", "setMyAvatar", "clearMyAvatar"}

// TestProfileWrites_TargetTheCallerOnly: the rendered row is the actor's, and
// a stale client supplying somebody else's id cannot retarget it. Undeclared
// args are dropped leniently on the engine path, so the supplied ids are
// exactly what a retargeting attempt looks like.
func TestProfileWrites_TargetTheCallerOnly(t *testing.T) {
	const callerID = "user-caller-6220"
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: callerID, Role: auth.RoleWriter})
	for _, name := range profileWrites {
		t.Run(name, func(t *testing.T) {
			fn := profileFn(t, name)
			args := profileArgs(name)
			args["userId"] = "user-someone-else"
			args["id"] = "user-someone-else"
			node, err := (&MemQLEngine{}).renderMutationTemplate(ctx, fn.MutationTemplate, args)
			require.NoError(t, err, "an empty id here would mean the mutation is inert, not that it is safe")
			require.Equal(t, callerID, node.ID, "%s must target the CALLER's row; %q means a supplied id retargets it", name, node.ID)
		})
	}
}

// With no actor the target must come back EMPTY, so the update fails loudly
// rather than selecting some row.
func TestProfileWrites_NoActorYieldsNoTarget(t *testing.T) {
	for _, name := range profileWrites {
		t.Run(name, func(t *testing.T) {
			fn := profileFn(t, name)
			args := profileArgs(name)
			args["userId"] = "user-someone-else"
			node, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), fn.MutationTemplate, args)
			if err == nil {
				require.Empty(t, node.ID, "with no actor %s must select no row; %q is fail-open", name, node.ID)
			}
		})
	}
}

// TestProfileWrites_DeclareNoTargetArg reads the same claim off the args
// schema, so an author who adds a target ARGUMENT trips a test naming the
// reason rather than discovering it through the render.
func TestProfileWrites_DeclareNoTargetArg(t *testing.T) {
	for _, name := range profileWrites {
		fn := profileFn(t, name)
		if fn.ArgsSchema == nil {
			continue
		}
		for _, f := range fn.ArgsSchema.Fields {
			if f == nil {
				continue
			}
			require.NotContains(t, []string{"userId", "id", "targetUserId"}, f.Name,
				"%s: a caller-supplied target defeats the self-scoping; the row comes from actor.userId", name)
			require.NotContains(t, []string{"role", "primaryEmail", "signInPolicy", "revocationEpoch"}, f.Name,
				"%s: the Profile writes a name and a photo, never an account's authority or identity", name)
		}
	}
}

// TestUpdateMyName_WritesTheThreeNameFields pins what lands on the row: the
// two JWT name claims and the display name, and nothing else.
func TestUpdateMyName_WritesTheThreeNameFields(t *testing.T) {
	fn := profileFn(t, "updateMyName")
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-caller-6220", Role: auth.RoleWriter})
	node, err := (&MemQLEngine{}).renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"firstName": "Ada", "displayName": "Ada", "role": "owner",
	})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &payload))
	require.Equal(t, "Ada", payload["firstName"])
	require.Equal(t, "Ada", payload["displayName"])
	require.Equal(t, "", payload["lastName"], "an omitted family name clears it rather than keeping a stale one")
	require.NotContains(t, payload, "role", "a supplied role must not reach the row")
}

// TestUpdateMyName_RefusesABlankName: a whitespace-only name is refused, not
// trimmed into an empty one that every surface would then render as nothing.
func TestUpdateMyName_RefusesABlankName(t *testing.T) {
	validate := profileFn(t, "updateMyName").SnapshotArgumentValidator()
	require.NoError(t, validate(map[string]any{"firstName": "Ada", "lastName": "", "displayName": "Ada"}))
	require.NoError(t, validate(map[string]any{"firstName": "Ada", "displayName": "Ada Lovelace"}), "the family name is optional")

	for label, args := range map[string]map[string]any{
		"blank first name":      {"firstName": "   ", "displayName": "Ada"},
		"blank display name":    {"firstName": "Ada", "displayName": " \t "},
		"no first name":         {"displayName": "Ada"},
		"first name too long":   {"firstName": strings.Repeat("a", 81), "displayName": "Ada"},
		"display name too long": {"firstName": "Ada", "displayName": strings.Repeat("a", 161)},
	} {
		require.Error(t, validate(args), "%s must be refused", label)
	}
}

// TestSetMyAvatar_AdmitsOnlyAnInlineImage is the photo's contract.
func TestSetMyAvatar_AdmitsOnlyAnInlineImage(t *testing.T) {
	validate := profileFn(t, "setMyAvatar").SnapshotArgumentValidator()

	for _, ok := range []string{
		webpAvatar,
		"data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ==",
		"data:image/png;base64,iVBORw0KGgo=",
		"data:image/webp;base64," + strings.Repeat("A", 40000-len("data:image/webp;base64,")),
	} {
		require.NoError(t, validate(map[string]any{"avatar": ok}), "a photo the Profile produces must be accepted: %.40s", ok)
	}

	for label, bad := range map[string]string{
		"remote URL":                     "https://example.com/me.png",
		"protocol-relative URL":          "//example.com/me.png",
		"SVG, which can carry script":    "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=",
		"script URL":                     "javascript:alert(1)",
		"a type the Profile never makes": "data:image/gif;base64,R0lGODlhAQABAA==",
		"not base64":                     "data:image/png;base64,<img src=x>",
		"a newline smuggled in":          "data:image/png;base64,iVBORw0K\nGgo=",
		"not base64-encoded":             "data:image/png,iVBORw0KGgo=",
		"empty":                          "",
		"one character over the bound":   "data:image/webp;base64," + strings.Repeat("A", 40001-len("data:image/webp;base64,")),
	} {
		require.Error(t, validate(map[string]any{"avatar": bad}), "%s must be refused", label)
	}
}
