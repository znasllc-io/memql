package memql

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"google.golang.org/protobuf/proto"
)

func TestAskCancellationIsScopedToForwardedHop(t *testing.T) {
	svc := &service{}
	alice := &streamSession{service: svc, stream: &forwardedStream{ctx: context.Background(), requestId: "mesh-alice"}}
	bob := &streamSession{service: svc, stream: &forwardedStream{ctx: context.Background(), requestId: "mesh-bob"}}
	a, doneA, err := alice.beginAskRequest(context.Background(), "same-client-id")
	if err != nil {
		t.Fatal(err)
	}
	defer doneA()
	b, doneB, err := bob.beginAskRequest(context.Background(), "same-client-id")
	if err != nil {
		t.Fatal(err)
	}
	defer doneB()
	svc.CancelForwardedRequest(context.Background(), "same-client-id")
	if a.Err() != nil || b.Err() != nil {
		t.Fatal("client ID addressed a replica-wide cancellation")
	}
	svc.CancelForwardedRequest(context.Background(), "mesh-alice")
	if a.Err() == nil || b.Err() != nil {
		t.Fatal("cancellation crossed the caller's hop")
	}
	if _, _, err := alice.beginAskRequest(context.Background(), "same-client-id"); err == nil {
		t.Fatal("unfinished request ID was reused")
	}
}

// Real NodeService transport, distinct browser sessions and the same client ID.
// No browser-local state exists at the receiver; the mesh ID is the cancel key.
func TestAskForwardUsesUniqueMeshIDsAndPreservesClientCorrelation(t *testing.T) {
	remote, address := startDisconnectPeer(t, node.NodeTypeAgent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity := &node.Identity{ID: "ask-bff", Type: node.NodeTypeBFF}
	peers := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peers, testLogger())
	dialer := node.NewWorkerDialer(identity, peers, nil, nil, []node.WorkerTarget{{NodeType: node.NodeTypeAgent, Address: address}}, testLogger())
	dialer.SetAiForwardResponseSink(router)
	dialer.Start(ctx)
	awaitDisconnectCondition(t, func() bool {
		ps := peers.SnapshotByType(node.NodeTypeAgent)
		return len(ps) == 1 && ps[0].Connection != nil
	}, "no peer")
	clients := []*recordingClientStream{newRecordingClientStream(ctx), newRecordingClientStream(ctx)}
	ids := map[string]bool{}
	for _, client := range clients {
		session := &streamSession{service: &service{logger: testLogger(), aiForwarder: router}, stream: client, logger: testLogger(), access: &auth.AccessContext{UserId: "v1:identity:user:alice", Role: auth.RoleWriter}, credentialClass: auth.ForwardedClassUser, accessLoaded: true, badgeStamped: true}
		envelope := &memqlv1.MemqlClientMessage{MessageId: "browser-message", Payload: &memqlv1.MemqlClientMessage_AiChat{AiChat: &memqlv1.AiChatMsg{RequestId: "client-turn", ConversationId: "private-conversation", Stream: true}}}
		if err := session.proxyAI(envelope, "client-turn", node.NodeTypeAgent); err != nil {
			t.Fatal(err)
		}
		select {
		case req := <-remote.requests:
			if req.RequestId == "client-turn" || ids[req.RequestId] {
				t.Fatal("shared client ID reached mesh registry")
			}
			ids[req.RequestId] = true
			var received memqlv1.MemqlClientMessage
			if err := proto.Unmarshal(req.MemqlEnvelope, &received); err != nil {
				t.Fatal(err)
			}
			if received.GetAiChat().GetRequestId() != "client-turn" {
				t.Fatal("durable turn identity changed")
			}
		case <-time.After(time.Second):
			t.Fatal("request did not cross hop")
		}
	}
	close(remote.drop)
	for _, client := range clients {
		awaitDisconnectCondition(t, func() bool { return len(client.snapshot()) > 0 }, "lost hop did not terminate")
		msg := client.snapshot()[0]
		if msg.GetQueryError().GetRequestId() != "client-turn" || msg.CorrelateTo != "browser-message" {
			t.Fatalf("lost client correlation: %v", msg)
		}
	}
}

func TestTranscriptionRegistryDoesNotCrossPrincipalsOrDeleteReplacement(t *testing.T) {
	actor := func(subject string) context.Context {
		return auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: subject})
	}
	alice, bob := transcriptionKey(actor("alice"), "same-id"), transcriptionKey(actor("bob"), "same-id")
	if alice == bob || alice == "same-id" {
		t.Fatal("transcription key does not scope caller")
	}
	svc := &service{}
	first, replacement := &transcribeStream{}, &transcribeStream{}
	if !svc.registerTranscribeStream(alice, first) || svc.registerTranscribeStream(alice, replacement) {
		t.Fatal("duplicate start was accepted")
	}
	if svc.lookupTranscribeStream(bob) != nil {
		t.Fatal("another caller can address the stream")
	}
	svc.unregisterTranscribeStream(alice)
	svc.registerTranscribeStream(alice, replacement)
	svc.removeTranscribeStream(alice, first)
	if svc.lookupTranscribeStream(alice) != replacement {
		t.Fatal("late finalizer removed new stream")
	}
}
