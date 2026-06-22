package telephony

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/livekit/protocol/livekit"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/proto"
)

// outboundTrunkName is the single outbound trunk fronting the carrier for
// placing PSTN calls.
const outboundTrunkName = "memql-telephony-outbound"

// CallHandle identifies a live outbound call so it can be ended / transferred /
// sent DTMF.
type CallHandle struct {
	Room      string `json:"room"`
	Identity  string `json:"identity"`
	SIPCallID string `json:"sipCallId"`
	To        string `json:"to"`
	From      string `json:"from"`
}

// outboundCapabilities are the agent-facing outbound calling operations.
func (i *Integration) outboundCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "placeCall",
			Description: "Place an outbound PSTN call to a number and bridge it into a partition-scoped room the agent talks in. Optional post-dial DTMF for IVR navigation.",
			Handler:     i.handlePlaceCall,
			ArgsSchema: map[string]string{
				"to":          "string (required) - callee E.164",
				"from":        "string (required) - caller-ID DID (E.164) to present",
				"partitionId": "string (required) - tenant partition the call belongs to",
				"dtmf":        "string (optional) - digits to dial after connect",
			},
		},
		{
			Name:        "endCall",
			Description: "Hang up an outbound call by removing the SIP participant.",
			Handler:     i.handleEndCall,
			ArgsSchema:  map[string]string{"room": "string (required)", "identity": "string (required)"},
		},
		{
			Name:        "transferCall",
			Description: "Transfer a live call to another E.164 (LiveKit SIP warm/cold transfer).",
			Handler:     i.handleTransferCall,
			ArgsSchema:  map[string]string{"room": "string (required)", "identity": "string (required)", "to": "string (required) - transfer target E.164"},
		},
		{
			Name:        "sendDtmf",
			Description: "Send DTMF digits to a live call (IVR navigation).",
			Handler:     i.handleSendDtmf,
			ArgsSchema:  map[string]string{"room": "string (required)", "identity": "string (required)", "digits": "string (required)"},
		},
	}
}

// EnsureOutboundTrunk returns the outbound trunk id, creating it once
// (idempotent by name). Address + auth come from the integration config
// (carrier SIP gateway + trunk credentials).
func (i *Integration) EnsureOutboundTrunk(ctx context.Context) (string, error) {
	sip, err := i.requireSIP()
	if err != nil {
		return "", err
	}
	if i.outboundAddress == "" {
		return "", fmt.Errorf("telephony: outbound SIP address not configured (set MEMQL_TELEPHONY_OUTBOUND_SIP_ADDRESS)")
	}
	list, err := sip.ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{})
	if err != nil {
		return "", fmt.Errorf("telephony: list outbound trunks: %w", err)
	}
	for _, t := range list.GetItems() {
		if t.GetName() == outboundTrunkName {
			return t.GetSipTrunkId(), nil
		}
	}
	created, err := sip.CreateSIPOutboundTrunk(ctx, &livekit.CreateSIPOutboundTrunkRequest{
		Trunk: &livekit.SIPOutboundTrunkInfo{
			Name:         outboundTrunkName,
			Address:      i.outboundAddress,
			AuthUsername: i.outboundAuthUser,
			AuthPassword: i.outboundAuthPass,
		},
	})
	if err != nil {
		return "", fmt.Errorf("telephony: create outbound trunk: %w", err)
	}
	return created.GetSipTrunkId(), nil
}

// PlaceCall dials `to` from caller-ID `from` and bridges the callee into a
// fresh partition-scoped room (Amendment A). Returns a handle for follow-on
// control. The SIP participant carries direction + partition metadata so the
// webhook call-record path attributes it as outbound.
func (i *Integration) PlaceCall(ctx context.Context, to, from, partitionID, dtmf string) (CallHandle, error) {
	sip, err := i.requireSIP()
	if err != nil {
		return CallHandle{}, err
	}
	trunkID, err := i.EnsureOutboundTrunk(ctx)
	if err != nil {
		return CallHandle{}, err
	}
	room := RoomPrefixForPartition(partitionID) + randSuffix()
	identity := "sip-callee-" + randSuffix()
	meta, _ := json.Marshal(map[string]string{"partitionId": partitionID})
	info, err := sip.CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunkID,
		SipCallTo:           to,
		RoomName:            room,
		ParticipantIdentity: identity,
		ParticipantMetadata: string(meta),
		ParticipantAttributes: map[string]string{
			"telephony.direction":  "outbound",
			"sip.phoneNumber":      to,
			"sip.trunkPhoneNumber": from,
		},
		Dtmf: dtmf,
	})
	if err != nil {
		return CallHandle{}, fmt.Errorf("telephony: place call: %w", err)
	}
	return CallHandle{Room: room, Identity: info.GetParticipantIdentity(), SIPCallID: info.GetSipCallId(), To: to, From: from}, nil
}

// EndCall removes the SIP participant, hanging up the call.
func (i *Integration) EndCall(ctx context.Context, room, identity string) error {
	if i.room == nil {
		return fmt.Errorf("telephony: LiveKit RoomService not configured")
	}
	_, err := i.room.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
	if err != nil {
		return fmt.Errorf("telephony: end call: %w", err)
	}
	return nil
}

// TransferCall warm/cold-transfers a live call to another E.164.
func (i *Integration) TransferCall(ctx context.Context, room, identity, to string) error {
	sip, err := i.requireSIP()
	if err != nil {
		return err
	}
	_, err = sip.TransferSIPParticipant(ctx, &livekit.TransferSIPParticipantRequest{
		ParticipantIdentity: identity,
		RoomName:            room,
		TransferTo:          to,
	})
	if err != nil {
		return fmt.Errorf("telephony: transfer call: %w", err)
	}
	return nil
}

// SendDTMF sends DTMF digits to a live call via a SipDTMF data packet
// addressed to the SIP participant.
func (i *Integration) SendDTMF(ctx context.Context, room, identity, digits string) error {
	if i.room == nil {
		return fmt.Errorf("telephony: LiveKit RoomService not configured")
	}
	packet := &livekit.DataPacket{
		Kind:                  livekit.DataPacket_RELIABLE,
		DestinationIdentities: []string{identity},
		Value:                 &livekit.DataPacket_SipDtmf{SipDtmf: &livekit.SipDTMF{Digit: digits}},
	}
	raw, err := proto.Marshal(packet)
	if err != nil {
		return fmt.Errorf("telephony: marshal dtmf: %w", err)
	}
	_, err = i.room.SendData(ctx, &livekit.SendDataRequest{
		Room:                  room,
		Data:                  raw,
		Kind:                  livekit.DataPacket_RELIABLE,
		DestinationIdentities: []string{identity},
	})
	if err != nil {
		return fmt.Errorf("telephony: send dtmf: %w", err)
	}
	return nil
}

// --- capability handlers ---------------------------------------------------

func (i *Integration) handlePlaceCall(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	to, from, partitionID := asString(args["to"]), asString(args["from"]), asString(args["partitionId"])
	if to == "" || from == "" || partitionID == "" {
		return nil, fmt.Errorf("telephony.placeCall: to, from, and partitionId are required")
	}
	handle, err := i.PlaceCall(ctx, to, from, partitionID, asString(args["dtmf"]))
	if err != nil {
		return nil, err
	}
	return handleNode("telephony:call:placed", handle), nil
}

func (i *Integration) handleEndCall(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	room, identity := asString(args["room"]), asString(args["identity"])
	if room == "" || identity == "" {
		return nil, fmt.Errorf("telephony.endCall: room and identity are required")
	}
	if err := i.EndCall(ctx, room, identity); err != nil {
		return nil, err
	}
	return handleNode("telephony:call:ended", map[string]string{"room": room, "identity": identity}), nil
}

func (i *Integration) handleTransferCall(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	room, identity, to := asString(args["room"]), asString(args["identity"]), asString(args["to"])
	if room == "" || identity == "" || to == "" {
		return nil, fmt.Errorf("telephony.transferCall: room, identity, and to are required")
	}
	if err := i.TransferCall(ctx, room, identity, to); err != nil {
		return nil, err
	}
	return handleNode("telephony:call:transferred", map[string]string{"room": room, "identity": identity, "to": to}), nil
}

func (i *Integration) handleSendDtmf(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	room, identity, digits := asString(args["room"]), asString(args["identity"]), asString(args["digits"])
	if room == "" || identity == "" || digits == "" {
		return nil, fmt.Errorf("telephony.sendDtmf: room, identity, and digits are required")
	}
	if err := i.SendDTMF(ctx, room, identity, digits); err != nil {
		return nil, err
	}
	return handleNode("telephony:call:dtmf", map[string]string{"room": room, "identity": identity, "digits": digits}), nil
}

// --- helpers ---------------------------------------------------------------

func handleNode(concept string, payload any) []memorynodes.MemoryNode {
	b, _ := json.Marshal(payload)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("%s:%d", concept, time.Now().UnixNano()),
		Concept:   "integration:" + concept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   b,
	}}
}

func randSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
