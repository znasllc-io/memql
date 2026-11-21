package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const processingReceiptConcept = "v1:calls:processingReceipt"
const processingRateConcept = "v1:calls:processingRate"

// Each track keeps at most three tiny output receipts, independently of room
// size. Visual observations have no downstream effect and need no replay row.
// One-day retention covers the complete maximum 24-hour call: a receipt cannot
// be older than its call, and expired calls reject before consulting receipts.
type processingTrackReceipts struct {
	ID, SiteID, RoomID, TrackID string
	Epoch                       int64
	Version                     time.Time
	Receipts                    map[string]processingReceipt
}

func processingReceiptID(room, track string, epoch int64) string {
	return string(id.NewUntracked().FromBytes([]byte(fmt.Sprintf("%s\x00%s\x00%d", room, track, epoch))))
}

func (s *Store) processingReceipts(ctx context.Context, owner string, j *processingJournal, track string) (*processingTrackReceipts, error) {
	out := &processingTrackReceipts{ID: processingReceiptID(j.RoomID, track, j.Epoch), SiteID: j.SiteID, RoomID: j.RoomID, TrackID: track, Epoch: j.Epoch, Receipts: map[string]processingReceipt{}}
	call, e := langparser.RenderCall("callsProcessingReceiptById", map[string]any{"id": out.ID})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "processing replay metadata", call, e)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		for _, kind := range []string{"transcript", "text_tone", "incomplete"} {
			if old, ok := j.LegacyReceipts[track+":"+kind]; ok {
				out.Receipts[kind] = old
			}
		}
		return out, nil
	}
	r := rows[0]
	raw := rowString(r, "receiptData")
	if memql.BareShortId(rowString(r, "siteId")) != j.SiteID || memql.BareShortId(rowString(r, "roomId")) != j.RoomID || rowString(r, "trackId") != track || int64(rowInt(r, "epoch")) != j.Epoch || len(raw) > 4096 || json.Unmarshal([]byte(raw), &out.Receipts) != nil || len(out.Receipts) > 3 {
		return nil, accountRefusal("processing_unavailable")
	}
	if out.Receipts == nil {
		out.Receipts = map[string]processingReceipt{}
	}
	out.Version = rowTime(r, "createdAt")
	return out, nil
}

func (s *Store) saveProcessingReceipts(ctx context.Context, owner string, r *processingTrackReceipts, now time.Time) error {
	raw, e := json.Marshal(r.Receipts)
	if e != nil || len(raw) > 4096 || len(r.Receipts) > 3 {
		return accountRefusal("processing_limit")
	}
	v := memql.VersionTimeAfter(r.Version, now)
	call, e := langparser.RenderCall("saveCallsProcessingReceipt", map[string]any{"stateId": r.ID, "versionTime": versionStamp(v), "siteId": r.SiteID, "roomId": r.RoomID, "trackId": r.TrackID, "epoch": r.Epoch, "receiptData": string(raw)})
	fence := absent(processingReceiptConcept, r.ID)
	if !r.Version.IsZero() {
		fence = at(processingReceiptConcept, r.ID, r.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "processing replay metadata", call, e)
	if e == nil {
		r.Version = v
	}
	return e
}

// Called under the cross-replica room gate, with a version fence as a second
// guard. Failed sends and partial writes can spend quota but never expand it.
func (s *Store) takeProcessingPublication(ctx context.Context, owner string, j *processingJournal, now time.Time) error {
	call, e := langparser.RenderCall("callsProcessingRateById", map[string]any{"id": j.RoomID})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "processing rate metadata", call, e)
	if e != nil {
		return e
	}
	count := 0
	var version time.Time
	if len(rows) > 0 {
		r := rows[0]
		if memql.BareShortId(rowString(r, "siteId")) != j.SiteID || memql.BareShortId(rowString(r, "roomId")) != j.RoomID {
			return accountRefusal("processing_unavailable")
		}
		version = rowTime(r, "createdAt")
		if int64(rowInt(r, "rateSecond")) == now.Unix() {
			count = rowInt(r, "rateCount")
		}
	} else if j.LegacySecond == now.Unix() {
		count = j.LegacyCount
	}
	if count < 0 || count >= 16 {
		return accountRefusal("processing_rate_limited")
	}
	v := memql.VersionTimeAfter(version, now)
	call, e = langparser.RenderCall("saveCallsProcessingRate", map[string]any{"stateId": j.RoomID, "versionTime": versionStamp(v), "siteId": j.SiteID, "roomId": j.RoomID, "rateSecond": now.Unix(), "rateCount": count + 1})
	fence := absent(processingRateConcept, j.RoomID)
	if !version.IsZero() {
		fence = at(processingRateConcept, j.RoomID, version)
	}
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "processing rate metadata", call, e)
}
