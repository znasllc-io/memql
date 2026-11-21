package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func visualEvent(b processingBundle, now time.Time) processingEvent {
	for _, track := range b.Tracks {
		if track.SafetyAllowed {
			return processingEvent{Kind: "visual_observation", RoomID: b.RoomID, ParticipantID: track.ParticipantID, MediaIdentity: track.MediaIdentity, TrackID: track.TrackID, Epoch: b.Revision, ConsentRevision: track.ConsentRevision, Source: track.Source, CapturedAt: float64(now.Unix()), Observation: &processingObservation{Score: .1, Model: "benign-fixture", Hash: processingHash{Status: "fixture_only", Provider: "local-fixture"}}}
		}
	}
	return processingEvent{}
}

func processingStoredBytes(t *testing.T, cw *callWorld, concept, room string) (int, int) {
	t.Helper()
	var count, bytes int
	e := cw.db.QueryRowContext(context.Background(), `SELECT count(*), COALESCE(sum(octet_length(payload::text)),0) FROM "MemoryNodes" WHERE concept=? AND payload->>'roomId'=?`, concept, roomConcept+":"+room).Scan(&count, &bytes)
	require.NoError(t, e)
	return count, bytes
}

func TestProcessingPublicationStorageDoesNotScaleWithRoomSize(t *testing.T) {
	cw, _, host, start, media := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	current := consentProcessing(t, cw, host, true, true, true)
	tracks := []MediaTrack{{SID: "TR_microphone", Source: "microphone"}}
	for n := 0; n < 99; n++ {
		tracks = append(tracks, MediaTrack{SID: fmt.Sprintf("TR_camera_%03d", n), Source: "camera"})
	}
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: current.Identity, Tracks: tracks})
	b, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, e)
	require.Len(t, b.Tracks, 100)
	before, e := cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	beforeJSON, _ := json.Marshal(before)
	beforeCount, beforeBytes := processingStoredBytes(t, cw, processingConcept, start.Call.ID)
	v := visualEvent(b, cw.clock.Now())
	for n := 0; n < 8; n++ {
		v.Sequence = int64(n)
		r, e := cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, v)
		require.NoError(t, e)
		require.Equal(t, "published", r["status"])
	}
	receiptCount, _ := processingStoredBytes(t, cw, processingReceiptConcept, start.Call.ID)
	require.Zero(t, receiptCount, "visual observations do not create persistent findings or replay metadata")
	require.Empty(t, media.packets, "visual observations are not participant accusations or enforcement")
	ev := speechEvent(b)
	r, e := cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "published", r["status"])
	tone := ev
	tone.Kind = "text_tone"
	tone.Text = ""
	tone.StartSample = 0
	tone.EndSample = 0
	tone.Language = ""
	tone.Label = "mixed or neutral wording"
	tone.Uncertainty = "uncalibrated"
	tone.Provenance = "fixture"
	tone.Scope = "text wording only; not a participant assessment"
	_, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, tone)
	require.NoError(t, e)
	duplicate, e := cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "duplicate", duplicate["status"])
	require.Len(t, media.packets, 2)
	after, e := cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	afterJSON, _ := json.Marshal(after)
	require.Equal(t, before.Version, after.Version)
	require.JSONEq(t, string(beforeJSON), string(afterJSON))
	afterCount, afterBytes := processingStoredBytes(t, cw, processingConcept, start.Call.ID)
	require.Equal(t, beforeCount, afterCount)
	require.Equal(t, beforeBytes, afterBytes)
	receiptCount, receiptBytes := processingStoredBytes(t, cw, processingReceiptConcept, start.Call.ID)
	rateCount, rateBytes := processingStoredBytes(t, cw, processingRateConcept, start.Call.ID)
	require.Equal(t, 2, receiptCount)
	require.Equal(t, 10, rateCount)
	require.Less(t, receiptBytes, 4096)
	require.Less(t, rateBytes, 4096)
	require.Less(t, receiptBytes+rateBytes, len(beforeJSON), "ten events cost less than one unchanged 100-track lease snapshot")
	t.Logf("100-track lease=%d canonical JSON bytes; 8 visual+1 transcript+1 tone persisted %d receipt bytes + %d rate bytes, zero lease bytes", len(beforeJSON), receiptBytes, rateBytes)
	// Unrelated camera disappearance does not invalidate an unchanged microphone;
	// renewal still recomputes the whole allowlist and advances its generation.
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: current.Identity, Tracks: []MediaTrack{{SID: "TR_microphone", Source: "microphone"}}})
	ev.StartSample = 16000
	ev.EndSample = 32000
	ev.SegmentID = fmtSegment(b.Revision, 16000, 32000)
	_, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	renewed, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, false, 100)
	require.NoError(t, e)
	require.Greater(t, renewed.Revision, b.Revision)
	_, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.ErrorContains(t, e, "processing_lease_expired")
}

func TestProcessingCompactRateAndReplayRemainSharedAcrossReplicas(t *testing.T) {
	cw, _, host, start, media := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	consentProcessing(t, cw, host, true, true, false)
	b, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, e)
	second := *cw.svc
	// Both service instances share only native storage/gates, no in-memory budget.
	var wg sync.WaitGroup
	results := make(chan string, 20)
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			svc := cw.svc
			if n%2 == 0 {
				svc = &second
			}
			ev := visualEvent(b, cw.clock.Now())
			ev.Sequence = int64(n)
			r, e := svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
			if e != nil {
				results <- "error:" + e.Error()
				return
			}
			results <- r["status"].(string)
		}(n)
	}
	wg.Wait()
	close(results)
	counts := map[string]int{}
	for r := range results {
		counts[r]++
	}
	require.Equal(t, map[string]int{"published": 16, "rate_limited": 4}, counts)
	cw.clock.Advance(time.Second)
	ev := speechEvent(b)
	var receipts [2]map[string]any
	var errs [2]error
	for n, svc := range []*Service{cw.svc, &second} {
		wg.Add(1)
		go func(n int, svc *Service) {
			defer wg.Done()
			receipts[n], errs[n] = svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
		}(n, svc)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, receipts[0]["event_id"], receipts[1]["event_id"])
	require.Len(t, media.packets, 1)
	ev.Text = "Different result for an already-published interval"
	stale, e := second.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e, "a dropped old result must not reset a healthy worker lease")
	require.Equal(t, "stale", stale["status"])
	require.Len(t, media.packets, 1)
}

func TestProcessingReplayProtectionSurvivesLegacyUpgradeAndMaximumCall(t *testing.T) {
	cw, _, host, start, media := processingDurationFixture(t, true, 1440)
	if cw == nil {
		return
	}
	ctx := context.Background()
	consentProcessing(t, cw, host, false, true, false)
	b, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, e)
	ev := speechEvent(b)
	first, e := cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	j, e := cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	receipts, e := cw.svc.store.processingReceipts(ctx, cw.owner, j, ev.TrackID)
	require.NoError(t, e)
	// Emulate an already-running old server's receipt; the upgrade must not
	// treat a missing new-format row as permission to publish the same event.
	j.LegacyReceipts = map[string]processingReceipt{ev.TrackID + ":transcript": receipts.Receipts["transcript"]}
	require.NoError(t, cw.svc.store.saveProcessing(ctx, cw.owner, j, cw.clock.Now()))
	_, e = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept=? AND id=?", processingReceiptConcept, processingReceiptConcept+":"+receipts.ID).Exec(ctx)
	require.NoError(t, e)
	replayed, e := cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "duplicate", replayed["status"])
	require.Equal(t, first["event_id"], replayed["event_id"])
	upgraded, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", b.LeaseID, false, 100)
	require.NoError(t, e)
	require.Greater(t, upgraded.Revision, b.Revision)
	ev = speechEvent(upgraded)
	_, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", upgraded.LeaseID, upgraded.Revision, ev)
	require.NoError(t, e)
	// Simulate uninterrupted expiry-only renewals without thousands of fixture
	// writes. The generation and initial receipt remain exactly as originally
	// published, while the room approaches its configured hard deadline.
	cw.clock.Advance(24*time.Hour - time.Minute)
	j, e = cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	j.Lease.ExpiresAt = cw.clock.Now().Add(2 * time.Minute)
	require.NoError(t, cw.svc.store.saveProcessing(ctx, cw.owner, j, cw.clock.Now()))
	replayed, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", upgraded.LeaseID, upgraded.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "duplicate", replayed["status"])
	cw.clock.Advance(time.Minute)
	// Even if retention now removes all old receipts, the hard room deadline
	// rejects before replay lookup. There is no 24-hour resurrection window.
	_, e = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept=? AND payload->>'roomId'=?", processingReceiptConcept, roomConcept+":"+start.Call.ID).Exec(ctx)
	require.NoError(t, e)
	_, e = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", upgraded.LeaseID, upgraded.Revision, ev)
	require.ErrorContains(t, e, "processing_not_available")
	require.Len(t, media.packets, 2)
}
