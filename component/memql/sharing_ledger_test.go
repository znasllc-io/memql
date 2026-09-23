package memql

import (
	"strings"
	"testing"
)

func TestTheLedgerCarriesNoPromptContent(t *testing.T) {
	// THE PROMISE OF D6, ASSERTED RATHER THAN REVIEWED. Somebody who offers
	// their Mac Studio to the team is entitled to know it is being used and NOT
	// entitled to read what it was used for.
	//
	// The walk is over the RENDERED ROW rather than over the struct, so a field
	// smuggled in through an `any` -- a nested map, a slice of objects -- is
	// caught too. Every leaf must be a count, a machine id, a week, or one of
	// the four level words.
	entry := FoldLedger("v1:worker:registration:m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "v1:worker:registration:m1", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
		{MachineId: "v1:worker:registration:m1", ActingUserId: "bob", Level: "reasoning", Week: "2026-W36"},
	})
	row := entry.Row()

	// otherCalls, otherPeople and systemCalls are the owner-approved split of
	// epic memql#5344 (design G4): still counts, split by WHO THE CALL WAS FOR
	// -- the owner, somebody else, or the cluster's own work.
	allowed := map[string]bool{"machineId": true, "week": true, "calls": true, "people": true, "byLevel": true,
		"otherCalls": true, "otherPeople": true, "systemCalls": true}
	for key := range row {
		if !allowed[key] {
			t.Fatalf("the ledger grew a field %q; a shared machine's owner sees counts and levels and nothing else", key)
		}
	}

	// No value anywhere in the row may be a caller's id. Counting people is the
	// promise; naming them would turn a usage figure into a surveillance
	// surface on hardware somebody volunteered.
	for _, forbidden := range []string{"alice", "bob"} {
		if strings.Contains(renderAll(row), forbidden) {
			t.Fatalf("the ledger must count people and never name them; %q reached the row: %#v", forbidden, row)
		}
	}
}

// renderAll flattens a row to one string so the assertion above can look at
// every leaf, however deeply a future field nests it.
func renderAll(v any) string {
	switch t := v.(type) {
	case map[string]any:
		var b strings.Builder
		for k, val := range t {
			b.WriteString(k)
			b.WriteString("\x00")
			b.WriteString(renderAll(val))
		}
		return b.String()
	case []any:
		var b strings.Builder
		for _, item := range t {
			b.WriteString(renderAll(item))
		}
		return b.String()
	case string:
		return t
	default:
		return ""
	}
}

func TestTheFoldReadsOnlyTheAllowlistedKeys(t *testing.T) {
	// A decision record carries a great deal this ledger must not see, and the
	// list will grow. Reading four named keys means a field added later is not
	// merely unrendered -- it is unread, so nobody has to remember to exclude
	// it.
	row := map[string]any{
		"machineId":    "m1",
		"actingUserId": "alice",
		"level":        "fast",
		"week":         "2026-W36",
		// Everything below is what a real decision record also carries.
		"prompt":       "the user's actual question",
		"completion":   "the model's actual answer",
		"errorMessage": "a path on somebody's disk",
	}
	got := LedgerCallFromRow(row)
	if got.MachineId != "m1" || got.ActingUserId != "alice" || got.Level != "fast" || got.Week != "2026-W36" {
		t.Fatalf("the four keys must be read: %+v", got)
	}
	if len(LedgerRowKeys()) != 4 {
		t.Fatalf("the allowlist must stay at four keys, got %v", LedgerRowKeys())
	}
}

func TestPeopleAreCountedNotNamed(t *testing.T) {
	entry := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m1", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
		{MachineId: "m1", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
		{MachineId: "m1", ActingUserId: "bob", Level: "strong", Week: "2026-W36"},
	})
	if entry.Calls != 3 {
		t.Fatalf("calls = %d, want 3", entry.Calls)
	}
	if entry.People != 2 {
		t.Fatalf("people = %d, want 2 -- alice twice is one person", entry.People)
	}
	if entry.ByLevel["fast"] != 2 || entry.ByLevel["strong"] != 1 {
		t.Fatalf("byLevel = %v", entry.ByLevel)
	}
}

func TestTheOwnersOwnCallsAreCounted(t *testing.T) {
	// The figure answers "how busy has this machine been", not "how much have I
	// lent it out". A ledger that excluded the owner would show zero on a
	// machine they use constantly, and its owner would conclude sharing had
	// done nothing.
	entry := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m1", ActingUserId: "olivia", Level: "fast", Week: "2026-W36"},
	})
	if entry.Calls != 1 || entry.People != 1 || entry.OtherCalls != 0 || entry.SystemCalls != 0 {
		t.Fatalf("got %+v", entry)
	}
}

func TestAnotherMachinesCallsAreNotCounted(t *testing.T) {
	entry := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m2", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
	})
	if entry.Calls != 0 {
		t.Fatalf("calls = %d, want 0", entry.Calls)
	}
}

func TestAnotherWeeksCallsAreNotCounted(t *testing.T) {
	entry := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m1", ActingUserId: "alice", Level: "fast", Week: "2026-W35"},
	})
	if entry.Calls != 0 {
		t.Fatalf("calls = %d, want 0", entry.Calls)
	}
}

func TestTheSentenceSaysNothingAboutWhoOrWhat(t *testing.T) {
	// One line, in words, because the figure is for a person deciding whether
	// to keep lending rather than for a dashboard. It COUNTS other people and
	// never names them; with a one-person share the count implies that person,
	// which is inherent to any count -- the owner chose the split knowing it
	// (epic memql#5344, design G4).
	empty := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", nil).Sentence()
	if !strings.Contains(empty, "No calls") {
		t.Fatalf("got %q", empty)
	}

	one := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m1", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
	}).Sentence()
	if !strings.Contains(one, "1 call") || strings.Contains(one, "alice") {
		t.Fatalf("got %q", one)
	}

	many := FoldLedger("m1", "v1:identity:user:olivia", "2026-W36", []LedgerCall{
		{MachineId: "m1", ActingUserId: "alice", Level: "fast", Week: "2026-W36"},
		{MachineId: "m1", ActingUserId: "bob", Level: "fast", Week: "2026-W36"},
	}).Sentence()
	if !strings.Contains(many, "2 other people") {
		t.Fatalf("got %q", many)
	}
	if strings.Contains(many, "alice") || strings.Contains(many, "bob") {
		t.Fatalf("the sentence must never name a caller, got %q", many)
	}
}

func TestTheLedgerSentenceSplitsYouFromOthers(t *testing.T) {
	// Design G4, the whole table. The owner's own calls are theirs, a call with
	// no acting person is the cluster's own work, and everybody else is
	// counted -- in calls and in distinct people -- and never named.
	const owner = "v1:identity:user:olivia"
	call := func(user string) LedgerCall {
		return LedgerCall{MachineId: "m", ActingUserId: user, Level: "fast", Week: "2026-W39"}
	}
	repeat := func(n int, user string) []LedgerCall {
		out := make([]LedgerCall, n)
		for i := range out {
			out[i] = call(user)
		}
		return out
	}
	join := func(parts ...[]LedgerCall) []LedgerCall {
		var out []LedgerCall
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name  string
		calls []LedgerCall
		want  string
	}{
		{"none", nil, "No calls have run on this machine this week."},
		{"one, yours", repeat(1, "olivia"), "Served 1 call this week, for you."},
		{"all yours", repeat(41, owner), "Served 41 calls this week, all of them yours."},
		{"mixed", join(repeat(29, owner), repeat(6, "ana"), repeat(6, "bo")), "Served 41 calls this week, 12 of them for 2 other people."},
		{"mixed, one other", join(repeat(29, owner), repeat(12, "ana")), "Served 41 calls this week, 12 of them for 1 other person."},
		{"mixed with system", join(repeat(26, owner), repeat(12, "ana"), repeat(3, "")), "Served 41 calls this week, 12 of them for 1 other person and 3 for the cluster's own work."},
		{"yours and system", join(repeat(38, owner), repeat(3, "")), "Served 41 calls this week, 3 of them for the cluster's own work."},
		{"all others", join(repeat(6, "ana"), repeat(6, "bo")), "Served 12 calls this week, all for 2 other people."},
		{"one other", repeat(1, "ana"), "Served 1 call this week, for 1 other person."},
		{"all system", repeat(5, ""), "Served 5 calls this week, all for the cluster's own work."},
		{"one, system", repeat(1, ""), "Served 1 call this week, for the cluster's own work."},
		{"others and system", join(repeat(12, "ana"), repeat(3, "")), "Served 15 calls this week: 12 for 1 other person and 3 for the cluster's own work."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FoldLedger("m", owner, "2026-W39", tc.calls).Sentence(); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestTheSplitCountsAreRendered(t *testing.T) {
	entry := FoldLedger("m", "v1:identity:user:olivia", "2026-W39", []LedgerCall{
		{MachineId: "m", ActingUserId: "olivia", Level: "fast", Week: "2026-W39"},
		{MachineId: "m", ActingUserId: "ana", Level: "fast", Week: "2026-W39"},
		{MachineId: "m", ActingUserId: "ana", Level: "strong", Week: "2026-W39"},
		{MachineId: "m", ActingUserId: "bo", Level: "fast", Week: "2026-W39"},
		{MachineId: "m", ActingUserId: "", Level: "fast", Week: "2026-W39"},
	})
	if entry.Calls != 5 || entry.OtherCalls != 3 || entry.OtherPeople != 2 || entry.SystemCalls != 1 || entry.People != 3 {
		t.Fatalf("got %+v", entry)
	}
	row := entry.Row()
	if row["otherCalls"] != 3 || row["otherPeople"] != 2 || row["systemCalls"] != 1 {
		t.Fatalf("row = %#v", row)
	}
}
