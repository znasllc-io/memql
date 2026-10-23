package access

import (
	"strings"
	"testing"
)

// The goldens below are the SURFACE, pinned whole rather than sentence by
// sentence. Alignment, blank lines, key order and ordering are the part a
// reader (or a jq script) actually navigates by, and none of it is covered by
// asserting that a phrase appears somewhere. Changing any of it should be a
// deliberate act.
//
// There is a JSON golden as well as a text one, because --json is half the
// product and the half where an absence is easiest to get wrong.

func landedSummary() Summary {
	return Summary{
		UserID: "u_01JQ8ZK", PrimaryEmail: "ada@example.com",
		DisplayName: "Ada Lovelace", SessionID: "sess_01JQ90F",
		Role: Role{
			OnTheWire: true, Reported: true,
			Slug: "release-manager", Name: "Release Manager", Rank: 150, HasRank: true,
		},
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{
			{ID: "g_1", Name: "Platform", Kind: "team", AccountID: "acct_9", AccountName: "Acme Corp"},
			{ID: "g_2", Name: "Release Captains", Kind: "role-group", AccountID: "acct_9", AccountName: "Acme Corp"},
		}},
		Scope: Scope{OnTheWire: true, Reported: true, AccountIDs: []string{"acct_9"}},
	}
}

func pinnedSummary() Summary {
	return Summary{
		UserID: "u_01JQ8ZK", PrimaryEmail: "ada@example.com",
		DisplayName: "Ada Lovelace", SessionID: "sess_01JQ90F",
	}
}

// What every operator sees TODAY, against any cluster at the current pin.
// This is the state the command spends its whole early life in.
const goldenPinnedCluster = `Your access on "acme"

  Signed in as    Ada Lovelace <ada@example.com>
  User id         u_01JQ8ZK
  Session         sess_01JQ90F  (this device)

  Role            not reported by this cluster
  Groups          not reported by this cluster
  Account scope   not reported by this cluster

This cluster does not report roles as data yet. The engine change that adds
the role slug, its name and its rank to MyAccess is memql#5181; until that
lands there is nothing here to show. The cockpit does NOT fall back to the
retired five-value enum: a custom role is none of those five, so a guess
would be wrong in exactly the case this command exists for.

Group membership and account scope arrive with memql#5165.
`

// The same machine once memql#5181 and memql#5165 have landed, holding a
// CUSTOM role -- the case the epic exists for.
const goldenCustomRole = `Your access on "acme"

  Signed in as    Ada Lovelace <ada@example.com>
  User id         u_01JQ8ZK
  Session         sess_01JQ90F  (this device)

  Role            Release Manager
                  release-manager · rank 150
  Groups          Platform (team) · g_1 — Acme Corp
                  Release Captains (role-group) · g_2 — Acme Corp
  Account scope   acct_9
`

// `items` and `account_ids` are present and EMPTY, never null and never
// missing, so `.groups.items[]` works in every state a caller can meet.
const goldenJSONPinned = `{
  "cluster": "acme",
  "user_id": "u_01JQ8ZK",
  "primary_email": "ada@example.com",
  "display_name": "Ada Lovelace",
  "session_id": "sess_01JQ90F",
  "role": {
    "on_wire": false,
    "reported": false
  },
  "groups": {
    "on_wire": false,
    "reported": false,
    "items": []
  },
  "account_scope": {
    "on_wire": false,
    "reported": false,
    "account_ids": []
  }
}
`

const goldenJSONLanded = `{
  "cluster": "acme",
  "user_id": "u_01JQ8ZK",
  "primary_email": "ada@example.com",
  "display_name": "Ada Lovelace",
  "session_id": "sess_01JQ90F",
  "role": {
    "on_wire": true,
    "reported": true,
    "slug": "release-manager",
    "name": "Release Manager",
    "rank": 150
  },
  "groups": {
    "on_wire": true,
    "reported": true,
    "items": [
      {
        "id": "g_1",
        "name": "Platform",
        "kind": "team",
        "account_id": "acct_9",
        "account_name": "Acme Corp"
      },
      {
        "id": "g_2",
        "name": "Release Captains",
        "kind": "role-group",
        "account_id": "acct_9",
        "account_name": "Acme Corp"
      }
    ]
  },
  "account_scope": {
    "on_wire": true,
    "reported": true,
    "account_ids": [
      "acct_9"
    ],
    "every_account": false
  }
}
`

func TestGoldenAgainstAClusterThatDoesNotReportRolesYet(t *testing.T) {
	var b strings.Builder
	Render(&b, pinnedSummary(), "acme")
	assertGolden(t, b.String(), goldenPinnedCluster)
}

func TestGoldenWithACustomRoleGroupsAndScope(t *testing.T) {
	var b strings.Builder
	Render(&b, landedSummary(), "acme")
	assertGolden(t, b.String(), goldenCustomRole)
}

func TestGoldenJSONAgainstAClusterThatDoesNotReportRolesYet(t *testing.T) {
	var b strings.Builder
	if err := RenderJSON(&b, pinnedSummary(), "acme"); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	assertGolden(t, b.String(), goldenJSONPinned)
}

func TestGoldenJSONWithACustomRoleGroupsAndScope(t *testing.T) {
	var b strings.Builder
	if err := RenderJSON(&b, landedSummary(), "acme"); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	assertGolden(t, b.String(), goldenJSONLanded)
}

// assertGolden reports the FIRST divergence and the surrounding lines rather
// than every line after an insertion, which otherwise buries a one-line change
// under a wall of shifted output.
func assertGolden(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := lineAt(gotLines, i), lineAt(wantLines, i)
		if g == w {
			continue
		}
		t.Errorf("first divergence at line %d:\n  got  %q\n  want %q\n\n--- full output ---\n%s", i+1, g, w, got)
		return
	}
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
