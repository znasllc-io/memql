package calls

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const observationConcept = "v1:calls:observationState"
const observationMaxPermissions = 64

type observationParty struct {
	callrules.ObservationPerson
	SeatID, KeyID, CredentialID, SessionID string
	GrantID, ApprovalDigest, ReviewerKeyID string
	JoinedAt                               time.Time
}
type observationOperation struct {
	Key, Digest, Actor, Action string
	Receipt                    callrules.ObservationReceipt
	At                         time.Time
}
type observationPermission struct {
	ID, Purpose, Status               string
	Revision                          int64
	Observer, Subject                 observationParty
	Recipients                        []observationParty
	CreatedAt, ExpiresAt, PublishedAt time.Time
	PublishedCount                    int
	Operations                        []observationOperation
}
type observationJournal struct {
	ID, SiteID, RoomID string
	Version            time.Time `json:"-"`
	Permissions        []observationPermission
}

func validObservationParty(p observationParty) bool {
	return callrules.ValidWorkflowID(p.SeatID) && p.KeyID != "" && p.Identity != "" && len(p.Identity) <= 128 && !p.JoinedAt.IsZero() && slices.Contains([]string{"person", "agent", "avatar"}, p.Kind) && (p.Kind == "person" && p.GrantID == "" || p.GrantID != "" && callrules.QuestionDigest(p.ApprovalDigest))
}
func validObservationJournal(j *observationJournal) bool {
	if j.ID == "" || j.RoomID != j.ID || j.SiteID == "" || len(j.Permissions) > observationMaxPermissions {
		return false
	}
	seen := map[string]bool{}
	for _, p := range j.Permissions {
		if !callrules.ValidWorkflowID(p.ID) || seen[p.ID] || !callrules.ValidAgentPurpose(p.Purpose) || !validObservationParty(p.Observer) || !validObservationParty(p.Subject) || p.Observer.SessionID == "" || !slices.Contains([]string{"pending", "approved", "withdrawn"}, p.Status) || p.Revision < 1 || p.Revision > 3 || p.CreatedAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) || p.ExpiresAt.After(p.CreatedAt.Add(5*time.Minute)) || len(p.Recipients) < 2 || len(p.Recipients) > 8 || p.PublishedCount < 0 || p.PublishedCount > 20 || len(p.Operations) < 1 || len(p.Operations) > 23 {
			return false
		}
		seen[p.ID] = true
		if p.Status == "pending" && p.Revision != 1 || p.Status == "approved" && (p.Revision != 2 || p.Subject.SessionID == "") || p.Status == "withdrawn" && p.Revision < 2 {
			return false
		}
		recipients := map[string]bool{}
		for _, person := range p.Recipients {
			if !validObservationParty(person) || recipients[person.Identity] {
				return false
			}
			recipients[person.Identity] = true
		}
		if !recipients[p.Observer.Identity] || !recipients[p.Subject.Identity] {
			return false
		}
		for _, op := range p.Operations {
			if !callrules.ValidWorkflowID(op.Key) || !callrules.QuestionDigest(op.Digest) || !callrules.QuestionDigest(op.Actor) || !callrules.ObservationAction(op.Action) || op.Receipt.ID != p.ID || op.Receipt.Revision < 1 || op.Receipt.Revision > 3 || op.At.Before(p.CreatedAt) {
				return false
			}
			if op.Action == "observation_publish" && (!callrules.QuestionDigest(op.Receipt.EventID) || op.Receipt.Status != "attempted") || op.Action != "observation_publish" && (op.Receipt.EventID != "" || !slices.Contains([]string{"pending", "approved", "withdrawn"}, op.Receipt.Status)) {
				return false
			}
		}
	}
	return true
}
func (s *Store) observationState(ctx context.Context, owner, site, room string) (*observationJournal, error) {
	call, e := langparser.RenderCall("callsObservationById", map[string]any{"id": room})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "observation consent metadata", call, e)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return &observationJournal{ID: room, SiteID: site, RoomID: room, Permissions: []observationPermission{}}, nil
	}
	if len(rows) != 1 {
		return nil, accountRefusal("observations_unavailable")
	}
	raw := rowString(rows[0], "stateData")
	var j observationJournal
	if len(raw) > 524288 || json.Unmarshal([]byte(raw), &j) != nil || !validObservationJournal(&j) || j.ID != room || j.SiteID != site || memql.BareShortId(rowString(rows[0], "siteId")) != site || memql.BareShortId(rowString(rows[0], "roomId")) != room {
		return nil, accountRefusal("observations_unavailable")
	}
	j.Version = rowTime(rows[0], "createdAt")
	return &j, nil
}
func (s *Store) saveObservation(ctx context.Context, owner string, j *observationJournal, now time.Time) error {
	raw, e := json.Marshal(j)
	if e != nil || len(raw) > 524288 || !validObservationJournal(j) {
		return accountRefusal("observations_limit")
	}
	v := memql.VersionTimeAfter(j.Version, now)
	call, e := langparser.RenderCall("saveCallsObservation", map[string]any{"stateId": j.ID, "versionTime": versionStamp(v), "siteId": j.SiteID, "roomId": j.RoomID, "stateData": string(raw)})
	fence := absent(observationConcept, j.ID)
	if !j.Version.IsZero() {
		fence = at(observationConcept, j.ID, j.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "observation consent metadata", call, e)
	if e == nil {
		j.Version = v
	}
	return e
}
