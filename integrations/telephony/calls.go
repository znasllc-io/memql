package telephony

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

// LiveKit webhook event names we act on.
const (
	eventParticipantJoined = "participant_joined"
	eventParticipantLeft   = "participant_left"
	eventRoomFinished      = "room_finished"
)

// openCall is the in-memory record of a connected telephony leg, populated on
// participant_joined and consumed on participant_left / room_finished to write
// exactly one append-only v1:telephony:call row with the real duration.
type openCall struct {
	startedAt   time.Time
	direction   string
	fromE164    string
	toE164      string
	partitionID string
	room        string
	agentID     string
}

// callTracker holds open telephony legs keyed by the SIP call id. Process-local
// is sufficient: the LiveKit webhook is a single endpoint, so join + leave for
// a call land on the same node.
type callTracker struct {
	mu   sync.Mutex
	open map[string]openCall
}

func newCallTracker() *callTracker { return &callTracker{open: map[string]openCall{}} }

// HandleWebhookEvent processes one LiveKit webhook event, writing a call record
// when a telephony leg disconnects. Non-telephony rooms and non-SIP
// participants are ignored. Safe to call with a nil engine (no-op write).
func (i *Integration) HandleWebhookEvent(ctx context.Context, event *livekit.WebhookEvent) error {
	if event == nil {
		return nil
	}
	room := event.GetRoom()
	if room == nil || !IsTelephonyRoom(room.GetName()) {
		return nil
	}
	switch event.GetEvent() {
	case eventParticipantJoined:
		p := event.GetParticipant()
		if p == nil || !isSIPParticipant(p) {
			return nil
		}
		i.tracker().remember(callIDOf(p), openCall{
			startedAt:   time.Now().UTC(),
			direction:   directionOf(p),
			fromE164:    fromOf(p),
			toE164:      toOf(p),
			partitionID: partitionFromEvent(event),
			room:        room.GetName(),
		})
		return nil
	case eventParticipantLeft:
		p := event.GetParticipant()
		if p == nil || !isSIPParticipant(p) {
			return nil
		}
		return i.closeLeg(ctx, callIDOf(p), "completed")
	case eventRoomFinished:
		// Close any legs still open in this room (carrier-side hangup that did
		// not emit a participant_left).
		return i.closeRoomLegs(ctx, room.GetName(), "completed")
	}
	return nil
}

func (i *Integration) tracker() *callTracker {
	if i.calls == nil {
		i.calls = newCallTracker()
	}
	return i.calls
}

func (t *callTracker) remember(callID string, c openCall) {
	if callID == "" {
		return
	}
	t.mu.Lock()
	t.open[callID] = c
	t.mu.Unlock()
}

func (t *callTracker) take(callID string) (openCall, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.open[callID]
	if ok {
		delete(t.open, callID)
	}
	return c, ok
}

func (t *callTracker) takeRoom(room string) []openCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []openCall
	for id, c := range t.open {
		if c.room == room {
			out = append(out, c)
			delete(t.open, id)
		}
	}
	return out
}

// closeLeg writes the final call row for one disconnected leg.
func (i *Integration) closeLeg(ctx context.Context, callID, disposition string) error {
	c, ok := i.tracker().take(callID)
	if !ok {
		return nil // unknown / already closed
	}
	return i.writeCallRecord(ctx, c, disposition)
}

func (i *Integration) closeRoomLegs(ctx context.Context, room, disposition string) error {
	for _, c := range i.tracker().takeRoom(room) {
		if err := i.writeCallRecord(ctx, c, disposition); err != nil {
			return err
		}
	}
	return nil
}

// writeCallRecord persists one completed call via mutationRecordCall. One
// append-only row per call, with the real duration + disposition. Bound to a
// partition + partition-scoped room (Amendment A).
func (i *Integration) writeCallRecord(ctx context.Context, c openCall, disposition string) error {
	eng, err := i.requireEngine()
	if err != nil {
		return nil // engine not wired (e.g. tests) -- nothing to persist
	}
	dur := int(time.Since(c.startedAt).Seconds())
	if dur < 0 {
		dur = 0
	}
	q := callRecordMutation(c, dur, disposition)
	if _, err := eng.Execute(ctx, q); err != nil {
		return fmt.Errorf("telephony: write call record: %w", err)
	}
	return nil
}

// callRecordMutation builds the mutationRecordCall query for a completed call.
// Pure + unit-testable.
func callRecordMutation(c openCall, durationSeconds int, disposition string) string {
	return fmt.Sprintf(
		`mutationRecordCall({direction: %q, fromE164: %q, toE164: %q, partitionId: %q, room: %q, carrier: %q, agentId: %q, durationSeconds: %d, disposition: %q})`,
		c.direction, c.fromE164, c.toE164, c.partitionID, c.room, "", c.agentID, durationSeconds, disposition,
	)
}

// --- LiveKit participant helpers ------------------------------------------

func isSIPParticipant(p *livekit.ParticipantInfo) bool {
	if p == nil {
		return false
	}
	if p.GetKind() == livekit.ParticipantInfo_SIP {
		return true
	}
	// Fallback: SIP participants carry sip.* attributes.
	_, ok := p.GetAttributes()["sip.callID"]
	return ok
}

func sipAttr(p *livekit.ParticipantInfo, key string) string {
	if p == nil {
		return ""
	}
	return p.GetAttributes()[key]
}

// directionOf reports the call direction. An outbound leg (placed via
// PlaceCall) is tagged telephony.direction=outbound on the SIP participant;
// everything else is an inbound call.
func directionOf(p *livekit.ParticipantInfo) string {
	if sipAttr(p, "telephony.direction") == "outbound" {
		return "outbound"
	}
	return "inbound"
}

// fromOf / toOf normalize caller/callee per direction. For an inbound call the
// external party (sip.phoneNumber) is the caller; for an outbound call it is
// the callee, and our caller-ID (sip.trunkPhoneNumber) is the from.
func fromOf(p *livekit.ParticipantInfo) string {
	if directionOf(p) == "outbound" {
		return sipAttr(p, "sip.trunkPhoneNumber")
	}
	return sipAttr(p, "sip.phoneNumber")
}

func toOf(p *livekit.ParticipantInfo) string {
	if directionOf(p) == "outbound" {
		return sipAttr(p, "sip.phoneNumber")
	}
	return sipAttr(p, "sip.trunkPhoneNumber")
}

// callIDOf returns the SIP call id (stable across join/leave), falling back to
// the participant identity.
func callIDOf(p *livekit.ParticipantInfo) string {
	if id := sipAttr(p, "sip.callID"); id != "" {
		return id
	}
	return p.GetIdentity()
}

// partitionFromEvent resolves the tenant partition for a telephony call,
// preferring the dispatch-rule metadata propagated onto the participant, then
// the room metadata; both carry {"partitionId": "..."}. Amendment A: scope is
// the partition, resolved from data, never parsed from a product name.
func partitionFromEvent(event *livekit.WebhookEvent) string {
	if p := event.GetParticipant(); p != nil {
		if pid := partitionFromMetadata(p.GetMetadata()); pid != "" {
			return pid
		}
		if pid := p.GetAttributes()["partitionId"]; pid != "" {
			return pid
		}
	}
	if r := event.GetRoom(); r != nil {
		if pid := partitionFromMetadata(r.GetMetadata()); pid != "" {
			return pid
		}
	}
	return ""
}

func partitionFromMetadata(meta string) string {
	meta = strings.TrimSpace(meta)
	if meta == "" {
		return ""
	}
	var m struct {
		PartitionID string `json:"partitionId"`
	}
	if json.Unmarshal([]byte(meta), &m) == nil {
		return m.PartitionID
	}
	return ""
}
