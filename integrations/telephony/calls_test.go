package telephony

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/memql"
)

// fakeEngine records the queries Execute receives. Embedding the interface
// means we only implement the one method the call path uses.
type fakeEngine struct {
	memql.IntegrationEngineAccess
	mu      sync.Mutex
	queries []string
}

func (f *fakeEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return &memql.ExecuteResult{}, nil
}

func (f *fakeEngine) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return ""
	}
	return f.queries[len(f.queries)-1]
}

func sipParticipant(callID, from, to, metadata string) *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{
		Identity: "sip_" + from,
		Kind:     livekit.ParticipantInfo_SIP,
		Metadata: metadata,
		Attributes: map[string]string{
			"sip.callID":           callID,
			"sip.phoneNumber":      from,
			"sip.trunkPhoneNumber": to,
		},
	}
}

func TestPartitionFromMetadata(t *testing.T) {
	if got := partitionFromMetadata(`{"partitionId":"acme"}`); got != "acme" {
		t.Errorf("got %q", got)
	}
	if got := partitionFromMetadata("not json"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := partitionFromMetadata(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestCallRecordMutation(t *testing.T) {
	c := openCall{direction: "inbound", fromE164: "+14155550100", toE164: "+18005550111", partitionID: "acme", room: "tel-acme-x"}
	q := callRecordMutation(c, 42, "completed", 0.07)
	for _, want := range []string{
		`recordCall(`, `direction: "inbound"`, `fromE164: "+14155550100"`,
		`toE164: "+18005550111"`, `partitionId: "acme"`, `room: "tel-acme-x"`,
		`durationSeconds: 42`, `disposition: "completed"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("mutation missing %q\n got: %s", want, q)
		}
	}
}

func TestWebhookInboundCallLifecycle(t *testing.T) {
	eng := &fakeEngine{}
	i := &Integration{}
	i.SetEngine(eng)

	room := &livekit.Room{Name: "tel-acme-abc"}
	p := sipParticipant("call-1", "+14155550100", "+18005550111", `{"partitionId":"acme"}`)

	// join -> remembered, no write yet
	if err := i.HandleWebhookEvent(context.Background(), &livekit.WebhookEvent{Event: eventParticipantJoined, Room: room, Participant: p}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(eng.queries) != 0 {
		t.Fatalf("expected no write on join, got %d", len(eng.queries))
	}

	// leave -> one append-only call row
	if err := i.HandleWebhookEvent(context.Background(), &livekit.WebhookEvent{Event: eventParticipantLeft, Room: room, Participant: p}); err != nil {
		t.Fatalf("leave: %v", err)
	}
	q := eng.last()
	if !strings.Contains(q, "recordCall(") || !strings.Contains(q, `partitionId: "acme"`) || !strings.Contains(q, `disposition: "completed"`) {
		t.Fatalf("unexpected call record: %s", q)
	}
}

func TestWebhookIgnoresNonTelephonyRoom(t *testing.T) {
	eng := &fakeEngine{}
	i := &Integration{}
	i.SetEngine(eng)
	room := &livekit.Room{Name: "polyphon-space1"}
	p := sipParticipant("call-2", "+1", "+2", "")
	_ = i.HandleWebhookEvent(context.Background(), &livekit.WebhookEvent{Event: eventParticipantLeft, Room: room, Participant: p})
	if len(eng.queries) != 0 {
		t.Fatalf("non-telephony room must be ignored, got %d writes", len(eng.queries))
	}
}

func TestWebhookRoomFinishedClosesOpenLegs(t *testing.T) {
	eng := &fakeEngine{}
	i := &Integration{}
	i.SetEngine(eng)
	room := &livekit.Room{Name: "tel-acme-z"}
	p := sipParticipant("call-3", "+14155550100", "+18005550111", `{"partitionId":"acme"}`)
	_ = i.HandleWebhookEvent(context.Background(), &livekit.WebhookEvent{Event: eventParticipantJoined, Room: room, Participant: p})
	_ = i.HandleWebhookEvent(context.Background(), &livekit.WebhookEvent{Event: eventRoomFinished, Room: room})
	if eng.last() == "" || !strings.Contains(eng.last(), `room: "tel-acme-z"`) {
		t.Fatalf("room_finished should close open leg, got %q", eng.last())
	}
}

func TestProvisionInboundRequiresArgs(t *testing.T) {
	i := &Integration{}
	if _, err := i.handleProvisionInbound(context.Background(), map[string]any{"e164": "+1"}, 0); err == nil {
		t.Fatal("expected error when partitionId missing")
	}
}
