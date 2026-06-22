package telephony

import (
	"context"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
)

func TestPlaceCallRequiresArgs(t *testing.T) {
	i := &Integration{}
	if _, err := i.handlePlaceCall(context.Background(), map[string]any{"to": "+1", "from": "+2"}, 0); err == nil {
		t.Fatal("expected error when partitionId missing")
	}
}

func TestOutboundDirectionAttribution(t *testing.T) {
	out := &livekit.ParticipantInfo{
		Kind: livekit.ParticipantInfo_SIP,
		Attributes: map[string]string{
			"telephony.direction":  "outbound",
			"sip.phoneNumber":      "+18005550111", // callee
			"sip.trunkPhoneNumber": "+14155550100", // our caller-ID
			"sip.callID":           "c1",
		},
	}
	if directionOf(out) != "outbound" {
		t.Fatalf("direction = %q", directionOf(out))
	}
	if fromOf(out) != "+14155550100" {
		t.Errorf("from = %q, want our caller-ID", fromOf(out))
	}
	if toOf(out) != "+18005550111" {
		t.Errorf("to = %q, want callee", toOf(out))
	}

	in := &livekit.ParticipantInfo{
		Kind: livekit.ParticipantInfo_SIP,
		Attributes: map[string]string{
			"sip.phoneNumber":      "+19995550100", // caller
			"sip.trunkPhoneNumber": "+14155550100", // our DID
			"sip.callID":           "c2",
		},
	}
	if directionOf(in) != "inbound" {
		t.Fatalf("direction = %q", directionOf(in))
	}
	if fromOf(in) != "+19995550100" || toOf(in) != "+14155550100" {
		t.Errorf("inbound from/to = %q/%q", fromOf(in), toOf(in))
	}
}

func TestRandSuffixUnique(t *testing.T) {
	a, b := randSuffix(), randSuffix()
	if a == "" || a == b {
		t.Fatalf("randSuffix not unique: %q %q", a, b)
	}
	if strings.ContainsAny(a, " /") {
		t.Fatalf("randSuffix not room-safe: %q", a)
	}
}

func TestEndCallRequiresRoomService(t *testing.T) {
	i := &Integration{} // no room client
	if err := i.EndCall(context.Background(), "tel-x-1", "id"); err == nil {
		t.Fatal("expected error without RoomService")
	}
}
