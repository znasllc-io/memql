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

const delegationConcept = "v1:calls:delegationState"

type delegatedOperation struct {
	ID, Digest, Kind, Period, RoomID, BookingID string
	Amount                                      int64
	At                                          time.Time
	Status                                      string
}

type delegationRow struct {
	ID, SiteID, KeyID, TokenDigest string
	Version                        time.Time
	State                          delegationState
}
type delegationState struct {
	Proposals                   []delegationProposal
	ApprovedAt                  time.Time
	Request                     callrules.DelegationRequest
	CreatedAt, RevokedAt        time.Time
	CredentialID                string
	CustomerID, PaymentMethodID string // Private binding of the human-approved top-up mandate.
	Operations                  []delegatedOperation
}

func delegationFromRow(r map[string]any) (*delegationRow, error) {
	d := &delegationRow{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), KeyID: memql.BareShortId(rowString(r, "keyId")), TokenDigest: rowString(r, "tokenDigest"), Version: rowTime(r, "createdAt")}
	data := rowString(r, "stateData")
	if len(data) > 1<<20 || json.Unmarshal([]byte(data), &d.State) != nil || d.ID == "" || d.SiteID == "" || d.KeyID == "" || len(d.TokenDigest) != 64 || d.State.CredentialID == "" || !callrules.ValidDelegation(d.State.Request, delegationApprovedAt(d)) || len(d.State.Operations) > 1000 || len(d.State.Proposals) > 20 {
		return nil, accountRefusal("delegation_state_unavailable")
	}
	for _, op := range d.State.Operations {
		if op.ID == "" || op.Amount < 0 || op.Amount > 50000 || (op.Kind != "usage" && op.Kind != "topup") {
			return nil, accountRefusal("delegation_state_unavailable")
		}
	}
	return d, nil
}
func (s *Store) delegation(ctx context.Context, owner, id string) (*delegationRow, error) {
	call, err := langparser.RenderCall("callsDelegationById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the delegated authority", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return delegationFromRow(rows[0])
}
func (s *Store) delegations(ctx context.Context, owner, site, key string) ([]*delegationRow, error) {
	call, err := langparser.RenderCall("callsDelegationsForKey", map[string]any{"siteId": site, "keyId": key})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the account's delegated authorities", call, err)
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, accountRefusal("delegation_limit")
	}
	out := make([]*delegationRow, 0, len(rows))
	for _, r := range rows {
		d, e := delegationFromRow(r)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, nil
}
func (s *Store) saveDelegation(ctx context.Context, owner string, d *delegationRow, now time.Time) error {
	data, err := json.Marshal(d.State)
	if err != nil || len(data) > 1<<20 {
		return accountRefusal("delegation_limit")
	}
	version := memql.VersionTimeAfter(d.Version, now)
	call, err := langparser.RenderCall("saveCallsDelegation", map[string]any{"grantId": d.ID, "versionTime": versionStamp(version), "siteId": d.SiteID, "keyId": d.KeyID, "tokenDigest": d.TokenDigest, "stateData": string(data)})
	fence := absent(delegationConcept, d.ID)
	if !d.Version.IsZero() {
		fence = at(delegationConcept, d.ID, d.Version)
	}
	err = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "the delegated authority", call, err)
	if err == nil {
		d.Version = version
	}
	return err
}

func delegationUsage(d *delegationRow, kind string, now time.Time) (period, total int64) {
	for _, op := range d.State.Operations {
		if op.Kind == kind {
			total += op.Amount
			if op.Period == callrules.DelegationPeriod(now) || op.Status != "settled" {
				period += op.Amount
			}
		}
	}
	return
}
func delegationInfo(d *delegationRow, now time.Time) callrules.DelegationInfo {
	p, t := delegationUsage(d, "usage", now)
	fp, ft := delegationUsage(d, "topup", now)
	status := "active"
	if !d.State.RevokedAt.IsZero() {
		status = "revoked"
	} else if !now.Before(d.State.Request.ExpiresAt) {
		status = "expired"
	}
	r := d.State.Request
	r.Action = ""
	return callrules.DelegationInfo{DelegationRequest: r, ID: d.ID, Status: status, CreatedAt: d.State.CreatedAt, SpentCents: t, PeriodSpentCents: p, TopupSpentCents: ft, TopupPeriodSpentCents: fp}
}

func delegationApprovedAt(d *delegationRow) time.Time {
	if !d.State.ApprovedAt.IsZero() {
		return d.State.ApprovedAt
	}
	return d.State.CreatedAt
}
