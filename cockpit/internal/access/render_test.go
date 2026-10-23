package access

import (
	"strings"
	"testing"
)

// Every assertion here is on the EXACT sentence printed. These sentences are
// the whole product for somebody trying to find out why a cluster will not let
// them do something, so changing the words should be a thing done on purpose.

func render(t *testing.T, s Summary, cluster string) string {
	t.Helper()
	var b strings.Builder
	Render(&b, s, cluster)
	return b.String()
}

// The case the epic exists for: a role that is none of the five legacy names.
// Its name, its slug and its rank all appear, and nothing anywhere falls back
// to "reader".
func TestRenderCustomRoleShowsNameSlugAndRank(t *testing.T) {
	out := render(t, Summary{
		UserID:       "u_ada",
		PrimaryEmail: "ada@example.com",
		DisplayName:  "Ada Lovelace",
		SessionID:    "sess_7",
		Role: Role{
			Slug: "release-manager", Name: "Release Manager",
			Rank: 150, HasRank: true, Reported: true,
		},
	}, "acme")

	for _, want := range []string{
		`Your access on "acme"`,
		"Ada Lovelace <ada@example.com>",
		"Release Manager",
		"release-manager",
		"rank 150",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "reader") {
		t.Errorf("output names \"reader\"; a custom role must never fall back to a legacy slug\n--- got ---\n%s", out)
	}
}

// An absent role must read as an ABSENCE with a reason, never as a blank and
// never as "you have no role" -- those are different claims.
func TestRenderSaysWhyTheRoleIsMissingWhenTheClusterDoesNotReportIt(t *testing.T) {
	out := render(t, Summary{
		UserID:       "u_ada",
		PrimaryEmail: "ada@example.com",
		SessionID:    "sess_7",
	}, "acme")

	for _, want := range []string{
		"not reported by this cluster",
		"This cluster does not report roles as data yet.",
		"memql#5181",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "reader") {
		t.Errorf("output names \"reader\"; the cockpit does not guess a role\n--- got ---\n%s", out)
	}
}

// Rank 0 is the design record's "unknown slug": the holder has nothing,
// everywhere, until they are re-roled. Printing a bare "rank 0" buries the one
// fact that explains every refusal they are about to hit.
func TestRenderCallsOutRankZeroAsHoldingNothing(t *testing.T) {
	out := render(t, Summary{
		UserID: "u_ada",
		Role:   Role{Slug: "retired-role", Rank: 0, HasRank: true, Reported: true},
	}, "acme")

	if !strings.Contains(out, "rank 0") {
		t.Errorf("output is missing %q\n--- got ---\n%s", "rank 0", out)
	}
	if !strings.Contains(out, "this role holds no permissions on this cluster") {
		t.Errorf("output does not explain rank 0\n--- got ---\n%s", out)
	}
	// The explanation REPLACES the bare rank line rather than following it.
	// Printing the rank twice reads like two different facts.
	if n := strings.Count(out, "rank 0"); n != 1 {
		t.Errorf("output says \"rank 0\" %d times, want exactly 1\n--- got ---\n%s", n, out)
	}
}

// A PAT carries no session row. The proto is explicit that this is not an
// error, so it must not read like one.
func TestRenderExplainsAnEmptySessionRatherThanShowingABlank(t *testing.T) {
	out := render(t, Summary{UserID: "u_ada", PrimaryEmail: "ada@example.com"}, "acme")

	if !strings.Contains(out, "this credential carries no session") {
		t.Errorf("output does not explain the empty session\n--- got ---\n%s", out)
	}
}
