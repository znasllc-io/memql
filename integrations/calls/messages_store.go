package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"time"
)

const messageReceiptConcept = "v1:calls:messageReceipt"
const messageRateConcept = "v1:calls:messageRate"

type messageReceiptState struct {
	Sequence     int64  `json:"sequence"`
	Digest       string `json:"digest,omitempty"`
	EventID      string `json:"eventId,omitempty"`
	Status       string `json:"status"`
	ChatAt       int64  `json:"chatAt,omitempty"`
	ReactionAt   int64  `json:"reactionAt,omitempty"`
	HandAt       int64  `json:"handAt,omitempty"`
	HandSequence int64  `json:"handSequence,omitempty"`
	HandRaised   bool   `json:"handRaised,omitempty"`
}
type messageReceipt struct {
	ID, SiteID, RoomID, SeatID, Identity string
	JoinedAt, ExpiresAt, Version         time.Time
	State                                messageReceiptState
}

func messageReceiptID(r roomRow, seat seatRow) string {
	return string(id.NewUntracked().FromBytes([]byte(fmt.Sprintf("message\x00%s\x00%s\x00%s\x00%s", r.ID, seat.ID, seat.Identity, versionStamp(seat.JoinedAt)))))
}
func (s *Store) messageReceipt(ctx context.Context, r roomRow, seat seatRow) (*messageReceipt, error) {
	key := messageReceiptID(r, seat)
	call, e := langparser.RenderCall("callsMessageReceiptById", map[string]any{"id": key})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), r.OwnerUserID, "message receipt metadata", call, e)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return decodeMessageReceipt(rows[0], r, seat)
}
func decodeMessageReceipt(row map[string]any, r roomRow, seat seatRow) (*messageReceipt, error) {
	key := messageReceiptID(r, seat)
	out := &messageReceipt{ID: key, SiteID: memql.BareShortId(rowString(row, "siteId")), RoomID: memql.BareShortId(rowString(row, "roomId")), SeatID: memql.BareShortId(rowString(row, "seatId")), Identity: rowString(row, "identity"), JoinedAt: rowTime(row, "joinedAt"), ExpiresAt: rowTime(row, "expiresAt"), Version: rowTime(row, "createdAt")}
	raw := rowString(row, "receiptData")
	if len(raw) > 1024 || json.Unmarshal([]byte(raw), &out.State) != nil || out.SiteID != r.SiteID || out.RoomID != r.ID || out.SeatID != seat.ID || out.Identity != seat.Identity || !out.JoinedAt.Equal(seat.JoinedAt) || !out.ExpiresAt.Equal(r.OpenedAt.Add(25*time.Hour)) || out.State.HandSequence < 0 || out.State.HandSequence > out.State.Sequence || out.State.Sequence < 0 || out.State.Sequence > 9007199254740990 || (out.State.Sequence == 0 && out.State.Status != "ready") || (out.State.Sequence > 0 && (out.State.Status != "attempted" || len(out.State.Digest) != 64 || len(out.State.EventID) != 64)) {
		return nil, accountRefusal("messages_unavailable")
	}
	return out, nil
}
func (s *Store) saveMessageReceipt(ctx context.Context, owner string, r *messageReceipt, now time.Time) error {
	raw, e := json.Marshal(r.State)
	if e != nil || len(raw) > 1024 {
		return accountRefusal("messages_unavailable")
	}
	v := memql.VersionTimeAfter(r.Version, now)
	call, e := langparser.RenderCall("saveCallsMessageReceipt", map[string]any{"stateId": r.ID, "versionTime": versionStamp(v), "siteId": r.SiteID, "roomId": r.RoomID, "seatId": r.SeatID, "identity": r.Identity, "joinedAt": versionStamp(r.JoinedAt), "expiresAt": versionStamp(r.ExpiresAt), "receiptData": string(raw)})
	fence := absent(messageReceiptConcept, r.ID)
	if !r.Version.IsZero() {
		fence = at(messageReceiptConcept, r.ID, r.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "message receipt metadata", call, e)
	if e == nil {
		r.Version = v
	}
	return e
}

type messageRateState struct {
	At    int64 `json:"at"`
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// Version-fenced counters span all replicas. A partial write may spend quota, never grant more quota.
func (s *Store) takeMessageRateOnce(ctx context.Context, owner, scope, target string, bytes int64, now time.Time) (time.Duration, error) {
	key := string(id.NewUntracked().FromBytes([]byte("messages-rate\x00" + scope + "\x00" + target)))
	call, e := langparser.RenderCall("callsMessageRateById", map[string]any{"id": key})
	actor := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx))
	if scope == "server" {
		actor, e = clusterRowsAs(actor)
	} else {
		var ok bool
		actor, ok = rowsAs(actor, owner)
		if !ok {
			return 0, errNoOwner
		}
	}
	if e != nil {
		return 0, e
	}
	rows, e := s.read(actor, "message rate metadata", call, e)
	if e != nil {
		return 0, e
	}
	state := messageRateState{}
	var version time.Time
	if len(rows) > 0 {
		row := rows[0]
		raw := rowString(row, "rateData")
		version = rowTime(row, "createdAt")
		if rowString(row, "scope") != scope || rowString(row, "targetId") != target || len(raw) > 512 || json.Unmarshal([]byte(raw), &state) != nil || state.Count < 0 || state.Bytes < 0 {
			return 0, accountRefusal("messages_unavailable")
		}
	}
	millis := now.UnixMilli()
	if millis < state.At {
		millis = state.At
	}
	if state.At == 0 || millis-state.At >= 1000 {
		state = messageRateState{At: millis}
	}
	maxCount, maxBytes := 20, int64(2<<20)
	if scope == "server" {
		maxCount, maxBytes = 32, 8<<20
	}
	if state.Count >= maxCount || state.Bytes+bytes > maxBytes {
		return time.Duration(max(1, 1000-(millis-state.At))) * time.Millisecond, nil
	}
	state.Count++
	state.Bytes += bytes
	raw, _ := json.Marshal(state)
	v := memql.VersionTimeAfter(version, now)
	call, e = langparser.RenderCall("saveCallsMessageRate", map[string]any{"stateId": key, "versionTime": versionStamp(v), "scope": scope, "targetId": target, "rateData": string(raw)})
	fence := absent(messageRateConcept, key)
	if !version.IsZero() {
		fence = at(messageRateConcept, key, version)
	}
	actor, e = fence(actor)
	if e != nil {
		return 0, e
	}
	return 0, s.write(actor, "message rate metadata", call, e)
}

func (s *Store) takeMessageRate(ctx context.Context, owner, scope, target string, bytes int64, now time.Time) (time.Duration, error) {
	for attempt := 0; attempt < 3; attempt++ {
		wait, e := s.takeMessageRateOnce(ctx, owner, scope, target, bytes, now)
		if !errors.Is(e, memql.ErrRowVersionChanged) {
			return wait, e
		}
	}
	return 0, accountRefusal("message_budget_busy")
}
