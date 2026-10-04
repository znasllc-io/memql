package memql

import (
	"context"
	"fmt"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/grpc/codes"
)

// An Ask turn names its source in AiChatMsg.provider and its level in
// AiChatMsg.level. A word outside the picker's vocabulary refuses the turn
// with a CODE a client can branch on -- before the engine is reached, so no
// goal, no run and no transcript turn exist for it. The session here has no
// engine at all: reaching it would panic, which is the assertion.
func TestAnAskTurnWithAnUnknownRouteChoiceIsRefusedWithACodeBeforeTheEngine(t *testing.T) {
	cases := []struct {
		name, provider, level, code, field string
	}{
		{"unknown source", "vendor:anything", "", engine.RouteSourceInvalid, "source"},
		{"model pin on an app", "app:claude-code:opus", "", engine.RouteSourceInvalid, "source"},
		{"unknown level", "app:claude-code", "max", engine.RouteLevelInvalid, "level"},
		{"embeddings is not a person's level", "", "embeddings", engine.RouteLevelInvalid, "level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := newRecordingClientStream(context.Background())
			s := &streamSession{service: &service{logger: testLogger()}, stream: stream, logger: testLogger()}
			s.handleAskStream(context.Background(), "turn-1", "message-1", &memqlv1.AiChatMsg{
				RequestId: "turn-1", ConversationId: "v1:os:askConversation:c1", Stream: true,
				Provider: tc.provider, Level: tc.level,
				Messages: []*memqlv1.AiChatMessage{{Role: "user", Content: "hi"}},
			})
			sent := stream.snapshot()
			if len(sent) != 1 {
				t.Fatalf("sent %d messages, want exactly the refusal", len(sent))
			}
			qe := sent[0].GetQueryError()
			if qe == nil {
				t.Fatalf("sent %T, want a QueryError", sent[0].GetPayload())
			}
			if qe.GetRequestId() != "turn-1" || qe.GetError().GetCode() != codes.InvalidArgument.String() {
				t.Fatalf("refused %q with %s, want the turn refused as InvalidArgument", qe.GetRequestId(), qe.GetError().GetCode())
			}
			md := qe.GetError().GetMetadata()
			if md["code"] != tc.code || md["field"] != tc.field {
				t.Fatalf("metadata = %v, want code %s field %s", md, tc.code, tc.field)
			}
		})
	}
}

// A refusal only the engine can make -- a route this cluster does not have --
// reaches the client the same way.
func TestAnAskRouteRefusalFromTheEngineKeepsItsCode(t *testing.T) {
	code, message, md := askFailureStatus(fmt.Errorf("ask: %w", &engine.RouteChoiceError{
		Code: engine.RoutePolicyUnknown, Field: "source", Value: "policy:gone", Reason: `route "gone" is not one of this cluster's routes`,
	}))
	if code != codes.InvalidArgument || md["code"] != engine.RoutePolicyUnknown || md["field"] != "source" || message == "" {
		t.Fatalf("status = %s %q %v", code, message, md)
	}
	// Anything else stays what it was.
	if code, _, md := askFailureStatus(fmt.Errorf("the conversation could not be saved")); code != codes.Internal || md != nil {
		t.Fatalf("an ordinary failure became %s %v", code, md)
	}
}
