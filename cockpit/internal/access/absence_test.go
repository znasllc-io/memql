package access

import (
	"strings"
	"testing"
)

// ===========================================================================
// THE ABSENCE HALF
// ===========================================================================
// "An absence is a sentence, never a blank" is the package's whole thesis, and
// every sentence serving it is asserted here. Without these, each one can be
// deleted or reworded with the suite still green -- which is how a blank gets
// back in.
//
// There are TWO absences and they must never render alike: a field this build's
// wire does not carry (no cluster could have sent it), and a field the cluster
// simply did not fill.

func TestRenderExplainsAnUnresolvedUserRow(t *testing.T) {
	out := render(t, Summary{SessionID: "sess_7"}, "acme")
	if !strings.Contains(out, "the cluster resolved no user row for this credential") {
		t.Errorf("output does not explain the missing user row\n--- got ---\n%s", out)
	}
}

// Finding 11: a resolved user with no name and no email is ordinary -- the
// wire documents it -- and saying "no user row" directly above a populated
// "User id" is the report contradicting itself in adjacent rows.
func TestRenderDoesNotDenyAUserRowItIsAboutToPrintTheIdOf(t *testing.T) {
	out := render(t, Summary{UserID: "u_ada"}, "acme")
	if strings.Contains(out, "resolved no user row") {
		t.Errorf("output denies a user row on the line above the id it prints\n--- got ---\n%s", out)
	}
	if !strings.Contains(out, "no name or email") {
		t.Errorf("output does not say the row is thin\n--- got ---\n%s", out)
	}
}

func TestRenderNamesAnEmptyRoleTheClusterActuallySent(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Role:   Role{OnTheWire: true, Reported: true},
	}, "acme")
	if !strings.Contains(out, "the cluster reported an empty role") {
		t.Errorf("output does not name the empty role\n--- got ---\n%s", out)
	}
}

// The two absences, side by side. A cluster whose wire HAS the field and sent
// nothing must not be told to wait for an engine change it already has.
func TestRenderTellsTheTwoAbsencesApart(t *testing.T) {
	pinned := render(t, Summary{UserID: "u"}, "acme")
	if !strings.Contains(pinned, "Role            "+absentFromWire) {
		t.Errorf("a field missing from the wire did not say so\n--- got ---\n%s", pinned)
	}
	if !strings.Contains(pinned, "memql#5181") {
		t.Errorf("the wire-absence does not name the engine change\n--- got ---\n%s", pinned)
	}

	silent := render(t, Summary{
		UserID: "u",
		Role:   Role{OnTheWire: true},
		Groups: Groups{OnTheWire: true},
		Scope:  Scope{OnTheWire: true},
	}, "acme")
	if !strings.Contains(silent, "this cluster sent no role for this credential") {
		t.Errorf("a silent cluster did not get its own sentence\n--- got ---\n%s", silent)
	}
	if strings.Contains(silent, "memql#5181") || strings.Contains(silent, "memql#5165") {
		t.Errorf("a cluster that already has the change was told to wait for it\n--- got ---\n%s", silent)
	}
	if !strings.Contains(silent, "Groups          "+absentFromReply) {
		t.Errorf("groups did not render the reply-absence\n--- got ---\n%s", silent)
	}
	if !strings.Contains(silent, "Account scope   "+absentFromReply) {
		t.Errorf("scope did not render the reply-absence\n--- got ---\n%s", silent)
	}
}

// Finding 6: the note must name only what is actually missing. Told that group
// membership has not arrived while looking at a list of groups, a reader
// cannot trust either statement.
func TestRenderNoteNamesOnlyTheBlocksMissingFromTheWire(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Role:   Role{OnTheWire: true, Reported: true, Slug: "admin"},
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{{ID: "g_1", Name: "Platform"}}},
		// Scope is the only one still missing from the wire.
	}, "acme")

	if !strings.Contains(out, "Account scope arrives with memql#5165.") {
		t.Errorf("the note does not name account scope alone\n--- got ---\n%s", out)
	}
	if strings.Contains(out, "Group membership") {
		t.Errorf("the note claims group membership is missing while printing it\n--- got ---\n%s", out)
	}
}

// D6's staff case. Rendering this as nothing would be the exact inverse of the
// truth, which is why the sentence exists at all.
func TestRenderShowsEveryAccountRatherThanNothing(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Scope:  Scope{OnTheWire: true, Reported: true, EveryAccount: true},
	}, "acme")
	if !strings.Contains(out, "every account on this cluster") {
		t.Errorf("the staff scope did not render\n--- got ---\n%s", out)
	}
}

// Finding 21: --json emits account_ids AND every_account, so the text report
// must not drop the ids. One command must not give two answers about one
// record.
func TestRenderKeepsNamedAccountsAlongsideEveryAccount(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Scope: Scope{
			OnTheWire: true, Reported: true, EveryAccount: true,
			AccountIDs: []string{"acct_1", "acct_2"},
		},
	}, "acme")

	if !strings.Contains(out, "every account on this cluster") {
		t.Errorf("the staff scope did not render\n--- got ---\n%s", out)
	}
	for _, id := range []string{"acct_1", "acct_2"} {
		if !strings.Contains(out, id) {
			t.Errorf("output drops %q, which --json reports\n--- got ---\n%s", id, out)
		}
	}
}

// Finding 22: the id is what a person quotes to whoever administers grants,
// and the only thing that tells two same-named groups apart.
func TestRenderShowsAGroupIDBesideItsName(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{
			{ID: "g_1", Name: "Platform", Kind: "team", AccountName: "Acme Corp"},
		}},
	}, "acme")

	for _, want := range []string{"g_1", "Platform", "team", "Acme Corp"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRenderDoesNotRepeatAGroupIDStandingInForAMissingName(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{{ID: "g_1", Kind: "team"}}},
	}, "acme")

	if n := strings.Count(out, "g_1"); n != 1 {
		t.Errorf("output says %q %d times, want exactly 1\n--- got ---\n%s", "g_1", n, out)
	}
}

// Finding 10: a group with neither id nor name rendered as eighteen spaces,
// and one with only an account rendered as a dangling " — Acme". Both are the
// blank this file's own rule forbids, and both are what a renamed
// MyAccessGroup field would produce.
func TestRenderNeverPrintsABlankOrDanglingGroupLine(t *testing.T) {
	out := render(t, Summary{
		UserID: "u",
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{
			{AccountName: "Acme"},
			{},
		}},
	}, "acme")

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "—") {
			t.Errorf("a group line begins with a dangling em-dash: %q\n--- got ---\n%s", line, out)
		}
	}
	if !strings.Contains(out, "named neither by id nor by name") {
		t.Errorf("a group with no id and no name got no sentence\n--- got ---\n%s", out)
	}
}
