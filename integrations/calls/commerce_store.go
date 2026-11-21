package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

const commerceConcept = "v1:calls:commerceState"
const commerceGlobalGate = "calls.commerce.admission"
const commerceJournalLimit = 128

type commerceOperation struct {
	PaymentID     string    `json:"paymentId,omitempty"`
	PaidRecorded  bool      `json:"paidRecorded,omitempty"`
	CapacityUntil time.Time `json:"capacityUntil,omitempty"`
	ID            string    `json:"id"`
	RequestDigest string    `json:"requestDigest"`
	Kind          string    `json:"kind"`
	ProviderID    string    `json:"providerId,omitempty"`
	Status        string    `json:"status"`
	URL           string    `json:"url,omitempty"`
	AmountCents   int64     `json:"amountCents,omitempty"`
	CostCents     int64     `json:"costCents,omitempty"`
	SaveMethod    bool      `json:"saveMethod,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	// DispatchClaimed is durable BEFORE an external call. An unknown create is
	// never retried automatically: Didit does not promise idempotent creation.
	DispatchClaimed bool `json:"dispatchClaimed,omitempty"`
}
type commerceState struct {
	AdultWorkflowID      string                  `json:"adultWorkflowId,omitempty"`
	AdultWorkflowVersion int                     `json:"adultWorkflowVersion,omitempty"`
	FinancialHold        bool                    `json:"financialHold,omitempty"`
	ID, SiteID, KeyID    string                  `json:"-"`
	Version              time.Time               `json:"-"`
	Mode                 providers.Mode          `json:"mode"`
	Adult                callrules.AdultEvidence `json:"adult"`
	AdultReference       string                  `json:"adultReference,omitempty"`
	PaidCents            int64                   `json:"paidCents"`
	CustomerID           string                  `json:"customerId,omitempty"`
	PaymentMethodID      string                  `json:"paymentMethodId,omitempty"`
	Operations           []commerceOperation     `json:"operations"`
}

func commerceID(site, key string) string {
	return callrules.HistoryDigest(nil, "commerce-account", site, key)[:32]
}
func newCommerceState(site, key string, mode providers.Mode) *commerceState {
	return &commerceState{ID: commerceID(site, key), SiteID: site, KeyID: key, Mode: mode, Adult: callrules.AdultEvidence{Status: "unverified", Provider: "didit"}, Operations: []commerceOperation{}}
}
func commerceFromRow(row map[string]any) (*commerceState, error) {
	var v commerceState
	raw := rowString(row, "stateData")
	if len(raw) > 524288 || json.Unmarshal([]byte(raw), &v) != nil || len(v.Operations) > commerceJournalLimit || v.PaidCents < 0 || v.PaidCents > commerceJournalLimit*50000 {
		return nil, allowanceUnavailable()
	}
	v.ID = memql.BareShortId(rowString(row, "id"))
	v.SiteID = memql.BareShortId(rowString(row, "siteId"))
	v.KeyID = memql.BareShortId(rowString(row, "keyId"))
	v.Version = rowTime(row, "createdAt")
	if v.ID != commerceID(v.SiteID, v.KeyID) || v.Version.IsZero() || (v.Mode != providers.Mock && v.Mode != providers.Sandbox) {
		return nil, allowanceUnavailable()
	}
	if v.Adult.Status != "unverified" && v.Adult.Status != "verified" && v.Adult.Status != "blocked" && v.Adult.Status != "recheck_required" {
		return nil, allowanceUnavailable()
	}
	seen := map[string]bool{}
	for _, o := range v.Operations {
		if o.ID == "" || seen[o.ID] || len(o.RequestDigest) != 64 || o.CreatedAt.IsZero() || o.AmountCents < 0 || o.CostCents < 0 || (o.Kind != "checkout" && o.Kind != "verification" && o.Kind != "auto_topup") {
			return nil, allowanceUnavailable()
		}
		if o.CostCents > 1000000000 || (o.Kind == "verification" && (o.AmountCents != 0 || o.CostCents == 0)) || (o.Kind != "verification" && (o.AmountCents < 1000 || o.AmountCents > 50000)) {
			return nil, allowanceUnavailable()
		}
		seen[o.ID] = true
	}
	return &v, nil
}
func (s *Store) commerceState(ctx context.Context, owner, site, key string) (*commerceState, error) {
	call, err := langparser.RenderCall("callsCommerceStateById", map[string]any{"id": commerceID(site, key)})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the private commerce account", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	v, err := commerceFromRow(rows[0])
	if err == nil && (v.SiteID != site || v.KeyID != key) {
		return nil, allowanceUnavailable()
	}
	return v, err
}
func (s *Store) saveCommerce(ctx context.Context, owner string, v *commerceState, now time.Time) error {
	data, err := json.Marshal(v)
	if err != nil || len(data) > 524288 {
		return allowanceUnavailable()
	}
	version := memql.VersionTimeAfter(v.Version, now)
	args := map[string]any{"stateId": v.ID, "stateData": string(data), "versionTime": versionStamp(version)}
	name := "updateCallsCommerceState"
	fence := at(commerceConcept, v.ID, v.Version)
	if v.Version.IsZero() {
		name = "createCallsCommerceState"
		fence = absent(commerceConcept, v.ID)
		args["siteId"] = v.SiteID
		args["keyId"] = v.KeyID
	}
	call, err := langparser.RenderCall(name, args)
	if err = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "the private commerce account", call, err); err == nil {
		v.Version = version
	}
	return err
}
func (s *Store) commerceAll(ctx context.Context) ([]*commerceState, error) {
	call, err := langparser.RenderCall("callsCommerceStates", nil)
	rows, err := s.readAsCluster(auth.ContextWithInternalOrigin(ctx), "the global commerce commitments", call, err)
	if err != nil {
		return nil, err
	}
	if len(rows) >= 1000 {
		return nil, allowanceUnavailable()
	}
	out := make([]*commerceState, 0, len(rows))
	for _, row := range rows {
		v, e := commerceFromRow(row)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Store) commerceRooms(ctx context.Context) ([]roomRow, error) {
	call, err := langparser.RenderCall("callsCommerceRooms", nil)
	rows, err := s.readAsCluster(auth.ContextWithInternalOrigin(ctx), "the global call commitments", call, err)
	if err != nil {
		return nil, err
	}
	if len(rows) >= 1000 {
		return nil, allowanceUnavailable()
	}
	out := make([]roomRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, roomFromRow(row))
	}
	return out, nil
}
func (s *Store) commerceAccountCount(ctx context.Context) (int, error) {
	call, err := langparser.RenderCall("callsCommerceAccounts", nil)
	rows, err := s.readAsCluster(auth.ContextWithInternalOrigin(ctx), "the global private-account count", call, err)
	if err != nil {
		return 0, err
	}
	if len(rows) >= 1000 {
		return 0, allowanceUnavailable()
	}
	return len(rows), nil
}
func (v *commerceState) operation(id string) *commerceOperation {
	for i := range v.Operations {
		if v.Operations[i].ID == id {
			return &v.Operations[i]
		}
	}
	return nil
}
func verificationPending(status string) bool {
	return status == "not_started" || status == "in_progress" || status == "in_review" || status == "dispatching" || status == "unknown"
}
func paymentPending(status string) bool {
	return status == "pending" || status == "requires_action" || status == "dispatching" || status == "unknown"
}
func (v *commerceState) verificationCosts() (attempts int, reserved, spent int64) {
	for _, o := range v.Operations {
		if o.Kind == "verification" {
			attempts++
			if verificationPending(o.Status) {
				reserved += o.CostCents
			} else {
				spent += o.CostCents
			}
		}
	}
	return
}
func (v *commerceState) latest(kind string) *commerceOperation {
	for i := len(v.Operations) - 1; i >= 0; i-- {
		if v.Operations[i].Kind == kind {
			return &v.Operations[i]
		}
	}
	return nil
}
func (v *commerceState) validateMode(c CommerceConfig) error {
	if v.Mode != c.Mode {
		return accountRefusal("provider_mode_changed")
	}
	return nil
}
func commerceUnknown(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: provider outcome requires reconciliation", callrules.Refuse(callrules.ErrConflict, "provider_pending", "a provider operation is unresolved"))
}
