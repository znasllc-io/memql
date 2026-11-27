package calls

import (
	"context"
	"encoding/json"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const mediaSignalConcept = "v1:calls:mediaSignalState"
const maxMediaSignalIntents = 128

// Only metadata is retained: no bearer/JWT, upstream URL, media or device secret.
// Unknown handshakes NEVER expire into successful cleanup. The bounded journal
// refuses further admission instead of forgetting an uncertain provider effect.
type mediaSignalJournal struct {
	ID, SiteID, RoomID string
	Version            time.Time `json:"-"`
	Intents            []mediaSignalIntent
	LastVerifiedAt     time.Time
	LastCleanupReason  string
}

// Status polling never takes write gates or performs provider I/O. A clear
// projection requires a recent successful provider audit and still-current
// native bindings. Family authority can perform guarded repairs, so the recent
// gated audit supplies that evidence while this read checks session/admission.
func (s *Service) mediaCleanupStatus(ctx context.Context, r roomRow) callrules.MediaCleanupStatus {
	out := callrules.MediaCleanupStatus{State: "pending", Reason: "media_cleanup_unverified"}
	j, e := s.store.mediaSignals(ctx, r)
	if e != nil {
		return out
	}
	if j.LastCleanupReason != "" {
		out.Reason = j.LastCleanupReason
	}
	for _, v := range j.Intents {
		if v.State != "active" || !s.now().Before(v.ExpiresAt) {
			out.PendingSignals++
			if v.State == "uncertain" {
				out.Reason = "handshake_outcome_unknown"
			}
			continue
		}
		if s.checkMediaBindingCurrent(ctx, r, v.Binding, false) != nil {
			out.PendingSignals++
			out.Reason = "native_authority_changed"
		}
	}
	if out.PendingSignals == 0 && j.LastCleanupReason == "" && !j.LastVerifiedAt.IsZero() && !s.now().Before(j.LastVerifiedAt) && s.now().Sub(j.LastVerifiedAt) < mediaSignalLeaseTTL {
		out.State = "clear"
		out.Reason = ""
	}
	return out
}

type mediaSignalIntent struct {
	ID, State            string
	GrantDigest          string
	Grant                callrules.GrantSpec
	Binding              mediaSignalBinding
	CreatedAt, ExpiresAt time.Time
}

func (j *mediaSignalJournal) find(id string) *mediaSignalIntent {
	for i := range j.Intents {
		if j.Intents[i].ID == id {
			return &j.Intents[i]
		}
	}
	return nil
}
func (j *mediaSignalJournal) remove(id string) {
	for i := range j.Intents {
		if j.Intents[i].ID == id {
			j.Intents = append(j.Intents[:i], j.Intents[i+1:]...)
			return
		}
	}
}
func (s *Store) mediaSignals(ctx context.Context, r roomRow) (*mediaSignalJournal, error) {
	call, e := langparser.RenderCall("callsMediaSignalsById", map[string]any{"id": r.ID})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), r.OwnerUserID, "media signalling barriers", call, e)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return &mediaSignalJournal{ID: r.ID, RoomID: r.ID, SiteID: r.SiteID}, nil
	}
	row := rows[0]
	j := &mediaSignalJournal{}
	raw := rowString(row, "stateData")
	if len(raw) > 262144 || json.Unmarshal([]byte(raw), j) != nil || j.ID != r.ID || j.RoomID != r.ID || j.SiteID != r.SiteID || len(j.Intents) > maxMediaSignalIntents {
		return nil, accountRefusal("media_barrier_unavailable")
	}
	seen := map[string]bool{}
	for _, v := range j.Intents {
		if v.ID == "" || seen[v.ID] || !v.Binding.valid() || v.Binding.RoomID != r.ID || !v.ExpiresAt.After(v.CreatedAt) {
			return nil, accountRefusal("media_barrier_unavailable")
		}
		if v.State != "pending" && v.State != "active" && v.State != "uncertain" && v.State != "draining" {
			return nil, accountRefusal("media_barrier_unavailable")
		}
		if v.Grant.RoomJoin {
			if _, e := nativeParticipantPermission(v.Grant); e != nil || mediaGrantDigest(v.Grant) != v.GrantDigest {
				return nil, accountRefusal("media_barrier_unavailable")
			}
		}
		seen[v.ID] = true
	}
	j.Version = rowTime(row, "createdAt")
	return j, nil
}
func (s *Store) saveMediaSignals(ctx context.Context, r roomRow, j *mediaSignalJournal, now time.Time) error {
	raw, e := json.Marshal(j)
	if e != nil || len(raw) > 262144 || len(j.Intents) > maxMediaSignalIntents {
		return accountRefusal("media_barrier_limit")
	}
	v := memql.VersionTimeAfter(j.Version, now)
	call, e := langparser.RenderCall("saveCallsMediaSignals", map[string]any{"stateId": j.ID, "versionTime": versionStamp(v), "siteId": j.SiteID, "roomId": j.RoomID, "stateData": string(raw)})
	fence := absent(mediaSignalConcept, j.ID)
	if !j.Version.IsZero() {
		fence = at(mediaSignalConcept, j.ID, j.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), r.OwnerUserID, fence, "media signalling barriers", call, e)
	if e == nil {
		j.Version = v
	}
	return e
}

func (s *Store) mediaSeatSession(ctx context.Context, r roomRow, seat seatRow, now time.Time) (*sessionRow, error) {
	call, e := langparser.RenderCall("callsMediaSessionsForSeat", map[string]any{"seatId": seat.ID})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(ctx), r.OwnerUserID, "the media admission's session", call, e)
	if e != nil {
		return nil, e
	}
	if len(rows) > 8 {
		return nil, accountRefusal("media_session_unavailable")
	}
	for _, row := range rows {
		v, e := s.checkedSessionRow(ctx, r.OwnerUserID, sessionFromRow(row))
		if e != nil {
			return nil, e
		}
		if v != nil && v.usable(now) && v.RoomID == r.ID && v.SeatID == seat.ID && v.SiteID == r.SiteID && v.KeyID == seat.KeyID && v.CredentialID == seat.CredentialID {
			return v, nil
		}
	}
	return nil, accountRefusal("media_session_unavailable")
}
