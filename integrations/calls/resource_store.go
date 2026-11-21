package calls

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const resourceConcept = "v1:calls:resourceState"

type resourceLease struct {
	ID, ResourceID, RoomID, SeatID, Identity, IngressID, RequestID, Digest, GrantID string
	CreatedAt, ExpiresAt                                                            time.Time
	Status                                                                          string
	StoppedReason                                                                   string
}

func (l resourceLease) active() bool { return l.Status != "stopped" }

type resourceState struct {
	OwnerUserID string
	Sources     []callrules.ResourceInfo
	Leases      []resourceLease
	// Registration retries are keyed without retaining credentials or device addresses.
	Registrations map[string]string
}
type resourceRow struct {
	ID, SiteID, KeyID string
	Version           time.Time
	State             resourceState
	ProjectionKnown   bool
}

func resourceStateID(site, key string) string {
	return callrules.HistoryDigest(nil, "resource-account", site, key)[:32]
}
func resourceFromRow(r map[string]any) (*resourceRow, error) {
	d := &resourceRow{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), KeyID: memql.BareShortId(rowString(r, "keyId")), Version: rowTime(r, "createdAt")}
	data := rowString(r, "stateData")
	if len(data) > 1<<20 || json.Unmarshal([]byte(data), &d.State) != nil || d.ID != resourceStateID(d.SiteID, d.KeyID) || d.State.OwnerUserID == "" || len(d.State.Sources) > callrules.MaxResources || len(d.State.Leases) > callrules.MaxResourceLeases || len(d.State.Registrations) > 128 {
		return nil, resourceRefusal("resource_state_unavailable")
	}
	for _, v := range d.State.Sources {
		if v.ID == "" || v.Revision < 1 || !callrules.ValidResourceOutputBounds(v.ResourceSpec) {
			return nil, resourceRefusal("resource_state_unavailable")
		}
	}
	for _, l := range d.State.Leases {
		if l.ID == "" || l.RoomID == "" || l.SeatID == "" || l.Identity == "" || l.ResourceID == "" || !l.ExpiresAt.After(l.CreatedAt) || l.ExpiresAt.Sub(l.CreatedAt) > callrules.ResourceLeaseLifetime {
			return nil, resourceRefusal("resource_state_unavailable")
		}
		switch l.Status {
		case "preparing", "awaiting_source", "publishing", "revoking", "stopped":
		default:
			return nil, resourceRefusal("resource_state_unavailable")
		}
	}
	if d.State.Registrations == nil {
		d.State.Registrations = map[string]string{}
	}
	d.ProjectionKnown = r["active"] != nil
	if d.ProjectionKnown && rowBool(r, "active") != resourceStateActive(d) {
		return nil, resourceRefusal("resource_state_unavailable")
	}
	return d, nil
}
func (s *Store) resourceState(ctx context.Context, owner, site, key string) (*resourceRow, error) {
	stateID := resourceStateID(site, key)
	call, err := langparser.RenderCall("callsResourceStateById", map[string]any{"id": stateID})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the account's named resources", call, err)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &resourceRow{ID: stateID, SiteID: site, KeyID: key, State: resourceState{OwnerUserID: owner, Sources: []callrules.ResourceInfo{}, Registrations: map[string]string{}}}, nil
	}
	d, err := resourceFromRow(rows[0])
	if err == nil && (d.SiteID != site || d.KeyID != key || d.State.OwnerUserID != owner) {
		return nil, resourceRefusal("resource_state_unavailable")
	}
	return d, err
}
func (s *Store) resourceStates(ctx context.Context) ([]*resourceRow, error) {
	// Budget decisions require exhaustion, never merely a short page: the
	// engine may clamp authored page sizes below the accounting threshold.
	out := []*resourceRow{}
	for _, inventory := range []struct {
		query string
		limit int
	}{{"callsResourceStates", 1000}, {"callsResourceLegacyStates", 100}} {
		call, err := langparser.RenderCall(inventory.query, nil)
		rows, err := s.readCompleteClusterInventory(ctx, "the resource commitments", call, err, inventory.limit)
		if errors.Is(err, errIncompleteInventory) {
			return nil, resourceRefusal("resource_capacity")
		}
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			d, err := resourceFromRow(row)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	return out, nil
}
func (s *Store) resourceStatePage(ctx context.Context, query string) ([]*resourceRow, error) {
	call, err := langparser.RenderCall(query, nil)
	rows, err := s.readAsCluster(auth.ContextWithInternalOrigin(ctx), "the resource commitments", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]*resourceRow, 0, len(rows))
	for _, r := range rows {
		d, e := resourceFromRow(r)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, nil
}
func (s *Store) saveResourceState(ctx context.Context, d *resourceRow, now time.Time) error {
	data, err := json.Marshal(d.State)
	if err != nil || len(data) > 1<<20 {
		return resourceRefusal("resource_limit")
	}
	version := memql.VersionTimeAfter(d.Version, now)
	call, err := langparser.RenderCall("saveCallsResourceState", map[string]any{"stateId": d.ID, "versionTime": versionStamp(version), "siteId": d.SiteID, "keyId": d.KeyID, "stateData": string(data), "active": resourceStateActive(d)})
	fence := absent(resourceConcept, d.ID)
	if !d.Version.IsZero() {
		fence = at(resourceConcept, d.ID, d.Version)
	}
	err = s.writeAs(auth.ContextWithInternalOrigin(ctx), d.State.OwnerUserID, fence, "the resource commitment", call, err)
	if err == nil {
		d.Version = version
		d.ProjectionKnown = true
	}
	return err
}

func resourceStateActive(d *resourceRow) bool {
	for _, l := range d.State.Leases {
		if l.active() {
			return true
		}
	}
	return false
}
