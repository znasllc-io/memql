package parser

import (
	"strings"
	"testing"
)

// The rank modifiers on the owned tier (epic memql#4832, task memql#4834).
//
// These sit beside the composite tier's own tests rather than replacing
// them: `clusterOwner` is still a modifier, still parses, and still
// formats the same way. What is new is that the modifier LIST is open --
// so the tests below cover the combinations, and just as importantly the
// two combinations that are refused.

func TestParseRowAuthzRankModifiers(t *testing.T) {
	cases := []struct {
		name string
		line string
		want RowAuthzDecl
	}{
		{
			"rank-visible reads",
			`@rowAuthz(owner="ownerUserId", rankVisible)`,
			RowAuthzDecl{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true},
		},
		{
			"rank-visible reads and rank-strict writes",
			`@rowAuthz(owner="ownerUserId", rankVisible, rankStrict)`,
			RowAuthzDecl{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, RankStrict: true},
		},
		{
			"an unowned floor",
			`@rowAuthz(owner="ownerUserId", rankVisible, unowned="developer")`,
			RowAuthzDecl{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, Unowned: "developer"},
		},
		{
			"every modifier at once",
			`@rowAuthz(owner="ownerUserId", rankVisible, rankStrict, unowned="developer", clusterOwner)`,
			RowAuthzDecl{
				Tier: RowAuthzOwned, Owner: "ownerUserId",
				RankVisible: true, RankStrict: true, Unowned: "developer", ClusterOwnerBypass: true,
			},
		},
		{
			// An attribute's argument list is a MAP, so there is no order
			// to depend on. Accepting one spelling and not the other would
			// be a lie about the grammar -- the same reasoning the
			// composite tier records.
			"modifier order does not matter",
			`@rowAuthz(clusterOwner, unowned="developer", rankStrict, rankVisible, owner="ownerUserId")`,
			RowAuthzDecl{
				Tier: RowAuthzOwned, Owner: "ownerUserId",
				RankVisible: true, RankStrict: true, Unowned: "developer", ClusterOwnerBypass: true,
			},
		},
		{
			// The composite must keep parsing to exactly what it always
			// did: the rank flags are additive, and every one of the 16
			// composite declarations in the tree means what it meant.
			"the composite tier is unchanged",
			`@rowAuthz(owner="ownerUserId", clusterOwner)`,
			RowAuthzDecl{Tier: RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRowAuthz(rowAuthzAttr(t, tc.line))
			if err != nil {
				t.Fatalf("ParseRowAuthz(%s) error = %v", tc.line, err)
			}
			if *got != tc.want {
				t.Fatalf("ParseRowAuthz(%s) = %+v, want %+v", tc.line, *got, tc.want)
			}
		})
	}
}

// TestParseRowAuthzRefusesIncoherentRankCombinations pins the two
// combinations that are REFUSED rather than resolved.
//
// Both refusals exist because either resolution would surprise someone:
// rankStrict alone would grant the authority to change a row the same
// caller cannot see, and an unowned floor alone would read as a widening
// that gates nothing. A parser that picks a side on an ambiguous
// AUTHORIZATION statement is the failure this whole file is written
// against.
func TestParseRowAuthzRefusesIncoherentRankCombinations(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantNames string
	}{
		{"rankStrict without rankVisible", `@rowAuthz(owner="ownerUserId", rankStrict)`, "rankVisible"},
		{"an unowned floor without rankVisible", `@rowAuthz(owner="ownerUserId", unowned="developer")`, "rankVisible"},
		{"rankVisible carrying a value", `@rowAuthz(owner="ownerUserId", rankVisible="yes")`, "takes no value"},
		{"an unowned floor with no slug", `@rowAuthz(owner="ownerUserId", rankVisible, unowned="")`, "quoted role slug"},
		{"rank modifiers with no owner field", `@rowAuthz(clusterOwner, rankVisible)`, "exactly one tier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := ParseRowAuthz(rowAuthzAttr(t, tc.line))
			if err == nil {
				t.Fatalf("ParseRowAuthz(%s): want a refusal, got %+v", tc.line, decl)
			}
			if !strings.Contains(err.Error(), tc.wantNames) {
				t.Fatalf("ParseRowAuthz(%s) error = %q, want it to name %q", tc.line, err, tc.wantNames)
			}
			// The shared property every refusal in this file carries: say
			// what IS accepted, or the author is guessing at a security
			// declaration.
			if !strings.Contains(err.Error(), `owner="<field>", clusterOwner`) {
				t.Fatalf("ParseRowAuthz(%s) error = %q, want it to name the accepted forms", tc.line, err)
			}
		})
	}
}

// TestFormatRowAuthzRoundTripsEveryRankCombination is the codemod's half.
// FormatRowAuthz is the only renderer, so anything it can write must read
// back through ParseRowAuthz as the SAME decl -- otherwise the codemod can
// silently rewrite one authorization statement into another.
func TestFormatRowAuthzRoundTripsEveryRankCombination(t *testing.T) {
	// Every reachable combination, enumerated rather than sampled: the
	// modifier set is small enough that "all of them" is cheaper to read
	// than an argument about which ones matter.
	for _, d := range []RowAuthzDecl{
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, RankStrict: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, Unowned: "developer"},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, ClusterOwnerBypass: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankVisible: true, RankStrict: true, Unowned: "admin", ClusterOwnerBypass: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId"},
	} {
		rendered, err := FormatRowAuthz(d)
		if err != nil {
			t.Fatalf("FormatRowAuthz(%+v) error = %v", d, err)
		}
		back, err := ParseRowAuthz(rowAuthzAttr(t, rendered))
		if err != nil {
			t.Fatalf("FormatRowAuthz(%+v) rendered %s, which does not parse: %v", d, rendered, err)
		}
		if *back != d {
			t.Fatalf("round trip: %+v -> %s -> %+v", d, rendered, *back)
		}
	}
}

// TestFormatRowAuthzRefusesWhatItCannotRoundTrip is the negative control
// for the test above, and it is the one that would have caught the
// composite's own near-miss: a renderer that DROPS a modifier emits
// something the parser reads back as a different -- and weaker --
// declaration, with nothing failing.
func TestFormatRowAuthzRefusesWhatItCannotRoundTrip(t *testing.T) {
	for _, d := range []RowAuthzDecl{
		// A rank modifier on a tier that has no owner to rank.
		{Tier: RowAuthzClusterOwner, RankVisible: true},
		{Tier: RowAuthzPublic, RankStrict: true},
		{Tier: RowAuthzGranted, Spec: "spaceMember", Unowned: "developer"},
		// The combinations ParseRowAuthz refuses; the renderer must refuse
		// them too, or it emits a string that does not read back at all.
		{Tier: RowAuthzOwned, Owner: "ownerUserId", RankStrict: true},
		{Tier: RowAuthzOwned, Owner: "ownerUserId", Unowned: "developer"},
	} {
		if rendered, err := FormatRowAuthz(d); err == nil {
			t.Fatalf("FormatRowAuthz(%+v) = %q, want a refusal", d, rendered)
		}
	}
}
