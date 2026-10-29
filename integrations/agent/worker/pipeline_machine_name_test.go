//go:build agent

package worker

// pipeline_machine_name_test.go -- a dispatch's result names the machine by the
// display name its OWNER chose, beside the registration id (Result.MachineName),
// and by nothing else.
//
// A caller that has to SAY where a step ran -- a check run, which is public on
// GitHub for a public repository -- would otherwise say a registration id. The
// name is stamped from the candidate the dispatch was attempted on (attempt), so
// a machine this replica holds and one its sibling holds, reached by the forward,
// are named alike: the replica that picked the candidate read its row, and the
// forward's wire carries nothing new.
//
// The name a machine REPORTED (Candidate.Name, its hostname unless the owner
// replaced it, "Joses-MacBook-Pro") is never stamped. Fleet, which only the owner
// sees, falls back to it; a public check run must not, because it can say things
// the owner never chose to publish.

import "testing"

func TestAPipelineResultNamesTheMachineAcrossReplicas(t *testing.T) {
	f := newPinFleet(t, "desktop")
	f.editRow(t, "desktop", func(c *Candidate) { c.DisplayName = "MSI Raider" })
	f.editRow(t, "laptop", func(c *Candidate) { c.DisplayName = "CyberPC" })

	t.Run("a machine this replica holds", func(t *testing.T) {
		res, err := f.step(t, nil)
		if err != nil || !res.OK || res.WorkerId != "desktop" || res.NodeId != nodeA {
			t.Fatalf("result = %+v err = %v, want the step on the desktop, held here", res, err)
		}
		if res.MachineName != "MSI Raider" {
			t.Errorf("MachineName = %q, want the display name the owner chose", res.MachineName)
		}
	})

	t.Run("a machine its sibling holds, reached by the forward", func(t *testing.T) {
		res, err := f.step(t, pinTo("laptop"))
		if err != nil || !res.OK || res.WorkerId != "laptop" || res.NodeId != nodeB {
			t.Fatalf("result = %+v err = %v, want the step on the laptop, held by %s", res, err, nodeB)
		}
		if res.MachineName != "CyberPC" {
			t.Errorf("MachineName = %q, want the display name the owner chose", res.MachineName)
		}
	})
}

// ONLY A NAME THE OWNER CHOSE. The display name is the owner's own label for the
// machine; the name the machine reported is its hostname, which the owner never
// chose to publish. A machine with no display name is named nothing here, and a
// reader that has only the registration id says so.
func TestAMachineIsNamedOnlyByTheNameItsOwnerChose(t *testing.T) {
	const hostname = "Joses-MacBook-Pro"
	f := newPinFleet(t, "desktop")
	ranOn := func(t *testing.T, registrationId string, edit func(*Candidate)) Result {
		t.Helper()
		f.editRow(t, registrationId, edit)
		var narrow func(*Request)
		if registrationId == "laptop" {
			narrow = pinTo("laptop") // held by the sibling replica: the forward
		}
		res, err := f.step(t, narrow)
		if err != nil || !res.OK || res.WorkerId != registrationId {
			t.Fatalf("result = %+v err = %v, want the step on %s", res, err, registrationId)
		}
		return res
	}

	t.Run("a display name is named, trimmed", func(t *testing.T) {
		if got := ranOn(t, "laptop", func(c *Candidate) { c.DisplayName, c.Name = "  CyberPC  ", hostname }).MachineName; got != "CyberPC" {
			t.Errorf("MachineName = %q, want the display name the owner chose, trimmed", got)
		}
	})

	for _, registrationId := range []string{"laptop", "desktop"} {
		held := map[string]string{"laptop": "reached by the forward", "desktop": "held by this replica"}[registrationId]
		t.Run("the name the machine reported is never named: "+held, func(t *testing.T) {
			res := ranOn(t, registrationId, func(c *Candidate) { c.DisplayName, c.Name = "", hostname })
			if res.MachineName != "" {
				t.Errorf("MachineName = %q for a machine its owner gave no display name, want none: a check run is public", res.MachineName)
			}
		})
	}

	t.Run("a display name of only whitespace is none, whatever the machine reported", func(t *testing.T) {
		if got := ranOn(t, "laptop", func(c *Candidate) { c.DisplayName, c.Name = "   ", hostname }).MachineName; got != "" {
			t.Errorf("MachineName = %q, want none", got)
		}
	})

	t.Run("a machine that names itself neither way is none", func(t *testing.T) {
		if got := ranOn(t, "laptop", func(c *Candidate) { c.DisplayName, c.Name = "", "" }).MachineName; got != "" {
			t.Errorf("MachineName = %q for a row that names the machine neither way, want none", got)
		}
	})
}

// A result that came back from no machine names none: nothing was selected.
func TestAResultFromNoMachineNamesNone(t *testing.T) {
	f := newPinFleet(t, "desktop")
	res, err := f.step(t, pinTo("a-machine-nobody-has"))
	if err != nil || res.OK || res.WorkerId != "" {
		t.Fatalf("result = %+v err = %v, want a refusal that selected nothing", res, err)
	}
	if res.MachineName != "" {
		t.Errorf("MachineName = %q on a result that selected no machine", res.MachineName)
	}
}
