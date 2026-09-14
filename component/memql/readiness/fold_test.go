package readiness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type foldFixture struct {
	Name    string         `json:"name"`
	Now     time.Time      `json:"now"`
	Reports []NodeReport   `json:"reports"`
	Nodes   []NodeLiveness `json:"nodes"`
	Expect  []struct {
		Module       string   `json:"module"`
		State        State    `json:"state"`
		Disagreement []string `json:"disagreement"`
		// Unknown and Stale are OPTIONAL in a fixture: the nine older cases
		// predate both lists and mean "none", which is what a nil decodes to
		// and what the normalisation below compares against.
		Unknown []string `json:"unknown"`
		Stale   []string `json:"stale"`
	} `json:"expect"`
}

// THE FIXTURES ARE SHARED. clients/os/test/system/readinessFold.test.ts reads
// the same directory, so a case added here is a case the TypeScript mirror
// must also pass; that is the whole parity mechanism.
func TestFoldFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "fold", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 15 {
		t.Fatalf("expected at least 15 fold fixtures, found %d -- the parity set is incomplete", len(paths))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx foldFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		t.Run(fx.Name, func(t *testing.T) {
			got := Fold(fx.Reports, fx.Nodes, fx.Now)
			if len(got) != len(fx.Expect) {
				t.Fatalf("got %d verdicts, want %d: %+v", len(got), len(fx.Expect), got)
			}
			for i, want := range fx.Expect {
				if got[i].Module != want.Module || got[i].State != want.State {
					t.Errorf("verdict %d: got %s=%s, want %s=%s", i, got[i].Module, got[i].State, want.Module, want.State)
				}
				if !reflect.DeepEqual(got[i].Disagreement, want.Disagreement) {
					t.Errorf("verdict %d disagreement: got %v, want %v", i, got[i].Disagreement, want.Disagreement)
				}
				for _, list := range []struct {
					name      string
					got, want []string
				}{
					{"unknown", got[i].Unknown, want.Unknown},
					{"stale", got[i].Stale, want.Stale},
				} {
					if list.got == nil {
						t.Errorf("verdict %d: %s is nil; the JSON contract is an empty list, never null", i, list.name)
					}
					wantList := list.want
					if wantList == nil {
						wantList = []string{}
					}
					if !reflect.DeepEqual(list.got, wantList) {
						t.Errorf("verdict %d %s: got %v, want %v", i, list.name, list.got, wantList)
					}
				}
			}
		})
	}
}

func TestNodeIsLiveNeedsBothHealthAndRecency(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-NodeLiveWindow / 2)
	stale := now.Add(-NodeLiveWindow - time.Second)
	cases := []struct {
		n    NodeLiveness
		want bool
	}{
		{NodeLiveness{NodeId: "a", Health: "healthy", LastSeen: fresh}, true},
		{NodeLiveness{NodeId: "a", Health: "draining", LastSeen: fresh}, true},
		{NodeLiveness{NodeId: "a", Health: "stopped", LastSeen: fresh}, false},
		{NodeLiveness{NodeId: "a", Health: "offline", LastSeen: fresh}, false},
		{NodeLiveness{NodeId: "a", Health: "healthy", LastSeen: stale}, false},
		{NodeLiveness{NodeId: "a", Health: "healthy"}, false},
	}
	for _, c := range cases {
		if got := NodeIsLive(c.n, now); got != c.want {
			t.Errorf("%+v: got %v want %v", c.n, got, c.want)
		}
	}
}

// A NODE THAT COULD NOT EVALUATE IS NOT A NODE THAT SAID "NO". Before this,
// a failed fleet read on one replica was written as `unconfigured`, folded
// against the others' `configured`, and read as "Partly set up" on every
// surface until the next deploy (production, 2026-09-13).
func TestAnUnknownRowNeverPinsAVerdict(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	nodes := []NodeLiveness{
		{NodeId: "a", Health: "healthy", LastSeen: now.Add(-time.Second)},
		{NodeId: "b", Health: "healthy", LastSeen: now.Add(-time.Second)},
	}
	got := Fold([]NodeReport{
		{Module: "ai", NodeId: "a", NodeType: "agent", State: Configured, ReportedAt: now},
		{Module: "ai", NodeId: "b", NodeType: "bff", State: Unknown, Reason: ReasonFleetReadFailed, ReportedAt: now},
	}, nodes, now)
	if len(got) != 1 || got[0].State != Configured {
		t.Fatalf("an unknown row changed the verdict: %+v", got)
	}
	if len(got[0].Disagreement) != 0 {
		t.Errorf("an unknown row counted as disagreement: %v", got[0].Disagreement)
	}
	if !reflect.DeepEqual(got[0].Unknown, []string{"b"}) {
		t.Errorf("the unknown node is not named: %v", got[0].Unknown)
	}
	for _, nv := range got[0].Nodes {
		if nv.NodeId == "b" {
			t.Errorf("the unknown node appears among the voters: %+v", got[0].Nodes)
		}
	}
}

// inferenceLanesFor builds the three inference lanes the way InferenceLanes
// does, so the cases below read as the rows a node actually writes.
func inferenceLanesFor(local, app, federation bool, live bool) []LaneReport {
	lane := func(name, from, scope string, complete bool) LaneReport {
		return LaneReport{Name: name, ConfigurableFrom: from, Scope: scope, Complete: complete,
			Slots: []SlotReport{{Name: InferenceLiveSlot, Present: complete && live}}}
	}
	return []LaneReport{
		lane(InferenceLaneLocal, "fleet", LaneScopeCluster, local),
		lane(InferenceLaneApp, "fleet", LaneScopeCluster, app),
		lane(InferenceLaneFederation, "deployment", "", federation),
	}
}

// THE REGRESSION memql#5259 NAMES, in the fold's own terms: a node that did
// not hear the pairing still says `unconfigured` from boot, and it is named as
// behind rather than counted as a node reporting a missing setup.
func TestARowBehindTheFreshestIsStaleNotAVote(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 43, 2, 0, time.UTC)
	live := func(id string) NodeLiveness { return NodeLiveness{NodeId: id, Health: "healthy", LastSeen: now} }
	got := Fold([]NodeReport{
		{Module: "ai", NodeId: "agent-a", NodeType: "agent", State: Configured, Lanes: inferenceLanesFor(true, false, false, true), ReportedAt: now.Add(-8 * time.Second)},
		{Module: "ai", NodeId: "edge-a", NodeType: "edge", State: Unconfigured, Lanes: inferenceLanesFor(false, false, false, false), ReportedAt: now.Add(-6 * time.Hour)},
	}, []NodeLiveness{live("agent-a"), live("edge-a")}, now)
	if len(got) != 1 || got[0].State != Configured {
		t.Fatalf("a node behind the cluster pinned the verdict: %+v", got)
	}
	if !reflect.DeepEqual(got[0].Stale, []string{"edge-a"}) || len(got[0].Disagreement) != 0 {
		t.Fatalf("stale %v disagreement %v; want [edge-a] and none", got[0].Stale, got[0].Disagreement)
	}
	for _, nv := range got[0].Nodes {
		if nv.NodeId == "edge-a" {
			t.Fatalf("the stale node appears among the voters: %+v", got[0].Nodes)
		}
	}

	// THE NEGATIVE CONTROL: the same two rows with the lanes UNMARKED -- the
	// shape every row had before scope existed -- fold exactly as they always
	// did. What sets a row aside is the scope, not the timestamp.
	unmarked := func(lanes []LaneReport) []LaneReport {
		out := append([]LaneReport(nil), lanes...)
		for i := range out {
			out[i].Scope = ""
		}
		return out
	}
	old := Fold([]NodeReport{
		{Module: "ai", NodeId: "agent-a", NodeType: "agent", State: Configured, Lanes: unmarked(inferenceLanesFor(true, false, false, true)), ReportedAt: now.Add(-8 * time.Second)},
		{Module: "ai", NodeId: "edge-a", NodeType: "edge", State: Unconfigured, Lanes: unmarked(inferenceLanesFor(false, false, false, false)), ReportedAt: now.Add(-6 * time.Hour)},
	}, []NodeLiveness{live("agent-a"), live("edge-a")}, now)
	if old[0].State != Partial || len(old[0].Stale) != 0 {
		t.Fatalf("unmarked lanes were judged stale: %+v", old[0])
	}
}

// PRESENCE IS NOT A FACT ABOUT THE CLUSTER. Two rows that agree a door is
// configured but disagree about whether its machine is awake were evaluated
// at different moments -- and neither is behind: the verdict does not turn on
// the live slot, so neither may be set aside for it.
func TestALiveSlotDifferenceIsNotStaleness(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	live := func(id string) NodeLiveness { return NodeLiveness{NodeId: id, Health: "healthy", LastSeen: now} }
	got := Fold([]NodeReport{
		{Module: "ai", NodeId: "agent-a", NodeType: "agent", State: Configured, Lanes: inferenceLanesFor(true, false, false, false), ReportedAt: now},
		{Module: "ai", NodeId: "bff-a", NodeType: "bff", State: Configured, Lanes: inferenceLanesFor(true, false, false, true), ReportedAt: now.Add(-9 * time.Minute)},
	}, []NodeLiveness{live("agent-a"), live("bff-a")}, now)
	if got[0].State != Configured || len(got[0].Stale) != 0 || len(got[0].Nodes) != 2 {
		t.Fatalf("a sleeping laptop set a row aside: %+v", got[0])
	}
}

// The wire contract: both lists are empty lists, never null, on every
// verdict -- including one with no reporters at all.
func TestTheSetAsideListsAreNeverNull(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	got := Fold([]NodeReport{{Module: "ai", NodeId: "gone", State: Configured, ReportedAt: now}}, nil, now)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"unknown":[]`, `"stale":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the verdict does not carry %s: %s", want, raw)
		}
	}
}
