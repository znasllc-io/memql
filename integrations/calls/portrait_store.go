package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const portraitConcept = "v1:calls:agentPortrait"

type portraitRow struct {
	callrules.PortraitInfo
	SiteID, KeyID, BlobRef string
	IssuedAt, Version      time.Time
}

func portraitFromRow(row map[string]any) (*portraitRow, error) {
	p := &portraitRow{PortraitInfo: callrules.PortraitInfo{ID: memql.BareShortId(rowString(row, "id")), Digest: rowString(row, "digest"), MimeType: rowString(row, "mimeType"), Width: int(rowInt(row, "width")), Height: int(rowInt(row, "height"))}, SiteID: memql.BareShortId(rowString(row, "siteId")), KeyID: memql.BareShortId(rowString(row, "keyId")), BlobRef: rowString(row, "blobRef"), IssuedAt: rowTime(row, "issuedAt"), Version: rowTime(row, "createdAt")}
	if p.SiteID == "" || p.KeyID == "" || p.IssuedAt.IsZero() || !callrules.ValidPortraitReference(p.ID, p.Digest) || p.ID == "" || (p.MimeType != "image/jpeg" && p.MimeType != "image/png") || p.Width < 1 || p.Height < 1 || p.Width > callrules.PortraitMaxSide || p.Height > callrules.PortraitMaxSide {
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
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "account portraits", call, err)
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
	version := memql.VersionTimeAfter(p.Version, now)
	call, err := langparser.RenderCall("saveCallsPortrait", map[string]any{"portraitId": p.ID, "versionTime": versionStamp(version), "siteId": p.SiteID, "keyId": p.KeyID, "digest": p.Digest, "mimeType": p.MimeType, "width": p.Width, "height": p.Height, "blobRef": p.BlobRef, "issuedAt": versionStamp(p.IssuedAt)})
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
