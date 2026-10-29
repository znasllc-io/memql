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

import (
	"strings"
	"testing"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

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

// A REFUSAL THE DISPATCHER WORDS ITSELF is a machine's name in a second place.
// The fleet executor quotes the dispatcher's refusal whole into a failed step's
// message, and the check run prints that message, so a sentence that says
// "machine Joses-MacBook-Pro is no longer connected" publishes a hostname the
// owner never chose to publish just as a result's MachineName would. Every
// sentence the dispatcher and the router compose about a machine in a pipeline
// dispatch names it as the attempt line does: by the display name its owner
// chose, else by the start of its registration id.
//
// Each case asserts its refusal fired (the code), so a case that stopped
// reaching its sentence cannot pass as "no hostname in the message". The
// sentence is matched whole, not the id alone: the plan's reasons are keyed by
// registration id, which a message may carry.
func TestARefusalNeverNamesAMachineByWhatItReported(t *testing.T) {
	const hostname = "Joses-MacBook-Pro"
	// A production-shaped registration id: 24 random hex characters.
	const ghost = "3f9c2ab01d2e4f3a8b5c6d7e"
	cases := []struct {
		name     string
		holder   string // the registration the refusal is about
		code     string
		setup    func(t *testing.T, f *pinFleet)
		narrow   func(*Request)
		unnamed  string                    // how a machine with no display name is called
		sentence func(label string) string // what the refusal says about it
	}{
		{
			name: "the machine is no longer connected to this replica", holder: ghost, code: "worker_disconnected",
			setup: func(t *testing.T, f *pinFleet) {
				// The row says this replica holds the stream; the registry has none.
				row := machine(ghost, withLabels(pipelineLabels()))
				row.ConnectedNodeId = nodeA
				f.h.store.machines = append(f.h.store.machines, row)
			},
			narrow:   pinTo(ghost),
			unnamed:  "machine 3f9c2ab0",
			sentence: func(label string) string { return label + " is no longer connected to this replica" },
		},
		{
			name: "the machine's live registration does not advertise pipelines", holder: "desktop", code: "pipelines_not_allowed",
			setup: func(t *testing.T, f *pinFleet) {
				f.d.registry.WorkerById("desktop").Labels = map[string]string{"os": "linux"}
			},
			narrow:   pinTo("desktop"),
			unnamed:  "machine desktop",
			sentence: func(label string) string { return label + " does not advertise pipelines=allowed" },
		},
		{
			name: "the replica holding the machine has no forward", holder: "laptop", code: "worker_unreachable",
			setup:   func(t *testing.T, f *pinFleet) { f.d.SetRemoteDispatcher(nil) },
			narrow:  pinTo("laptop"),
			unnamed: "machine laptop",
			sentence: func(label string) string {
				return label + " is connected to replica " + nodeB + " and this node has no forward"
			},
		},
		{
			name: "the machine's reported repository policy does not accept the repository", holder: "desktop", code: "no_worker_available",
			setup: func(t *testing.T, f *pinFleet) {
				f.editRow(t, "desktop", func(c *Candidate) {
					c.RepositoryScopes = workerservice.RepositoryScopes{"workerHost.pipeline_step": {"someone/else"}}
				})
			},
			narrow:  pinTo("desktop"),
			unnamed: "machine desktop",
			sentence: func(label string) string {
				return label + "'s reported repository policy does not accept o/r for workerHost.pipeline_step"
			},
		},
		{
			name: "the machine has not reported the step's contract", holder: "desktop", code: "no_worker_available",
			setup: func(t *testing.T, f *pinFleet) {
				f.editRow(t, "desktop", func(c *Candidate) { c.ActionContracts = workerservice.ActionContracts{} })
			},
			narrow:  pinTo("desktop"),
			unnamed: "machine desktop",
			sentence: func(label string) string {
				return label + " has not reported workerHost.pipeline_step contract 5"
			},
		},
		{
			name: "the machine has not reported the native platform", holder: "desktop", code: "no_worker_available",
			setup: func(t *testing.T, f *pinFleet) {},
			narrow: func(r *Request) {
				r.PinRegistration = "desktop"
				r.Args["execution"], r.Args["platform"] = "native", "darwin/arm64"
			},
			unnamed:  "machine desktop",
			sentence: func(label string) string { return label + " has not reported native platform darwin/arm64" },
		},
	}
	for _, c := range cases {
		for _, variant := range []struct{ name, display, want string }{
			{"owner named", "CyberPC", "CyberPC"},
			{"owner gave no name", "", c.unnamed},
		} {
			t.Run(c.name+": "+variant.name, func(t *testing.T) {
				f := newPinFleet(t, "desktop")
				c.setup(t, f)
				f.editRow(t, c.holder, func(row *Candidate) { row.DisplayName, row.Name = variant.display, hostname })

				res, err := f.step(t, c.narrow)
				if err != nil || res.OK || res.ErrorCode != c.code {
					t.Fatalf("result = %+v err = %v, want the refusal %s", res, err, c.code)
				}
				if strings.Contains(res.ErrorMessage, hostname) {
					t.Errorf("the refusal names the machine by what it reported, which a public check run would print:\n%s", res.ErrorMessage)
				}
				if want := c.sentence(variant.want); !strings.Contains(res.ErrorMessage, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, res.ErrorMessage)
				}
			})
		}
	}
}
