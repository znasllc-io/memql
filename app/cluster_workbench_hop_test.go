package app

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/grpc"
)

// Each peer owns its stream and answers heartbeats without sharing the
// originating agent's state. This catches a reused dialer that installs a
// response sink but never opens the workbench connections (#5803).
type workbenchDialPeer struct {
	nodev1.UnimplementedNodeServiceServer
	id string
}

func (p *workbenchDialPeer) Stream(stream nodev1.NodeService_StreamServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&nodev1.NodeServerMessage{Payload: &nodev1.NodeServerMessage_NodeWelcome{
		NodeWelcome: &nodev1.NodeWelcome{NodeId: p.id},
	}}); err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(&nodev1.NodeServerMessage{Payload: &nodev1.NodeServerMessage_Heartbeat{
			Heartbeat: &nodev1.NodeHeartbeat{Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
		}}); err != nil {
			return err
		}
	}
}

func TestWorkbenchWiringPreservesSharedDialerRoutesAcrossTheHop(t *testing.T) {
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "1")
	for _, restricted := range []bool{true, false} {
		name := "bff-unrestricted"
		if restricted {
			name = "agent-with-fleet"
		}
		t.Run(name, func(t *testing.T) {
			identity := &node.Identity{ID: "origin", Type: node.NodeTypeBFF}
			if restricted {
				identity.Type = node.NodeTypeAgent
			}
			peers := node.NewPeerManager(identity, dialerLogger())
			peers.SetTimings(20*time.Millisecond, time.Second, 2*time.Second)
			peers.SetStaleGossipTimeout(50 * time.Millisecond)
			targets := []node.WorkerTarget{
				{NodeType: node.NodeTypeAgent, NodeId: "sibling-agent"},
				{NodeType: node.NodeTypeWorkbench, NodeId: "workbench-a"},
				{NodeType: node.NodeTypeWorkbench, NodeId: "workbench-b"},
				{NodeType: node.NodeTypeIdentity, NodeId: "identity"},
			}
			for index := range targets {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				targets[index].Address = listener.Addr().String()
				server := grpc.NewServer()
				nodev1.RegisterNodeServiceServer(server, &workbenchDialPeer{id: targets[index].NodeId})
				go server.Serve(listener)
				t.Cleanup(server.Stop)
			}
			dialer := node.NewWorkerDialer(identity, peers, nil, nil, targets, dialerLogger())
			if restricted {
				dialer.SetDialTypes(node.NodeTypeAgent)
			}
			a := &App{Logger: dialerLogger()}
			a.Dependencies = append(a.Dependencies, dialer)
			a.wireWorkbenchForwarding(identity, peers, nil, nil)
			if countDialers(a) != 1 || a.existingWorkerDialer() != dialer {
				t.Fatal("workbench wiring replaced or duplicated the fleet dialer")
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				dialer.Stop(context.Background())
				peers.Stop(context.Background())
			})
			peers.Start(ctx)
			dialer.Start(ctx)
			deadline := time.Now().Add(3 * time.Second)
			var stableSince time.Time
			for time.Now().Before(deadline) {
				connected := map[string]bool{}
				for _, typ := range []node.NodeType{node.NodeTypeAgent, node.NodeTypeWorkbench, node.NodeTypeIdentity} {
					for _, peer := range peers.SnapshotByType(typ) {
						connected[peer.Info.NodeId] = peer.Monitored && peer.Connection != nil &&
							time.Since(peer.LastSeen) < 100*time.Millisecond
					}
				}
				ready := connected["sibling-agent"] && connected["workbench-a"] && connected["workbench-b"]
				if restricted {
					if connected["identity"] {
						t.Fatal("adding workbenches opened an unrelated route")
					}
				} else {
					ready = ready && connected["identity"]
				}
				if !ready {
					stableSince = time.Time{}
				} else if stableSince.IsZero() {
					stableSince = time.Now()
				} else if time.Since(stableSince) > 150*time.Millisecond {
					return // Both replicas survive multiple gossip-expiry periods.
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("shared dialer failed to retain fleet and workbench routes: %v", dialer.ActiveAddresses())
		})
	}
}
