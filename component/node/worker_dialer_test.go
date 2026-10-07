package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestParseWorkerPeers_Empty(t *testing.T) {
	got, issues := ParseWorkerPeers("")
	if got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues for empty input, got %v", issues)
	}
	got, issues = ParseWorkerPeers("   ")
	if got != nil {
		t.Errorf("expected nil for whitespace-only input, got %v", got)
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues for whitespace-only input, got %v", issues)
	}
}

func TestParseWorkerPeers_Basic(t *testing.T) {
	in := "identity=identity:50053,agent=agent:50055,workbench=workbench:50057,planner=planner:50056"
	got, issues := ParseWorkerPeers(in)
	if len(issues) != 0 {
		t.Errorf("expected no issues, got %v", issues)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 targets, got %d: %v", len(got), got)
	}
	wantByType := map[NodeType]string{
		NodeTypeIdentity:  "identity:50053",
		NodeTypeAgent:     "agent:50055",
		NodeTypeWorkbench: "workbench:50057",
		NodeTypePlanner:   "planner:50056",
	}
	for _, target := range got {
		if wantByType[target.NodeType] != target.Address {
			t.Errorf("type %s: want %s, got %s", target.NodeType, wantByType[target.NodeType], target.Address)
		}
	}
}

func TestParseWorkerPeers_Whitespace(t *testing.T) {
	in := "  agent = agent:50055 ,  planner=planner:50056  "
	got, _ := ParseWorkerPeers(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d: %v", len(got), got)
	}
}

func TestParseWorkerPeers_CaseInsensitiveType(t *testing.T) {
	got, _ := ParseWorkerPeers("AGENT=agent:50055,Planner=planner:50056")
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d: %v", len(got), got)
	}
	for _, target := range got {
		if target.NodeType != NodeTypeAgent && target.NodeType != NodeTypePlanner {
			t.Errorf("unexpected node type: %s", target.NodeType)
		}
	}
}

func TestParseWorkerPeers_SkipsUnknownOrBFF(t *testing.T) {
	// BFF is a valid node type but not a dialable type (we'd never forward
	// to it), so it must be rejected even though ValidNodeTypes says yes.
	got, issues := ParseWorkerPeers("bff=bff:50058,nonsense=whatever:50060,agent=agent:50055")
	if len(got) != 1 {
		t.Fatalf("expected 1 target (only agent), got %d: %v", len(got), got)
	}
	if got[0].NodeType != NodeTypeAgent {
		t.Errorf("expected NodeTypeAgent, got %s", got[0].NodeType)
	}
	// memql#3450: a rejected entry must be REPORTED, not silently dropped.
	if len(issues) != 2 {
		t.Fatalf("expected 2 reported issues (bff + nonsense), got %d: %v", len(issues), issues)
	}
	joined := issueEntries(issues)
	for _, want := range []string{"bff=bff:50058", "nonsense=whatever:50060"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected issue for %q, got %v", want, issues)
		}
	}
	for _, issue := range issues {
		if issue.Reason == "" {
			t.Errorf("issue %q carries no reason", issue.Entry)
		}
	}
}

// memql#3450: the documented cluster-mode workbench seed
// (MEMQL_WORKER_PEERS=workbench=workbench:50060, shipped in
// deploy/k8s/base/agent.yaml) must produce a dial target. It was parsed and
// then discarded because workbench was in neither dial predicate.
func TestParseWorkerPeers_AcceptsWorkbench(t *testing.T) {
	got, issues := ParseWorkerPeers("workbench=workbench:50060")
	if len(issues) != 0 {
		t.Fatalf("expected no issues for the documented workbench seed, got %v", issues)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 target, got %d: %v", len(got), got)
	}
	if got[0].NodeType != NodeTypeWorkbench {
		t.Errorf("expected NodeTypeWorkbench, got %s", got[0].NodeType)
	}
	if got[0].Address != "workbench:50060" {
		t.Errorf("expected workbench:50060, got %s", got[0].Address)
	}
}

// memql#3450: an unrecognised node type must name itself AND the valid set,
// so the operator can see what they should have typed.
func TestParseWorkerPeers_UnknownTypeIssueIsActionable(t *testing.T) {
	_, issues := ParseWorkerPeers("workbanch=workbench:50060")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0].Reason, "workbanch") {
		t.Errorf("reason should name the bad type, got %q", issues[0].Reason)
	}
	if !strings.Contains(issues[0].Reason, "workbench") {
		t.Errorf("reason should list the valid types, got %q", issues[0].Reason)
	}
}

func TestParseWorkerPeers_SkipsMalformed(t *testing.T) {
	got, issues := ParseWorkerPeers(",,=addr,type=,agent=agent:50055,garbage")
	if len(got) != 1 {
		t.Fatalf("expected 1 target, got %d: %v", len(got), got)
	}
	if got[0].Address != "agent:50055" {
		t.Errorf("expected agent:50055, got %s", got[0].Address)
	}
	// "=addr" (empty type), "type=" (empty address) and "garbage" (no "=")
	// are all reported; the empty parts from ",," are pure formatting and
	// stay quiet.
	if len(issues) != 3 {
		t.Fatalf("expected 3 reported issues, got %d: %v", len(issues), issues)
	}
	joined := issueEntries(issues)
	for _, want := range []string{"=addr", "type=", "garbage"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected issue for %q, got %v", want, issues)
		}
	}
}

func TestParseWorkerPeers_DedupesByTypeAndAddress(t *testing.T) {
	got, issues := ParseWorkerPeers("agent=agent:50055,agent=agent:50055,agent=agent-2:50055")
	if len(got) != 2 {
		t.Fatalf("expected 2 unique targets, got %d: %v", len(got), got)
	}
	if len(issues) != 1 {
		t.Fatalf("expected the duplicate to be reported, got %d: %v", len(issues), issues)
	}
}

func issueEntries(issues []WorkerPeerIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, issue.Entry)
	}
	return strings.Join(parts, "|")
}

func TestIsWorkerType(t *testing.T) {
	cases := []struct {
		in   NodeType
		want bool
	}{
		{NodeTypeAgent, true},
		{NodeTypePlanner, true},
		{NodeTypeAgent, true},
		{NodeTypePlanner, true},
		{NodeTypeBFF, false},
		// Neither of these is a worker the BFF forwards AI work to; both are
		// nevertheless dialable (see TestIsDialableType).
		{NodeTypeWorkbench, false},
		{NodeTypeIdentity, false},
		{NodeType("nonsense"), false},
		{NodeType(""), false},
	}
	for _, tc := range cases {
		if got := isWorkerType(tc.in); got != tc.want {
			t.Errorf("isWorkerType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsDialableType(t *testing.T) {
	cases := []struct {
		in   NodeType
		want bool
	}{
		{NodeTypeAgent, true},
		{NodeTypePlanner, true},
		{NodeTypeAgent, true},
		{NodeTypePlanner, true},
		// memql#3380: the bff dials identity for the deploy-control surface.
		{NodeTypeIdentity, true},
		// memql#3450: agent nodes dial workbench for WorkbenchForwardRequest.
		{NodeTypeWorkbench, true},
		// The bff is the dialer, never the dialee; mcp is a protocol head that
		// nobody forwards to.
		{NodeTypeBFF, false},
		{NodeTypeMCP, false},
		{NodeType("nonsense"), false},
		{NodeType(""), false},
	}
	for _, tc := range cases {
		if got := isDialableType(tc.in); got != tc.want {
			t.Errorf("isDialableType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// dialableTypeNames feeds the operator-facing warning; it must stay in sync
// with isDialableType or the warning tells people to type a value the parser
// then rejects.
func TestDialableTypeNames_MatchesPredicate(t *testing.T) {
	names := dialableTypeNames()
	if len(names) == 0 {
		t.Fatalf("dialableTypeNames returned nothing")
	}
	for _, n := range names {
		if !isDialableType(NodeType(n)) {
			t.Errorf("dialableTypeNames lists %q but isDialableType rejects it", n)
		}
	}
	for nt := range ValidNodeTypes {
		if !isDialableType(nt) {
			continue
		}
		if !slices.Contains(names, string(nt)) {
			t.Errorf("isDialableType accepts %q but dialableTypeNames omits it", nt)
		}
	}
	if !slices.Contains(names, string(NodeTypeIdentity)) {
		t.Errorf("dialableTypeNames omits identity, which is dialable but not in ValidNodeTypes")
	}
	if !slices.IsSorted(names) {
		t.Errorf("dialableTypeNames should be sorted for a stable warning: %v", names)
	}
}

func TestTargetKey_DisambiguatesByType(t *testing.T) {
	a := WorkerTarget{NodeType: NodeTypeAgent, Address: "same:50055"}
	b := WorkerTarget{NodeType: NodeTypePlanner, Address: "same:50055"}
	if targetKey(a) == targetKey(b) {
		t.Errorf("targetKey should disambiguate by type: %s == %s", targetKey(a), targetKey(b))
	}
}

func TestNewWorkerDialer_NilWhenNothingToDrive(t *testing.T) {
	// No engine, no eventBus, no seeds -> nil.
	wd := NewWorkerDialer(testIdentity(), NewPeerManager(testIdentity(), testLogger()), nil, nil, nil, testLogger())
	if wd != nil {
		t.Errorf("expected nil dialer with nothing to drive reconciliation")
	}
}

func TestNewWorkerDialer_NilWhenIdentityOrPeerMgrMissing(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	seeds := []WorkerTarget{{NodeType: NodeTypeAgent, Address: "agent:50055"}}

	if wd := NewWorkerDialer(nil, pm, nil, nil, seeds, testLogger()); wd != nil {
		t.Errorf("expected nil dialer when identity is nil")
	}
	if wd := NewWorkerDialer(testIdentity(), nil, nil, nil, seeds, testLogger()); wd != nil {
		t.Errorf("expected nil dialer when peerMgr is nil")
	}
	if wd := NewWorkerDialer(testIdentity(), pm, nil, nil, seeds, nil); wd != nil {
		t.Errorf("expected nil dialer when logger is nil")
	}
}

func TestNewWorkerDialer_ConstructsWithSeedsOnly(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	seeds := []WorkerTarget{{NodeType: NodeTypeAgent, Address: "agent:50055"}}
	wd := NewWorkerDialer(testIdentity(), pm, nil, nil, seeds, testLogger())
	if wd == nil {
		t.Fatalf("expected dialer to be constructed with seeds only")
	}
	if got := wd.ActiveAddresses(); len(got) != 0 {
		t.Errorf("expected no active addresses before Start, got %v", got)
	}
}

func TestPeerManager_AttachDetachConnection(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	// AttachConnection on a missing entry must be a no-op, not panic.
	pm.AttachConnection("ghost", &peerConnection{})
	if pm.Get("ghost") != nil {
		t.Errorf("AttachConnection on missing node must not create an entry")
	}

	pm.RegisterMonitored(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeAgent),
		Address:  "peer1:50055",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	entry := pm.Get("peer-1")
	if entry == nil {
		t.Fatalf("expected entry after RegisterMonitored")
	}
	if entry.Connection != nil {
		t.Errorf("Connection should start nil")
	}

	conn := &peerConnection{}
	pm.AttachConnection("peer-1", conn)
	if pm.Get("peer-1").Connection != conn {
		t.Errorf("AttachConnection failed to bind connection")
	}

	pm.DetachConnection("peer-1")
	if pm.Get("peer-1").Connection != nil {
		t.Errorf("DetachConnection failed to clear connection")
	}

	// Detaching a missing node must not panic.
	pm.DetachConnection("ghost")
}

// With the per-pod addresses emitted by the manifests, an agent must retain
// every sibling target while excluding itself. A shared Service address would
// make both replicas look like self and leave no cross-replica route.
func TestWorkerDialerDistinctReplicaAddressesRetainSibling(t *testing.T) {
	replicas := []WorkerTarget{
		{NodeType: NodeTypeAgent, NodeId: "agent-a", Address: "10.42.0.10:50055"},
		{NodeType: NodeTypeAgent, NodeId: "agent-b", Address: "10.42.0.11:50055"},
	}
	for i, self := range replicas {
		identity := &Identity{ID: self.NodeId, Type: self.NodeType, Address: self.Address}
		wd := NewWorkerDialer(identity, NewPeerManager(identity, testLogger()), nil, nil, replicas, testLogger())
		wd.SetDialTypes(NodeTypeAgent)
		got, err := wd.buildDesiredSet(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		other := replicas[1-i]
		if len(got) != 1 || got[targetKey(other)] != other {
			t.Fatalf("%s desired peers = %+v; want only sibling %+v", self.NodeId, got, other)
		}
	}
}

func TestWorkerDialerDBTargetsReplaceGenericServiceSeedsByType(t *testing.T) {
	serviceSeed := WorkerTarget{NodeType: NodeTypeWorkbench, Address: "workbench:50060"}
	agentSeed := WorkerTarget{NodeType: NodeTypeAgent, Address: "agent:50055"}
	payload, err := structpb.NewStruct(map[string]any{
		"nodeType": "workbench",
		"address":  "10.42.0.29:50060",
		"health":   "healthy",
	})
	if err != nil {
		t.Fatal(err)
	}

	identity := testIdentity()
	wd := NewWorkerDialer(identity, NewPeerManager(identity, testLogger()), nil, nil,
		[]WorkerTarget{serviceSeed, agentSeed}, testLogger())
	wd.engine = discoveryReply{result: &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{
			Id:      "v1:cluster:node:workbench-a",
			Payload: payload,
		}},
	}}}

	got, err := wd.buildDesiredSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	discovered := WorkerTarget{NodeType: NodeTypeWorkbench, NodeId: "workbench-a", Address: "10.42.0.29:50060"}
	if len(got) != 2 || got[targetKey(discovered)] != discovered || got[targetKey(agentSeed)] != agentSeed {
		t.Fatalf("desired peers = %+v; want discovered workbench replica and unshadowed agent seed", got)
	}
	if _, ok := got[targetKey(serviceSeed)]; ok {
		t.Fatalf("generic workbench service seed remains alongside per-node targets: %+v", got)
	}
}

func TestWorkerDialerRetainsServiceSeedUntilDBHasType(t *testing.T) {
	seed := WorkerTarget{NodeType: NodeTypeWorkbench, Address: "workbench:50060"}
	identity := testIdentity()
	wd := NewWorkerDialer(identity, NewPeerManager(identity, testLogger()), nil, nil, []WorkerTarget{seed}, testLogger())
	wd.engine = discoveryReply{result: &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}}

	got, err := wd.buildDesiredSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[targetKey(seed)] != seed {
		t.Fatalf("desired peers = %+v; want bootstrap seed while DB has no workbench rows", got)
	}
}

func TestWorkerDialerStaleEntryCannotDetachReplacementConnection(t *testing.T) {
	for _, teardown := range []string{"shutdown", "connect exit"} {
		t.Run(teardown, func(t *testing.T) {
			identity := testIdentity()
			pm := NewPeerManager(identity, testLogger())
			peerID := "agent-a"
			pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: peerID, NodeType: "agent", Address: "10.42.0.11:50055"})

			oldTarget := WorkerTarget{NodeType: NodeTypeAgent, NodeId: peerID, Address: "10.42.0.10:50055"}
			newTarget := WorkerTarget{NodeType: NodeTypeAgent, NodeId: peerID, Address: "10.42.0.11:50055"}
			wd := NewWorkerDialer(identity, pm, nil, nil, []WorkerTarget{oldTarget}, testLogger())
			oldConn := newPeerConnection(identity, peerID, oldTarget.Address, testLogger())
			newConn := newPeerConnection(identity, peerID, newTarget.Address, testLogger())
			t.Cleanup(oldConn.Close)
			t.Cleanup(newConn.Close)
			oldEntry := &dialEntry{
				target: oldTarget,
				conn:   oldConn,
				nodeId: peerID,
				cancel: func() {},
			}
			wd.conns[targetKey(oldTarget)] = oldEntry
			pm.AttachConnection(peerID, newConn)

			switch teardown {
			case "shutdown":
				wd.shutdownEntry(oldEntry)
			case "connect exit":
				wd.handleConnectExit(oldEntry)
			}
			if got := pm.Get(peerID).Connection; got != newConn {
				t.Fatalf("stale %s cleared the replacement connection: got %p, want %p", teardown, got, newConn)
			}
		})
	}
}

type discoveryReply struct {
	result *memqlengine.ExecuteResult
	err    error
}

func (d discoveryReply) Execute(context.Context, string) (*memqlengine.ExecuteResult, error) {
	return d.result, d.err
}

func TestWorkerDialerFailedDiscoveryRetainsLiveConnection(t *testing.T) {
	for _, why := range []string{"read error", "nil result", "missing bundle", "successful empty"} {
		t.Run(why, func(t *testing.T) {
			identity := testIdentity()
			pm := NewPeerManager(identity, testLogger())
			target := WorkerTarget{NodeType: NodeTypeAgent, NodeId: "live-sibling", Address: "sibling:50055"}
			wd := NewWorkerDialer(identity, pm, nil, nil, []WorkerTarget{target}, testLogger())
			// Only DB discovery supplies this existing connection, not a static seed.
			wd.seeds = nil
			response := discoveryReply{}
			switch why {
			case "read error":
				response.err = errors.New("transient database failure")
			case "missing bundle":
				response.result = &memqlengine.ExecuteResult{}
			case "successful empty":
				response.result = &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
			}
			wd.engine = response
			conn := newPeerConnection(identity, target.NodeId, target.Address, testLogger())
			t.Cleanup(conn.Close)
			canceled := false
			entry := &dialEntry{target: target, nodeId: target.NodeId, conn: conn, cancel: func() { canceled = true }}
			wd.conns[targetKey(target)] = entry
			pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: target.NodeId, NodeType: "agent", Address: target.Address})
			pm.AttachConnection(target.NodeId, conn)
			wd.reconcile(context.Background())
			if why == "successful empty" {
				if !canceled || len(wd.conns) != 0 {
					t.Fatal("a successful empty topology must retire its connections")
				}
			} else if canceled || wd.conns[targetKey(target)] != entry || pm.Get(target.NodeId).Connection != conn {
				t.Fatal("an unsuccessful discovery closed a healthy peer and its in-flight requests")
			}
		})
	}
}
