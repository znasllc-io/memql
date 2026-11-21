package calls

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"time"
)

const familyConcept = "v1:calls:familyState"

// Family state contains opaque browser bindings, never a child's name, age,
// document or media. A mock receipt is a separate record from adult evidence.
type familyState struct {
	ID, SiteID, KeyID, RoomID, Kind, TokenDigest, SessionID string
	Version                                                 time.Time
	Data                                                    familyData
}
type familyData struct {
	Status                                                    string
	ExpiresAt, ApprovedAt, RevokedAt, ClosingAt               time.Time
	ConsentVersion, ApprovalCredential, RequestDigest, Reason string
	GuardianSeatID                                            string
	GuardianSeen                                              bool
	GuardianSID                                               string
	GuardianEventAt                                           time.Time
}

func familyFromRow(r map[string]any) (*familyState, error) {
	v := &familyState{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), KeyID: memql.BareShortId(rowString(r, "keyId")), RoomID: memql.BareShortId(rowString(r, "roomId")), Kind: rowString(r, "kind"), TokenDigest: rowString(r, "tokenDigest"), SessionID: memql.BareShortId(rowString(r, "sessionId")), Version: rowTime(r, "createdAt")}
	data := rowString(r, "stateData")
	if len(data) > 65536 || json.Unmarshal([]byte(data), &v.Data) != nil || v.ID == "" || v.RoomID == "" || v.KeyID == "" || v.SiteID == "" || (v.Kind != "room" && v.Kind != "invitation") {
		return nil, accountRefusal("family_state_unavailable")
	}
	return v, nil
}
func (s *Store) familyRows(ctx context.Context, owner, query string, args map[string]any) ([]*familyState, error) {
	call, err := langparser.RenderCall(query, args)
	rows, err := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "family consent state", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]*familyState, 0, len(rows))
	for _, r := range rows {
		v, e := familyFromRow(r)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Store) family(ctx context.Context, owner, id string) (*familyState, error) {
	rows, err := s.familyRows(ctx, owner, "callsFamilyById", map[string]any{"id": id})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}
func (s *Store) familyForSession(ctx context.Context, owner, site, session string) (*familyState, error) {
	rows, err := s.familyRows(ctx, owner, "callsFamilyForSession", map[string]any{"siteId": site, "sessionId": session})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, accountRefusal("family_state_unavailable")
	}
	return rows[0], nil
}
func (s *Store) saveFamily(ctx context.Context, owner string, v *familyState, now time.Time) error {
	data, err := json.Marshal(v.Data)
	if err != nil || len(data) > 65536 {
		return accountRefusal("family_state_unavailable")
	}
	version := memql.VersionTimeAfter(v.Version, now)
	call, err := langparser.RenderCall("saveCallsFamily", map[string]any{"stateId": v.ID, "versionTime": versionStamp(version), "siteId": v.SiteID, "keyId": v.KeyID, "roomId": v.RoomID, "kind": v.Kind, "tokenDigest": v.TokenDigest, "sessionId": v.SessionID, "stateData": string(data)})
	fence := absent(familyConcept, v.ID)
	if !v.Version.IsZero() {
		fence = at(familyConcept, v.ID, v.Version)
	}
	err = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "family consent state", call, err)
	if err == nil {
		v.Version = version
	}
	return err
}
