package registration

import (
	"sort"
	"testing"
)

// shared_mailbox_test.go -- the pin memql#4304's design asks for.
//
// The exact membership of the list is not load-bearing and never could be:
// no list of local parts identifies every shared inbox, and the design says
// so (the flag is a HINT that blocks nothing). What the pin is for is that a
// silent edit changes what the product TELLS PEOPLE about their own security
// posture -- so adding or removing a name should be a visible diff somebody
// chose, not a drive-by.
//
// The false-positive cases below are the part that actually matters. A hint
// that is wrong about a real person is worse than a hint that misses some
// team inboxes: the miss costs nothing, and the false positive costs trust in
// every other thing the page says.

func TestSharedMailboxListIsPinned(t *testing.T) {
	want := []string{
		// RFC 2142: the mailboxes an organisation is expected to run.
		"abuse", "hostmaster", "info", "marketing", "noc", "postmaster",
		"sales", "security", "support", "webmaster", "www",
		// The names organisations actually pick for a shared inbox.
		"admin", "billing", "contact", "dev", "finance", "hello", "hr",
		"it", "legal", "no-reply", "noreply", "office", "ops", "team",
	}
	got := SharedMailboxLocalParts()

	sortedWant := append([]string(nil), want...)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedWant)
	sort.Strings(sortedGot)

	if len(sortedGot) != len(sortedWant) {
		t.Fatalf("the shared-mailbox list has %d entries, want %d.\n got: %v\nwant: %v\n\n"+
			"Adding or removing a name changes what the product tells people about their own "+
			"account. Update this list in the same commit and the diff records the choice.",
			len(sortedGot), len(sortedWant), sortedGot, sortedWant)
	}
	for i := range sortedWant {
		if sortedGot[i] != sortedWant[i] {
			t.Errorf("list entry %d is %q, want %q", i, sortedGot[i], sortedWant[i])
		}
	}
}

func TestLooksLikeSharedMailbox(t *testing.T) {
	cases := []struct {
		email string
		want  bool
		why   string
	}{
		{"support@acme.test", true, "an RFC 2142 role name"},
		{"team@acme.test", true, "the commonest team-inbox name there is"},
		{"NoReply@ACME.test", true, "case folds"},
		{"support+eu@acme.test", true, "a +tag does not make it personal"},

		{"jane@acme.test", false, "an ordinary person"},

		// THE FALSE-POSITIVE CASES. Every one of these is a real name that a
		// substring or prefix test would flag, telling a person their own
		// account looks like a shared mailbox. Exact matching on the local
		// part is what keeps them out, and it is the reason the matcher is
		// written that way rather than as a `strings.Contains` loop.
		{"itzhak@acme.test", false, "contains `it`"},
		{"devon@acme.test", false, "starts with `dev`"},
		{"winfo@acme.test", false, "contains `info`"},
		{"sales.rodriguez@acme.test", false, "starts with `sales`"},
		{"opsahl@acme.test", false, "starts with `ops`"},
		{"legalese@acme.test", false, "starts with `legal`"},

		{"", false, "not an address"},
		{"nodomain", false, "not an address"},
		{"@acme.test", false, "no local part"},
		{"trailing@", false, "no domain"},
	}
	for _, tc := range cases {
		if got := LooksLikeSharedMailbox(tc.email); got != tc.want {
			t.Errorf("LooksLikeSharedMailbox(%q) = %v, want %v (%s)", tc.email, got, tc.want, tc.why)
		}
	}
}
