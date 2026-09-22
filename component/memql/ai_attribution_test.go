package memql

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func promptFor(t *testing.T) *PromptTemplate {
	t.Helper()
	return &PromptTemplate{Name: "agentReply", Level: string(airoute.LevelStrong)}
}

// A PROMPT CALL NAMES ITS CALLER (memql#5581). requestForPrompt built a
// request out of the prompt and nothing else, so the v1:router:call row for a
// prompt invocation named no person, no upstream request and no footprint.
func TestPromptRequestNamesThePersonTheRequestAndTheFootprint(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:alice",
		Role:   auth.RoleWriter,
	})
	ctx = ContextWithAICallAttribution(ctx, AICallAttribution{
		RequestId: "v1:cognition:utterance:abc",
		AgentId:   "v1:agents:agent:scribe",
		Touches:   []string{"v1:library:file", "v1:knowledge:document"},
	})

	req, err := requestForPrompt(ctx, promptFor(t), nil, airoute.ModalityChat, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if req.UserId != "v1:identity:user:alice" {
		t.Errorf("userId=%q; the decision record cannot answer who caused this call", req.UserId)
	}
	if req.RequestId != "v1:cognition:utterance:abc" {
		t.Errorf("requestId=%q; the row cannot be joined to the request that caused it", req.RequestId)
	}
	if req.AgentId != "v1:agents:agent:scribe" {
		t.Errorf("agentId=%q", req.AgentId)
	}
	if want := []string{"v1:library:file", "v1:knowledge:document"}; !reflect.DeepEqual(req.Touches, want) {
		t.Errorf("touches=%v, want %v", req.Touches, want)
	}
	if req.CallerKind != auth.CallerKindUser {
		t.Errorf("callerKind=%q, want %q", req.CallerKind, auth.CallerKindUser)
	}
}

// A CALL WITH NO PERSON SAYS WHICH KIND OF CALLER IT WAS. The blank userId is
// the correct and complete answer for a maintenance sweep or a boot seed, and
// without a second field it is indistinguishable from an attribution defect.
func TestCallerlessPromptRequestSaysWhichKindOfCallerItWas(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want string
	}{
		{"maintenance sweep", func() context.Context {
			return auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("auditEventRetentionSweep"))
		}, auth.CallerKindSystem},
		{"connector", func() context.Context {
			return auth.ContextWithConnectorActor(context.Background(), "shopify")
		}, auth.CallerKindConnector},
		{"anonymous", func() context.Context {
			return auth.ContextWithAccess(context.Background(), auth.AnonymousActor())
		}, auth.CallerKindAnonymous},
		{"nobody stamped anything", func() context.Context {
			return context.Background()
		}, auth.CallerKindUnattributed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := requestForPrompt(tc.ctx(), promptFor(t), nil, airoute.ModalityChat, "hello")
			if err != nil {
				t.Fatal(err)
			}
			if req.CallerKind != tc.want {
				t.Fatalf("callerKind=%q, want %q", req.CallerKind, tc.want)
			}
			if req.CallerKind == "" {
				t.Fatal("a blank caller kind is the one answer the field must never carry")
			}
		})
	}
}

// THE RUN CONTEXT IS THE CORRELATION KEY FOR AN AUTOMATION'S PROMPT CALL.
// component/automations stamps one on every non-sandboxed execution, which is
// the path a DSL ai(...) runs on, so the run and step that caused the call are
// already on the context -- no parameter threaded through the shape evaluator.
func TestPromptRequestTakesItsRequestIdFromTheWorkRun(t *testing.T) {
	ctx := common.ContextWithRun(context.Background(), common.RunContext{
		RunId:       "v1:work:run:r1",
		StepKey:     "summarise",
		Mode:        common.RunModeLive,
		OwnerUserId: "v1:identity:user:bob",
	})
	req, err := requestForPrompt(ctx, promptFor(t), nil, airoute.ModalityChat, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if want := "run:v1:work:run:r1#summarise"; req.RequestId != want {
		t.Errorf("requestId=%q, want %q", req.RequestId, want)
	}
	// The run's owner is a person, on a context that carries no access
	// context at all -- an automation executing on a replica that did not
	// take the request.
	if req.UserId != "v1:identity:user:bob" {
		t.Errorf("userId=%q; the run's owner is the person this call was for", req.UserId)
	}
	// The session door's hinge (design D7): a prompt call inside a run now
	// names the step it could hand over.
	if req.RunId != "v1:work:run:r1" || req.StepId != "summarise" {
		t.Errorf("run/step = %q/%q; a tool-needing prompt turn had no step to hand an app door", req.RunId, req.StepId)
	}
}

// NOTHING IS INVENTED. With nothing on the context naming an upstream request,
// the field stays EMPTY and the router stamps a fresh id -- a synthesised id
// here would be a correlation key that correlates nothing.
func TestPromptRequestInventsNoRequestId(t *testing.T) {
	req, err := requestForPrompt(context.Background(), promptFor(t), nil, airoute.ModalityChat, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if req.RequestId != "" {
		t.Fatalf("requestId=%q; with nothing on the context to name, it must stay empty", req.RequestId)
	}
}

// A CALLER THAT ALREADY NAMED A VALUE KEEPS IT. The agent replier stamps the
// gRPC request id onto its own request, and rediscovering a different one here
// would rename the very thing the field exists to join on.
func TestCallAttributionFillsOnlyWhatIsEmpty(t *testing.T) {
	ctx := ContextWithAICallAttribution(context.Background(), AICallAttribution{
		RequestId: "from-context",
		AgentId:   "agent-from-context",
		Touches:   []string{"from-context"},
	})
	req := applyCallAttribution(ctx, airoute.ResolveRequest{
		RequestId:  "from-caller",
		AgentId:    "agent-from-caller",
		UserId:     "user-from-caller",
		CallerKind: auth.CallerKindSystem,
		Touches:    []string{"from-caller"},
	})
	for _, got := range []struct{ field, value, want string }{
		{"requestId", req.RequestId, "from-caller"},
		{"agentId", req.AgentId, "agent-from-caller"},
		{"userId", req.UserId, "user-from-caller"},
		{"callerKind", req.CallerKind, auth.CallerKindSystem},
	} {
		if got.value != got.want {
			t.Errorf("%s=%q, want %q", got.field, got.value, got.want)
		}
	}
	if !reflect.DeepEqual(req.Touches, []string{"from-caller"}) {
		t.Errorf("touches=%v, want the caller's", req.Touches)
	}
}

// An empty attribution must not SHADOW a richer one an outer frame set.
func TestEmptyAttributionDoesNotShadow(t *testing.T) {
	ctx := ContextWithAICallAttribution(context.Background(), AICallAttribution{RequestId: "outer"})
	ctx = ContextWithAICallAttribution(ctx, AICallAttribution{})
	req := applyCallAttribution(ctx, airoute.ResolveRequest{})
	if req.RequestId != "outer" {
		t.Fatalf("requestId=%q; an empty stamp overwrote a richer one", req.RequestId)
	}
}
