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

const processingConcept = "v1:calls:processingState"
const processingTTL = 30 * time.Second

// Durable metadata only: no tokens, media, model output or transcript text.
type processingJournal struct {
	Reason             string
	ID, SiteID, RoomID string
	Version            time.Time `json:"-"`
	Epoch              int64
	Consents           map[string]processingConsent
	Lease              processingLeaseState
	RateSecond         int64
	Receipts           map[string]processingReceipt
	RateCount          int
}
type processingConsent struct {
	callrules.ProcessingConsent
	Identity                string
	JoinedAt                time.Time
	GrantID, ApprovalDigest string
}
type processingLeaseState struct {
	ID, Identity, Owner string
	ExpiresAt           time.Time
	Tracks              []processingTrack
}
type processingTrack struct {
	RoomID               string `json:"room_id"`
	ParticipantID        string `json:"participant_id"`
	MediaIdentity        string `json:"media_identity"`
	TrackID              string `json:"track_id"`
	DisplayName          string `json:"display_name"`
	Epoch                int64  `json:"epoch"`
	ConsentRevision      string `json:"consent_revision"`
	ExpiresAt            int64  `json:"expires_at"`
	Source               string `json:"source"`
	SafetyAllowed        bool   `json:"safety_allowed"`
	TranscriptionAllowed bool   `json:"transcription_allowed"`
	InsightsAllowed      bool   `json:"insights_allowed"`
}

func (s *Store) processing(ctx context.Context, owner, room string) (*processingJournal, error) {
	call, e := langparser.RenderCall("callsProcessingById", map[string]any{"id": room})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "processing authority", call, e)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	r := rows[0]
	raw := rowString(r, "stateData")
	var j processingJournal
	if len(raw) > 524288 || json.Unmarshal([]byte(raw), &j) != nil || j.ID != room || j.RoomID != room || j.SiteID != memql.BareShortId(rowString(r, "siteId")) || len(j.Consents) > 128 || len(j.Lease.Tracks) > 300 || j.Epoch < 0 {
		return nil, accountRefusal("processing_unavailable")
	}
	j.Version = rowTime(r, "createdAt")
	if j.Consents == nil {
		j.Consents = map[string]processingConsent{}
	}
	return &j, nil
}
func (s *Store) saveProcessing(ctx context.Context, owner string, j *processingJournal, now time.Time) error {
	raw, e := json.Marshal(j)
	if e != nil || len(raw) > 524288 || len(j.Consents) > 128 || len(j.Lease.Tracks) > 300 {
		return accountRefusal("processing_limit")
	}
	v := memql.VersionTimeAfter(j.Version, now)
	call, e := langparser.RenderCall("saveCallsProcessing", map[string]any{"stateId": j.ID, "versionTime": versionStamp(v), "siteId": j.SiteID, "roomId": j.RoomID, "stateData": string(raw)})
	fence := absent(processingConcept, j.ID)
	if !j.Version.IsZero() {
		fence = at(processingConcept, j.ID, j.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "processing authority", call, e)
	if e == nil {
		j.Version = v
	}
	return e
}
func newProcessing(r roomRow) *processingJournal {
	return &processingJournal{ID: r.ID, RoomID: r.ID, SiteID: r.SiteID, Consents: map[string]processingConsent{}}
}
func (j *processingJournal) consent(s seatRow) processingConsent {
	c := j.Consents[s.ID]
	if c.Identity != s.Identity || !c.JoinedAt.Equal(s.JoinedAt) {
		return processingConsent{ProcessingConsent: callrules.ProcessingConsent{Revision: "0"}}
	}
	return c
}
func (j *processingJournal) revoke() { j.Reason = ""; j.Epoch++; j.Lease = processingLeaseState{} }
