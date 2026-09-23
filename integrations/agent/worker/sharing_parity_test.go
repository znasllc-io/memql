//go:build agent

package worker

// EVERY SHARING REFUSAL IS COUNTED, NEVER NAMED (epic memql#5344, design G12).
//
// component/worker writes the sentence a machine is refused with, and
// component/memql decides whether that sentence is about SOMEBODY ELSE'S
// machine -- which it then aggregates to a count rather than handing the
// caller a list of other people's registration ids (epic memql#5327, D12).
// The two packages cannot import each other (worker reaches identity, which
// reaches memql), so the classifier matches on words. A new refusal sentence
// that the classifier did not recognise would slip back into the named list:
// exactly the enumeration D12 closed, reopened by a copy edit.
//
// This package imports both, so this is where the two are held together. It
// walks every mode, both cockpit answers and silence, and a listed and an
// unlisted person, and asks the classifier about every non-empty sentence.

import (
	"context"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestEverySharingRefusalIsCountedNeverNamed(t *testing.T) {
	noGroups := func(context.Context, string) []string { return nil }
	shares := []workerservice.Sharing{
		{Mode: workerservice.SharingModeOwner},
		{Mode: workerservice.SharingModeCluster},
		{Mode: workerservice.SharingModePeople, UserIds: []string{"ana"}},
		{Mode: workerservice.SharingModePeople, GroupIds: []string{"design"}},
	}
	serves := []string{workerservice.InferenceServeOwner, workerservice.InferenceServeCluster, ""}
	checked := 0
	check := func(where, why string) {
		t.Helper()
		if why == "" {
			return
		}
		checked++
		if !memqlengine.IsForeignShareRefusal(why) {
			t.Errorf("%s: %q is not recognised as a sharing refusal, so it would reach a caller as a "+
				"NAMED line carrying another user's machine id (memql#5327, D12)", where, why)
		}
	}
	for _, s := range shares {
		for _, serve := range serves {
			check("SharingRefusal", workerservice.SharingRefusal(s.Mode, serve))
			check("SystemRefusal", workerservice.SystemRefusal(s, serve))
			for _, user := range []string{"ana", "bo"} {
				check("PersonRefusal", workerservice.PersonRefusal(s, serve, workerservice.NewPerson(context.Background(), user, noGroups)))
			}
		}
	}
	// A walk that checked nothing would pass forever.
	if checked < 10 {
		t.Fatalf("checked only %d refusal sentences; the walk is not reaching the refusals it exists for", checked)
	}
}

// THE TWO READERS OF ONE CONSENT AGREE (epic memql#5344). component/memql
// reads a stored share for the builtins (ParseMachineSharing) and
// component/worker reads it for routing (SharingFromRow), and the import edge
// runs worker -> memql, so neither can call the other. Two readers that
// disagreed would let the dialog offer a machine as lent to somebody the
// router refuses -- or worse, the other way round. The fixture walks every
// shape a stored block can take, including the ones that must read as owner.
func TestTheTwoReadersOfAStoredShareAgree(t *testing.T) {
	rows := []any{
		nil,
		map[string]any{},
		map[string]any{"mode": "owner"},
		map[string]any{"mode": "cluster"},
		map[string]any{"mode": "cluster ", "userIds": []any{"ana"}},
		map[string]any{"mode": "Cluster"},
		map[string]any{"mode": "people"},
		map[string]any{"mode": "people", "userIds": []any{}},
		map[string]any{"mode": "people", "userIds": []any{" ana ", "", "v1:identity:user:ana", "bo"}},
		map[string]any{"mode": "people", "groupIds": []any{"v1:identity:group:design", "design"}},
		map[string]any{"mode": "people", "userIds": []any{"ana"}, "groupIds": []any{"g"}},
		map[string]any{"mode": "everyone", "userIds": []any{"ana"}},
		"not a map",
	}
	for i, row := range rows {
		want := workerservice.SharingFromRow(row)
		mode, users, groups := memqlengine.ParseMachineSharing(row)
		if mode != want.Mode || !sameList(users, want.UserIds) || !sameList(groups, want.GroupIds) {
			t.Errorf("row %d %#v:\n  memql  reads mode=%q users=%v groups=%v\n  worker reads mode=%q users=%v groups=%v",
				i, row, mode, users, groups, want.Mode, want.UserIds, want.GroupIds)
		}
	}
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
