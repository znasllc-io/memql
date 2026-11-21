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

const agentAdmissionConcept = "v1:calls:agentAdmissionState"

// One bounded room journal, fenced under roomGate across all replicas. Retention
// deletes every version after 30 days. Raw links, credentials and media are absent.
type agentAdmissionJournal struct {
	ID, SiteID, RoomID string
	Version            time.Time
	Requests           []agentAdmission
}
type agentAdmission struct {
	PortraitID, PortraitDigest                               string
	ID, GrantID, ApprovalDigest, RequestDigest, ReviewDigest string
	ApprovalRevision                                         int
	Name, Purpose, ConductVersion, Terms, Status             string
	CreatedAt, ExpiresAt                                     time.Time
	ReviewerCredential, ReviewerKeyID, AdmissionKey          string
	SeatID                                                   string
	JoinedAt                                                 time.Time
}

func (s *Store) agentAdmissions(ctx context.Context, owner, room string) (*agentAdmissionJournal, error) {
	call, e := langparser.RenderCall("callsAgentAdmissionById", map[string]any{"id": room})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "agent admission state", call, e)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	r := rows[0]
	j := &agentAdmissionJournal{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), RoomID: memql.BareShortId(rowString(r, "roomId")), Version: rowTime(r, "createdAt")}
	raw := rowString(r, "stateData")
	if len(raw) > 262144 || json.Unmarshal([]byte(raw), &j.Requests) != nil || len(j.Requests) > 128 || j.ID != room || j.RoomID != room || j.SiteID == "" {
		return nil, accountRefusal("agent_admission_unavailable")
	}
	return j, nil
}
func (s *Store) saveAgentAdmissions(ctx context.Context, owner string, j *agentAdmissionJournal, now time.Time) error {
	raw, e := json.Marshal(j.Requests)
	if e != nil || len(raw) > 262144 || len(j.Requests) > 128 {
		return accountRefusal("agent_admission_limit")
	}
	version := memql.VersionTimeAfter(j.Version, now)
	call, e := langparser.RenderCall("saveCallsAgentAdmission", map[string]any{"stateId": j.ID, "versionTime": versionStamp(version), "siteId": j.SiteID, "roomId": j.RoomID, "stateData": string(raw)})
	fence := absent(agentAdmissionConcept, j.ID)
	if !j.Version.IsZero() {
		fence = at(agentAdmissionConcept, j.ID, j.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "agent admission state", call, e)
	if e == nil {
		j.Version = version
	}
	return e
}
func (j *agentAdmissionJournal) find(id string) *agentAdmission {
	if j != nil {
		for i := range j.Requests {
			if j.Requests[i].ID == id {
				return &j.Requests[i]
			}
		}
	}
	return nil
}
func (a agentAdmission) public() callrules.PendingAgent {
	return callrules.PendingAgent{RequestID: a.ID, ReviewDigest: a.ReviewDigest, Name: a.Name, Purpose: a.Purpose, ConductVersion: a.ConductVersion, ApprovalRevision: a.ApprovalRevision, ExpiresAt: a.ExpiresAt, Media: callrules.CurrentAgentPolicy().Media, PortraitID: a.PortraitID, PortraitDigest: a.PortraitDigest}
}
func (a agentAdmission) result(room string, now time.Time) callrules.AgentJoinStatus {
	status := a.Status
	if (status == "pending" || status == "approved") && !now.Before(a.ExpiresAt) {
		status = "expired"
	}
	return callrules.AgentJoinStatus{CallID: room, RequestID: a.ID, Status: status, ExpiresAt: a.ExpiresAt}
}
