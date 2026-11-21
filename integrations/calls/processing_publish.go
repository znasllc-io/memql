package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const processingTopic = "callkey.processing.v1"

type processingPublisher interface {
	PublishProcessing(context.Context, string, []byte, []string) error
}
type processingReceipt struct {
	Digest, EventID, Segment string
	Position                 int64
	At                       time.Time
}
type processingEvent struct {
	Kind            string                 `json:"kind"`
	RoomID          string                 `json:"room_id"`
	ParticipantID   string                 `json:"participant_id"`
	MediaIdentity   string                 `json:"media_identity"`
	TrackID         string                 `json:"track_id"`
	DisplayName     string                 `json:"display_name"`
	Epoch           int64                  `json:"epoch"`
	ConsentRevision string                 `json:"consent_revision"`
	Source          string                 `json:"source"`
	StartSample     int64                  `json:"start_sample,omitempty"`
	EndSample       int64                  `json:"end_sample,omitempty"`
	Text            string                 `json:"text,omitempty"`
	Language        string                 `json:"language,omitempty"`
	SegmentID       string                 `json:"transcript_segment_id,omitempty"`
	CapturedAt      float64                `json:"captured_at,omitempty"`
	Sequence        int64                  `json:"sequence,omitempty"`
	Observation     *processingObservation `json:"observation,omitempty"`
	Label           string                 `json:"label,omitempty"`
	Score           float64                `json:"score,omitempty"`
	Uncertainty     string                 `json:"uncertainty,omitempty"`
	Provenance      string                 `json:"provenance,omitempty"`
	Scope           string                 `json:"scope,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
}
type processingObservation struct {
	Score float64        `json:"nsfw_score"`
	Model string         `json:"model_revision"`
	Hash  processingHash `json:"hash_check"`
}
type processingHash struct {
	Status     string   `json:"status"`
	Provider   string   `json:"provider"`
	Candidates []string `json:"candidate_ids"`
}

func plainProcessingText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
func decodeProcessingEvent(raw []byte) (processingEvent, error) {
	var ev processingEvent
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&ev) != nil {
		return ev, callrules.ErrInvalid
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	allowed := []string{"kind", "room_id", "participant_id", "media_identity", "track_id", "display_name", "epoch", "consent_revision", "source"}
	switch ev.Kind {
	case "transcript":
		allowed = append(allowed, "start_sample", "end_sample", "text", "language", "transcript_segment_id")
	case "visual_observation":
		allowed = append(allowed, "captured_at", "sequence", "observation")
	case "text_tone":
		allowed = append(allowed, "transcript_segment_id", "label", "score", "uncertainty", "provenance", "scope")
	case "incomplete":
		allowed = append(allowed, "reason", "transcript_segment_id")
	default:
		return ev, callrules.ErrInvalid
	}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return ev, callrules.ErrInvalid
		}
	}
	for _, s := range []string{ev.RoomID, ev.ParticipantID, ev.MediaIdentity, ev.TrackID, ev.ConsentRevision} {
		if s == "" || len(s) > 128 || !plainProcessingText(s, 128) {
			return ev, callrules.ErrInvalid
		}
	}
	if len(ev.SegmentID) > 128 || !plainProcessingText(ev.SegmentID, 128) {
		return ev, callrules.ErrInvalid
	}
	if ev.Epoch < 1 || ev.Epoch > 1e12 || !plainProcessingText(ev.DisplayName, 80) {
		return ev, callrules.ErrInvalid
	}
	switch ev.Kind {
	case "transcript":
		if ev.StartSample < 0 || ev.EndSample <= ev.StartSample || ev.EndSample > 16000*86400 || ev.EndSample-ev.StartSample > 16000*90 || strings.TrimSpace(ev.Text) == "" || !plainProcessingText(ev.Text, 4096) || ev.SegmentID != fmt.Sprintf("%d:%d:%d", ev.Epoch, ev.StartSample, ev.EndSample) {
			return ev, callrules.ErrInvalid
		}
		if ev.Language != "" {
			if len(ev.Language) < 2 || len(ev.Language) > 3 {
				return ev, callrules.ErrInvalid
			}
			for _, c := range ev.Language {
				if c < 'a' || c > 'z' {
					return ev, callrules.ErrInvalid
				}
			}
		}
	case "visual_observation":
		o := ev.Observation
		if o == nil || math.IsNaN(o.Score) || o.Score < 0 || o.Score > 1 || ev.Sequence < 0 || ev.Sequence > 1<<53 || o.Model == "" || !plainProcessingText(o.Model, 128) || !plainProcessingText(o.Hash.Provider, 128) || len(o.Hash.Candidates) > 16 || !slices.Contains([]string{"unavailable", "low_quality", "no_candidate", "candidate", "fixture_only"}, o.Hash.Status) {
			return ev, callrules.ErrInvalid
		}
		for _, c := range o.Hash.Candidates {
			if !plainProcessingText(c, 128) {
				return ev, callrules.ErrInvalid
			}
		}
	case "text_tone":
		if !slices.Contains([]string{"positive wording", "negative wording", "mixed or neutral wording"}, ev.Label) || math.IsNaN(ev.Score) || ev.Score < -1 || ev.Score > 1 || ev.Uncertainty != "uncalibrated" || ev.Scope != "text wording only; not a participant assessment" || !plainProcessingText(ev.Provenance, 128) || ev.Provenance == "" || len(ev.SegmentID) > 128 {
			return ev, callrules.ErrInvalid
		}
	case "incomplete":
		if ev.Reason == "" || len(ev.Reason) > 128 {
			return ev, callrules.ErrInvalid
		}
		for _, r := range ev.Reason {
			if r != '_' && (r < 'a' || r > 'z') {
				return ev, callrules.ErrInvalid
			}
		}
	}
	return ev, nil
}
func (s *Service) processingPublish(ctx context.Context, site callrules.SiteRef, roomID, actor, leaseID string, revision int64, ev processingEvent) (map[string]any, error) {
	fresh := memql.ContextWithFreshRead(ctx)
	hint, e := s.store.roomByID(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return nil, e
	}
	if hint == nil || hint.SiteID != site.SiteID {
		return nil, callrules.ErrNotFound
	}
	backend, e := s.backendFor(fresh, *hint)
	if e != nil || backend == nil {
		return nil, accountRefusal("processing_unavailable")
	}
	sender, ok := backend.(processingPublisher)
	if !ok {
		return nil, accountRefusal("processing_unavailable")
	}
	observed, e := backend.Participants(ctx, hint.MediaRoom)
	if e != nil {
		return nil, accountRefusal("processing_unavailable")
	}
	release, e := s.gates(ctx, roomGate(roomID))
	if e != nil {
		return nil, e
	}
	defer release()
	room, e := s.store.roomByID(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return nil, e
	}
	if room == nil || room.SiteID != site.SiteID || room.MediaServerID != hint.MediaServerID {
		return nil, callrules.ErrNotFound
	}
	p, e := s.store.PolicyFor(fresh, site.OwnerUserID, site.SiteID)
	if e != nil {
		return nil, e
	}
	if !processingRoomEligible(*room, p, s.now()) {
		return nil, accountRefusal("processing_not_available")
	}
	j, e := s.store.processing(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return nil, e
	}
	if j == nil || j.Lease.ID != leaseID || j.Lease.Owner != actor || j.Epoch != revision || !s.now().Before(j.Lease.ExpiresAt) {
		return nil, accountRefusal("processing_lease_expired")
	}
	tracks, e := s.processingTracks(fresh, *room, j, observed)
	if e != nil {
		return nil, e
	}
	if !sameProcessingTracks(tracks, j.Lease.Tracks) {
		return nil, accountRefusal("processing_tracks_changed")
	}
	var track *processingTrack
	for n := range j.Lease.Tracks {
		t := &j.Lease.Tracks[n]
		if t.TrackID == ev.TrackID {
			track = t
			break
		}
	}
	if track == nil || ev.RoomID != room.ID || ev.ParticipantID != track.ParticipantID || ev.MediaIdentity != track.MediaIdentity || ev.Source != track.Source || ev.ConsentRevision != track.ConsentRevision || ev.Epoch != j.Epoch {
		return nil, accountRefusal("processing_authority_changed")
	}
	if ev.Kind == "transcript" && !track.TranscriptionAllowed || ev.Kind == "text_tone" && !track.InsightsAllowed || ev.Kind == "visual_observation" && !track.SafetyAllowed {
		return nil, accountRefusal("processing_not_consented")
	}
	if ev.Kind == "visual_observation" && (ev.CapturedAt < float64(s.now().Unix())-30 || ev.CapturedAt > float64(s.now().Unix())+1) {
		return nil, callrules.ErrInvalid
	}
	ev.DisplayName = track.DisplayName // never trust worker-supplied attribution
	raw, e := json.Marshal(ev)
	if e != nil {
		return nil, callrules.ErrInvalid
	}
	digest := string(id.NewUntracked().FromBytes(raw))
	key := ev.TrackID + ":" + ev.Kind
	if j.Receipts == nil {
		j.Receipts = map[string]processingReceipt{}
	}
	prior, had := j.Receipts[key]
	if had && prior.Digest == digest {
		return map[string]any{"status": "duplicate", "event_id": prior.EventID}, nil
	}
	if ev.Kind == "transcript" && had && ev.StartSample < prior.Position || ev.Kind == "visual_observation" && had && ev.Sequence <= prior.Position {
		return nil, accountRefusal("processing_event_old")
	}
	if ev.Kind == "text_tone" {
		spoken, ok := j.Receipts[ev.TrackID+":transcript"]
		if !ok || spoken.Segment != ev.SegmentID || had && prior.Segment == ev.SegmentID {
			return nil, accountRefusal("processing_event_old")
		}
	}
	if ev.Kind == "incomplete" && had && s.now().Sub(prior.At) < time.Second {
		return nil, accountRefusal("processing_rate_limited")
	}
	if j.RateSecond != s.now().Unix() {
		j.RateSecond = s.now().Unix()
		j.RateCount = 0
	}
	if j.RateCount >= 16 {
		return nil, accountRefusal("processing_rate_limited")
	}
	j.RateCount++
	receipt := processingReceipt{Digest: digest, EventID: id.NewShortId(), Segment: ev.SegmentID, At: s.now()}
	if ev.Kind == "transcript" {
		receipt.Position = ev.EndSample
	}
	if ev.Kind == "visual_observation" {
		receipt.Position = ev.Sequence
	}
	j.Receipts[key] = receipt
	// Persist only the digest/sequence before sending. An uncertain send may lose one
	// ephemeral caption, but retry cannot duplicate it or persist its text in a run.
	if e = s.store.saveProcessing(fresh, site.OwnerUserID, j, s.now()); e != nil {
		return nil, e
	}
	if ev.Kind != "visual_observation" {
		var packet map[string]any
		_ = json.Unmarshal(raw, &packet)
		packet["event_id"] = receipt.EventID
		packet["published_at"] = s.now().Unix()
		packet["expires_at"] = j.Lease.ExpiresAt.Unix()
		if ev.Kind == "transcript" {
			packet["start_sample"] = ev.StartSample
			packet["end_sample"] = ev.EndSample
		}
		if ev.Kind == "text_tone" {
			packet["score"] = ev.Score
		}
		seats, e := s.store.seatsForRoom(fresh, site.OwnerUserID, room.ID)
		if e != nil {
			return nil, e
		}
		recipients := []string{}
		for _, seat := range seats {
			if seat.occupying() {
				recipients = append(recipients, seat.Identity)
			}
		}
		payload, _ := json.Marshal(packet)
		// A short server data send shares the admission/consent gate, so consent cannot
		// commit withdrawn and then have an earlier result published afterwards.
		sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if len(recipients) > 0 {
			if e = sender.PublishProcessing(sendCtx, room.MediaRoom, payload, recipients); e != nil {
				return nil, accountRefusal("processing_delivery_uncertain")
			}
		}
	}
	return map[string]any{"status": "published", "event_id": receipt.EventID}, nil
}
