package memql

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestAskStreamIDsMatchSnapshotsOnDirectAndForwardedClients(t *testing.T) {
	for _, hop := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "forwarded"}[hop], func(t *testing.T) {
			metadata, err := structpb.NewStruct(map[string]any{"ask": map[string]any{
				"kind": "run", "arguments": map[string]any{
					"runId": "v1:work:run:run-a", "goalId": "v1:work:goal:goal-a", "concept": "v1:work:run",
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			message := &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AiChunk{AiChunk: &memqlv1.AiStreamChunk{
				RequestId: "request", Chunk: &memqlv1.AiStreamChunk_Metadata{Metadata: metadata},
			}}}
			client := newRecordingClientStream(context.Background())
			session := &streamSession{stream: client}
			if hop {
				var received memqlv1.MemqlServerMessage
				worker := &streamSession{stream: &forwardedStream{ctx: context.Background(), requestId: "mesh-request", send: func(envelope *nodev1.NodeServerMessage) error {
					return proto.Unmarshal(envelope.GetAiForwardResponse().GetMemqlServerMsg(), &received)
				}}}
				if err := worker.sendServerMessage("mesh-request", message); err != nil {
					t.Fatal(err)
				}
				args := received.GetAiChunk().GetMetadata().AsMap()["ask"].(map[string]any)["arguments"].(map[string]any)
				if args["runId"] != "v1:work:run:run-a" {
					t.Fatal("mesh identity was rewritten")
				}
				responses := make(chan *memqlv1.MemqlServerMessage, 2)
				responses <- &received
				responses <- &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AiChatResult{AiChatResult: &memqlv1.AiChatResult{RequestId: "request"}}}
				close(responses)
				session.relayForwardedResponses("browser-message", "request", responses)
			} else if err := session.sendServerMessage("browser-message", message); err != nil {
				t.Fatal(err)
			}
			args := client.snapshot()[0].GetAiChunk().GetMetadata().AsMap()["ask"].(map[string]any)["arguments"].(map[string]any)
			if args["runId"] != "run-a" || args["goalId"] != "goal-a" || args["concept"] != "v1:work:run" {
				t.Fatalf("stream identity differs from query wire contract: %v", args)
			}
			original := metadata.AsMap()["ask"].(map[string]any)["arguments"].(map[string]any)
			if original["runId"] != "v1:work:run:run-a" {
				t.Fatal("shared internal metadata was mutated")
			}
		})
	}
}
