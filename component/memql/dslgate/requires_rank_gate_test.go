package dslgate

import "testing"

// `@requiresRank` as a caller check (epic memql#5165).
//
// The gate's four documented resolutions all assume a construct whose rows
// have an owner or whose caller check can be written into the filter. An
// admin-floored read of an UNOWNED concept has neither, and this is the
// classification that closes that hole -- so these tests pin both halves: that
// an admin-or-above floor satisfies the gate, and that nothing weaker does.

func TestRequiresRankAdminFloorSatisfiesTheUserScopeGate(t *testing.T) {
	src := `@description("one person's memberships")
@requiresRank("admin")
query groupMembership groupsForUser {
  args {
    userId  string!
  }
  filter  row => row.userId == args.userId
  shape   groupMembershipFull
}
`
	violations := ScanFiles([]SourceFile{{Path: "identity/queries.memql", Content: src}}, Options{})
	for _, v := range violations {
		if v.Gate == GateUserScopeSelection {
			t.Fatalf("an admin-floored read was flagged: %s", v.Detail)
		}
	}
}

func TestAWeakerRankFloorDoesNotSatisfyTheUserScopeGate(t *testing.T) {
	// The negative control, and the one that matters: without it the test
	// above would pass against a helper that returned true for every
	// `@requiresRank`, which would turn a writer-floored read of everybody's
	// rows into a construct the gate no longer looks at.
	for _, floor := range []string{"writer", "reader", "viewer", "user"} {
		src := `@requiresRank("` + floor + `")
query groupMembership groupsForUser {
  args {
    userId  string!
  }
  filter  row => row.userId == args.userId
  shape   groupMembershipFull
}
`
		violations := ScanFiles([]SourceFile{{Path: "identity/queries.memql", Content: src}}, Options{})
		found := false
		for _, v := range violations {
			if v.Gate == GateUserScopeSelection {
				found = true
			}
		}
		if !found {
			t.Fatalf("@requiresRank(%q) satisfied the user-scope gate; only admin and above may", floor)
		}
	}
}

func TestAnUnrecognisedRankSlugDoesNotSatisfyTheUserScopeGate(t *testing.T) {
	// A custom role may genuinely outrank admin, and a source scan cannot
	// know: rank is data. Falling through to the flagged bucket is the
	// conservative direction, and the cost is a construct that must state
	// its gate another way.
	src := `@requiresRank("clientManager")
query groupMembership groupsForUser {
  args {
    userId  string!
  }
  filter  row => row.userId == args.userId
  shape   groupMembershipFull
}
`
	violations := ScanFiles([]SourceFile{{Path: "identity/queries.memql", Content: src}}, Options{})
	for _, v := range violations {
		if v.Gate == GateUserScopeSelection {
			return
		}
	}
	t.Fatal("an unresolvable rank slug satisfied the user-scope gate")
}

func TestRequiresRankFloorDetection(t *testing.T) {
	for _, tc := range []struct {
		preamble string
		want     bool
	}{
		{`@requiresRank("admin")`, true},
		{`@requiresRank("developer")`, true},
		{`@requiresRank("owner")`, true},
		{`@actor` + "\n" + `@requiresRank("admin")`, true},
		{`@requiresRank("writer")`, false},
		{`@requiresRank("clientManager")`, false},
		{`@description("mentions requiresRank in prose")`, false},
		{``, false},
	} {
		if got := requiresAdminRankFloor(tc.preamble); got != tc.want {
			t.Fatalf("requiresAdminRankFloor(%q) = %v, want %v", tc.preamble, got, tc.want)
		}
	}
}
