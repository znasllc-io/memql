package calls

import (
	"context"
	"encoding/json"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const spotlightChangesPerMinute = 30

// A single bounded room value, not an unbounded operation log. Replaying a
// superseded operation conflicts with its old revision; it cannot undo a newer
// selection. The newest operation may replay without another room version.
type spotlightState struct {
	Revision                 string
	Seat, Identity           string
	JoinedAt                 time.Time
	RequestID, RequestDigest string
	Window                   callrules.JoinWindow
}

func readSpotlight(raw string) (spotlightState, error) {
	state := spotlightState{Revision: "0"}
	if raw == "" {
		return state, nil
	}
	if len(raw) > 2048 || json.Unmarshal([]byte(raw), &state) != nil || state.Revision == "" || len(state.Revision) > 128 || len(state.Seat) > 128 || len(state.Identity) > 128 || (state.Seat == "") != (state.Identity == "") || (state.Seat != "" && state.JoinedAt.IsZero()) || !fundingOperationID.MatchString(state.RequestID) || len(state.RequestDigest) != 64 || state.Window.Count < 0 || state.Window.Count > spotlightChangesPerMinute {
		return spotlightState{}, accountRefusal("spotlight_unavailable")
	}
	return state, nil
}

func validSpotlightRequest(r *callrules.SpotlightRequest) bool {
	return r != nil && r.RoomID != "" && len(r.RoomID) <= 128 && r.ActorIdentity != "" && len(r.ActorIdentity) <= 128 && r.Revision != "" && len(r.Revision) <= 128 && fundingOperationID.MatchString(r.RequestID) && len(r.Seat) <= 128 && len(r.Identity) <= 128 && (r.Seat == "") == (r.Identity == "")
}

func spotlightParticipant(state spotlightState, seats []seatRow) *callrules.SpotlightParticipant {
	for _, seat := range seats {
		if seat.ID == state.Seat && seat.Identity == state.Identity && seat.JoinedAt.Equal(state.JoinedAt) && seat.occupying() {
			return &callrules.SpotlightParticipant{Seat: seat.ID, Identity: seat.Identity, DisplayName: seat.DisplayName}
		}
	}
	return nil
}

func spotlightView(room roomRow, seats []seatRow) (*callrules.Spotlight, error) {
	state, err := readSpotlight(room.SpotlightData)
	if err != nil {
		return nil, err
	}
	return &callrules.Spotlight{Revision: state.Revision, Participant: spotlightParticipant(state, seats)}, nil
}

func (s *Service) setSpotlight(ctx context.Context, sess callrules.Session, req callrules.HostActionRequest) error {
	if !validSpotlightRequest(req.Spotlight) || req.Seat != "" || req.RequestID != "" || req.ReviewDigest != "" || req.Processing != nil {
		return callrules.ErrInvalid
	}
	r := *req.Spotlight
	h, err := s.hostOf(ctx, sess)
	if err != nil {
		return err
	}
	if r.RoomID != h.room.ID || r.ActorIdentity != h.seat.Identity {
		return accountRefusal("spotlight_admission_changed")
	}
	gates := []string{roomGate(h.room.ID)}
	if h.seat.KeyID != "" {
		gates = append(gates, accountGate(h.seat.KeyID), keyGate(h.seat.KeyID))
	}
	release, err := s.gates(ctx, gates...)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.stillHost(fresh, h)
	if err != nil {
		return err
	}
	state, err := readSpotlight(room.SpotlightData)
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(r)
	digest := callrules.HistoryDigest(nil, "shared-spotlight", h.session.ID, h.seat.ID, string(encoded))
	if state.RequestID == r.RequestID {
		if state.RequestDigest != digest {
			return fundingConflict("idempotency_conflict")
		}
		return nil
	}
	if state.Revision != r.Revision {
		return fundingConflict("spotlight_changed")
	}
	var target seatRow
	if r.Seat != "" {
		target, err = s.targetIn(fresh, h, r.Seat)
		if err != nil {
			return err
		}
		if !target.occupying() || target.Identity != r.Identity {
			return fundingConflict("spotlight_target_changed")
		}
	}
	if state.Seat == r.Seat && state.Identity == r.Identity && (r.Seat == "" || state.JoinedAt.Equal(target.JoinedAt)) {
		return nil
	}
	window, allowed, _ := callrules.CountJoinAttempt(state.Window, s.now(), spotlightChangesPerMinute, time.Minute)
	if !allowed {
		return fundingConflict("spotlight_rate_limited")
	}
	state = spotlightState{Revision: id.NewShortId(), Seat: r.Seat, Identity: r.Identity, JoinedAt: target.JoinedAt,
		RequestID: r.RequestID, RequestDigest: digest, Window: window}
	data, _ := json.Marshal(state)
	return s.store.updateRoom(fresh, h.site.OwnerUserID, room, map[string]any{"spotlightData": string(data)}, memql.VersionTimeAfter(room.Version, s.now()))
}
