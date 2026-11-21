package calls

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const portraitConcept = "v1:calls:agentPortrait"

type portraitRow struct {
	callrules.PortraitInfo
	SiteID, KeyID, BlobRef               string
	IssuedAt, Version                    time.Time
	Status, RetirementID, RetirementETag string
	Size                                 int64
	RetirementReady                      bool
	UnusedSince, RetiredAt, PurgeAt      time.Time
}

func portraitFromRow(row map[string]any) (*portraitRow, error) {
	p := &portraitRow{RetirementReady: rowBool(row, "retirementReady"), Status: rowString(row, "status"), RetirementID: rowString(row, "retirementId"), RetirementETag: rowString(row, "retirementETag"), Size: int64(rowInt(row, "byteSize")), UnusedSince: rowTime(row, "unusedSince"), RetiredAt: rowTime(row, "retiredAt"), PurgeAt: rowTime(row, "purgeAt"), PortraitInfo: callrules.PortraitInfo{ID: memql.BareShortId(rowString(row, "id")), Digest: rowString(row, "digest"), MimeType: rowString(row, "mimeType"), Width: int(rowInt(row, "width")), Height: int(rowInt(row, "height"))}, SiteID: memql.BareShortId(rowString(row, "siteId")), KeyID: memql.BareShortId(rowString(row, "keyId")), BlobRef: rowString(row, "blobRef"), IssuedAt: rowTime(row, "issuedAt"), Version: rowTime(row, "createdAt")}
	if (p.Status != "" && p.Status != "active" && p.Status != "retiring" && p.Status != "retired") || p.Size < 0 || p.Size > callrules.PortraitCanonicalBytes || p.SiteID == "" || p.KeyID == "" || p.IssuedAt.IsZero() || !callrules.ValidPortraitReference(p.ID, p.Digest) || p.ID == "" || (p.MimeType != "image/jpeg" && p.MimeType != "image/png") || p.Width < 1 || p.Height < 1 || p.Width > callrules.PortraitMaxSide || p.Height > callrules.PortraitMaxSide {
		return nil, accountRefusal("portrait_unavailable")
	}
	return p, nil
}
func (s *Store) portrait(ctx context.Context, owner, id string) (*portraitRow, error) {
	call, err := langparser.RenderCall("callsPortraitById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "agent portrait", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return portraitFromRow(rows[0])
}
func (s *Store) portraits(ctx context.Context, owner, site, key string) ([]*portraitRow, error) {
	call, err := langparser.RenderCall("callsPortraitsForKey", map[string]any{"siteId": site, "keyId": key})
	rows, err := s.readCompleteOwnerPage(auth.ContextWithInternalOrigin(ctx), owner, "account portraits", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]*portraitRow, 0, len(rows))
	for _, row := range rows {
		p, e := portraitFromRow(row)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
func (s *Store) savePortrait(ctx context.Context, owner string, p *portraitRow, now time.Time) error {
	if p.Status == "" {
		p.Status = "active"
	}
	version := memql.VersionTimeAfter(p.Version, now)
	call, err := langparser.RenderCall("saveCallsPortrait", map[string]any{"portraitId": p.ID, "versionTime": versionStamp(version), "siteId": p.SiteID, "keyId": p.KeyID, "digest": p.Digest, "mimeType": p.MimeType, "width": p.Width, "height": p.Height, "blobRef": p.BlobRef, "issuedAt": versionStamp(p.IssuedAt), "status": p.Status, "byteSize": p.Size, "unusedSince": optionalPortraitTime(p.UnusedSince), "retiredAt": optionalPortraitTime(p.RetiredAt), "purgeAt": optionalPortraitTime(p.PurgeAt), "retirementId": p.RetirementID, "retirementETag": p.RetirementETag, "retirementReady": p.RetirementReady})
	fence := absent(portraitConcept, p.ID)
	if !p.Version.IsZero() {
		fence = at(portraitConcept, p.ID, p.Version)
	}
	err = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "agent portrait", call, err)
	if err == nil {
		p.Version = version
	}
	return err
}

func optionalPortraitTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return versionStamp(t)
}
func (p *portraitRow) available() bool { return p != nil && (p.Status == "" || p.Status == "active") }

// readCompleteOwnerPage refuses truncated bounded inventories. Cleanup and asset
// limits cannot treat a lower operator MaxResults ceiling as proof of absence.
// It clears inherited cursors and preserves the existing private owner authority.
func (s *Store) readCompleteOwnerPage(ctx context.Context, owner, what, call string, rendered error) ([]map[string]any, error) {
	if rendered != nil {
		return nil, quietly("calls: render "+what, rendered)
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	owned, ok := rowsAs(ctx, owner)
	if !ok {
		return nil, errNoOwner
	}
	raw, e := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(owned)), ""), "query "+call)
	if e != nil {
		return nil, quietly("calls: read "+what, e)
	}
	out, rows, e := accountingInventoryPage(raw)
	if e != nil || !uniquePortraitInventoryRows(rows) {
		return nil, fmt.Errorf("calls: %s inventory incomplete", what)
	}
	next, e := memql.ResultPageCursor(out)
	if e != nil || next != "" {
		return nil, fmt.Errorf("calls: %s inventory incomplete", what)
	}
	return rows, nil
}

// Instance IDs are mandatory and unique in a complete shaped inventory. A
// malformed row cannot disappear into an apparently empty reference set.
func uniquePortraitInventoryRows(rows []map[string]any) bool {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		id := memql.BareShortId(rowString(row, "id"))
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
