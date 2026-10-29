//go:build agent

package worker

// pipeline_machine_name_test.go -- a dispatch's result names the machine by the
// label its owner sees in Fleet, beside the registration id (Result.MachineName).
//
// A caller that has to SAY where a step ran -- a check run read on GitHub --
// would otherwise say a registration id. The name is stamped from the candidate
// the dispatch was attempted on (attempt), so a machine this replica holds and
// one its sibling holds, reached by the forward, are named alike: the replica
// that picked the candidate read its row, and the forward's wire carries nothing
// new.

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
			t.Errorf("MachineName = %q, want the display name the owner sees", res.MachineName)
		}
	})

	t.Run("a machine its sibling holds, reached by the forward", func(t *testing.T) {
		res, err := f.step(t, pinTo("laptop"))
		if err != nil || !res.OK || res.WorkerId != "laptop" || res.NodeId != nodeB {
			t.Fatalf("result = %+v err = %v, want the step on the laptop, held by %s", res, err, nodeB)
		}
		if res.MachineName != "CyberPC" {
			t.Errorf("MachineName = %q, want the display name the owner sees", res.MachineName)
		}
	})
}

// The label is the one Fleet draws: the owner's display name, and the name the
// machine reported when there is none (Candidate.Label). The registration id is
// not a name and is never stood in for one here; a reader that has only the id
// says so.
func TestAMachineWithNoDisplayNameIsNamedByWhatItReported(t *testing.T) {
	f := newPinFleet(t, "desktop")
	f.editRow(t, "laptop", func(c *Candidate) { c.DisplayName, c.Name = "   ", "cyberpc-host" })
	res, err := f.step(t, pinTo("laptop"))
	if err != nil || !res.OK || res.WorkerId != "laptop" {
		t.Fatalf("result = %+v err = %v, want the step on the laptop", res, err)
	}
	if res.MachineName != "cyberpc-host" {
		t.Errorf("MachineName = %q, want the name the machine reported", res.MachineName)
	}

	f.editRow(t, "laptop", func(c *Candidate) { c.DisplayName, c.Name = "", "" })
	res, err = f.step(t, pinTo("laptop"))
	if err != nil || !res.OK || res.WorkerId != "laptop" {
		t.Fatalf("result = %+v err = %v, want the step on the laptop", res, err)
	}
	if res.MachineName != "" {
		t.Errorf("MachineName = %q for a row that names the machine neither way, want none", res.MachineName)
	}
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
