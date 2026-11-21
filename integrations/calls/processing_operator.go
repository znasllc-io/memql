package calls

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type processingBundle struct {
	LeaseID       string            `json:"lease_id"`
	Revision      int64             `json:"revision"`
	ExpiresAt     int64             `json:"expires_at"`
	RoomID        string            `json:"room_id"`
	MediaRoom     string            `json:"media_room"`
	MediaURL      string            `json:"media_url"`
	MediaToken    string            `json:"media_token"`
	MediaIdentity string            `json:"media_identity"`
	FamilyCall    bool              `json:"family_call"`
	Tracks        []processingTrack `json:"tracks"`
}

// The pinned self-hosted media configuration permits ten simultaneous video
// subscriptions per participant. Refuse the whole lease before minting its
// sampler token: a larger track budget must never imply silent visual coverage.
// Keep this conservative limit across providers until their receive path is
// independently qualified.
const processingVisualPublicationLimit = 10

// Results are ephemeral. Refuse run contexts before parsing anything because a
// run journal persists its statements and results, including tokens or captions.
func (i *Integration) processingOperator(ctx context.Context, args map[string]any, allowed ...string) (*siteView, string, error) {
	if e := refuseInARun(ctx, "Processing is available only to the trusted worker outside runs."); e != nil {
		return nil, "", e
	}
	a, ok := auth.AccessFromContext(ctx)
	if !ok || a == nil || !a.IsClusterOwner() {
		return nil, "", accountRefusal("processing_operator_required")
	}
	for k := range args {
		found := false
		for _, v := range allowed {
			if k == v {
				found = true
			}
		}
		if !found {
			return nil, "", callrules.ErrInvalid
		}
	}
	siteID, ok := args["siteId"].(string)
	if !ok || siteID == "" || len(siteID) > 128 {
		return nil, "", callrules.ErrInvalid
	}
	roomID, ok := args["roomId"].(string)
	if !ok || roomID == "" || len(roomID) > 128 {
		return nil, "", callrules.ErrInvalid
	}
	site, e := i.siteFor(memql.ContextWithFreshRead(ctx), siteID)
	if e != nil {
		return nil, "", accountRefusal("processing_not_available")
	}
	return site, a.UserId + ":" + a.IdentityId, nil
}
func (i *Integration) handleProcessingLease(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	site, actor, e := i.processingOperator(ctx, args, "siteId", "roomId", "leaseId", "reset", "maxTracks")
	if e != nil {
		return nil, e
	}
	leaseID := ""
	if v, exists := args["leaseId"]; exists && v != nil {
		var ok bool
		leaseID, ok = v.(string)
		if !ok || len(leaseID) > 128 {
			return nil, callrules.ErrInvalid
		}
	}
	reset := false
	if v, exists := args["reset"]; exists && v != nil {
		var ok bool
		reset, ok = v.(bool)
		if !ok {
			return nil, callrules.ErrInvalid
		}
	}
	if reset && leaseID == "" {
		return nil, callrules.ErrInvalid
	}
	maxTracks, ok := wholeNumber(args["maxTracks"])
	if !ok || maxTracks < 0 || maxTracks > 300 {
		return nil, callrules.ErrInvalid
	}
	out, e := i.service.processingLease(ctx, callrules.SiteRef{SiteID: site.ID, OwnerUserID: site.OwnerUserID}, args["roomId"].(string), actor, leaseID, reset, maxTracks)
	if e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(out)
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, accountRefusal("processing_unavailable")
	}
	return i.node("processingLease", payload)
}
func (i *Integration) handleProcessingPublish(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	site, actor, e := i.processingOperator(ctx, args, "siteId", "roomId", "leaseId", "revision", "event")
	if e != nil {
		return nil, e
	}
	leaseID, ok := args["leaseId"].(string)
	if !ok || leaseID == "" || len(leaseID) > 128 {
		return nil, callrules.ErrInvalid
	}
	// Use the same numeric normalizer as bounded policy arguments; fractional values reject.
	revision, ok := processingInteger(args["revision"])
	if !ok || revision < 1 {
		return nil, callrules.ErrInvalid
	}
	raw, e := json.Marshal(args["event"])
	if e != nil || len(raw) > 16384 {
		return nil, callrules.ErrInvalid
	}
	ev, e := decodeProcessingEvent(raw)
	if e != nil {
		return nil, e
	}
	receipt, e := i.service.processingPublish(ctx, callrules.SiteRef{SiteID: site.ID, OwnerUserID: site.OwnerUserID}, args["roomId"].(string), actor, leaseID, revision, ev)
	if e != nil {
		return nil, e
	}
	return i.node("processingPublish", receipt)
}
func processingInteger(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x >= 0 && x <= 1e12 && float64(int64(x)) == x {
			return int64(x), true
		}
	case json.Number:
		n, e := x.Int64()
		return n, e == nil
	}
	return 0, false
}
func (s *Service) processingLease(ctx context.Context, site callrules.SiteRef, roomID, actor, leaseID string, reset bool, maxTracks int) (processingBundle, error) {
	none := processingBundle{}
	fresh := memql.ContextWithFreshRead(ctx)
	hint, e := s.store.roomByID(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return none, e
	}
	if hint == nil || hint.SiteID != site.SiteID {
		return none, callrules.ErrNotFound
	}
	backend, e := s.backendFor(fresh, *hint)
	if e != nil || backend == nil {
		return none, accountRefusal("processing_unavailable")
	}
	if _, ok := backend.(processingPublisher); !ok {
		return none, accountRefusal("processing_unavailable")
	}
	observed, e := backend.Participants(ctx, hint.MediaRoom)
	if e != nil {
		return none, accountRefusal("processing_unavailable")
	}
	server, e := s.store.MediaServerByID(fresh, hint.MediaServerID)
	if e != nil || server == nil {
		return none, accountRefusal("processing_unavailable")
	}
	credential, e := s.credential(fresh, *server)
	if e != nil {
		return none, e
	}
	release, e := s.gates(ctx, roomGate(roomID))
	if e != nil {
		return none, e
	}
	oldIdentity := ""
	defer func() {
		release()
		if oldIdentity != "" {
			s.disconnectProcessing(ctx, *hint, oldIdentity)
		}
	}()
	room, e := s.store.roomByID(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return none, e
	}
	if room == nil || room.SiteID != site.SiteID || room.MediaServerID != hint.MediaServerID {
		return none, callrules.ErrNotFound
	}
	policy, e := s.store.PolicyFor(fresh, site.OwnerUserID, site.SiteID)
	if e != nil {
		return none, e
	}
	if !processingRoomEligible(*room, policy, s.now()) {
		return none, accountRefusal("processing_not_available")
	}
	j, e := s.store.processing(fresh, site.OwnerUserID, roomID)
	if e != nil {
		return none, e
	}
	if j == nil {
		j = newProcessing(*room)
	}
	alive := j.Lease.ID != "" && s.now().Before(j.Lease.ExpiresAt)
	if alive && (leaseID != j.Lease.ID || actor != j.Lease.Owner) {
		return none, accountRefusal("processing_lease_busy")
	}
	if reset && (!alive || leaseID != j.Lease.ID || actor != j.Lease.Owner) {
		return none, accountRefusal("processing_lease_expired")
	}
	tracks, e := s.processingTracks(fresh, *room, j, observed)
	if e != nil {
		return none, e
	}
	visualPublications := 0
	for _, track := range tracks {
		if track.SafetyAllowed {
			visualPublications++
		}
	}
	if maxTracks == 0 || len(tracks) > maxTracks || visualPublications > processingVisualPublicationLimit {
		oldIdentity = j.Lease.Identity
		j.revoke()
		j.Reason = "processing_capacity"
		if e = s.store.saveProcessing(fresh, site.OwnerUserID, j, s.now()); e != nil {
			return none, e
		}
		return none, accountRefusal("processing_capacity")
	}
	if len(tracks) == 0 {
		if j.Lease.ID != "" {
			oldIdentity = j.Lease.Identity
			j.revoke()
			if e = s.store.saveProcessing(fresh, site.OwnerUserID, j, s.now()); e != nil {
				return none, e
			}
		}
		return none, accountRefusal("processing_no_consent")
	}
	changed := !alive || reset || len(j.LegacyReceipts) > 0 || !sameProcessingTracks(tracks, j.Lease.Tracks)
	if changed {
		oldIdentity = j.Lease.Identity
		j.revoke()
		j.Lease.ID, e = callrules.NewToken(s.random, tokenBytes)
		if e != nil {
			return none, e
		}
		j.Lease.Identity = "sampler-" + id.NewShortId()
		j.Lease.Owner = actor
	}
	j.Reason = ""
	j.Lease.ExpiresAt = s.now().Add(processingTTL)
	for n := range tracks {
		tracks[n].Epoch = j.Epoch
		tracks[n].ExpiresAt = j.Lease.ExpiresAt.Unix()
	}
	j.Lease.Tracks = tracks
	token, e := MintPass(credential.Key, credential.Secret, callrules.KindSampler, room.MediaRoom, j.Lease.Identity, "Callkey processing", nil, processingTTL)
	if e != nil {
		return none, e
	}
	if e = s.store.saveProcessing(fresh, site.OwnerUserID, j, s.now()); e != nil {
		return none, e
	}
	return processingBundle{LeaseID: j.Lease.ID, Revision: j.Epoch, ExpiresAt: j.Lease.ExpiresAt.Unix(), RoomID: room.ID, MediaRoom: room.MediaRoom, MediaURL: server.PublicURL, MediaToken: token, MediaIdentity: j.Lease.Identity, Tracks: tracks}, nil
}
func sameProcessingTracks(a, b []processingTrack) bool {
	a = append([]processingTrack(nil), a...)
	b = append([]processingTrack(nil), b...)
	for i := range a {
		a[i].Epoch = 0
		a[i].ExpiresAt = 0
	}
	for i := range b {
		b[i].Epoch = 0
		b[i].ExpiresAt = 0
	}
	return reflect.DeepEqual(a, b)
}
func (s *Service) processingTracks(ctx context.Context, r roomRow, j *processingJournal, present []MediaParticipant) ([]processingTrack, error) {
	seats, e := s.store.seatsForRoom(ctx, r.OwnerUserID, r.ID)
	if e != nil {
		return nil, e
	}
	return s.processingTracksForSeats(ctx, r, j, present, seats)
}

func (s *Service) processingTracksForSeats(ctx context.Context, r roomRow, j *processingJournal, present []MediaParticipant, seats []seatRow) ([]processingTrack, error) {
	config, e := decodeUsageConfig(r.UsageConfig)
	if e != nil {
		return nil, e
	}
	tracks := []processingTrack{}
	seen := map[string]bool{}
	for _, seat := range seats {
		c := j.consent(seat)
		if !c.Safety && !c.Transcription {
			continue
		}
		allowed, e := s.processingSeatAuthorized(ctx, r, seat, c)
		if e != nil {
			return nil, e
		}
		if !allowed {
			continue
		}
		for _, p := range present {
			if p.Identity != seat.Identity {
				continue
			}
			for _, t := range p.Tracks {
				if t.Muted || t.SID == "" || len(t.SID) > 128 || seen[t.SID] {
					continue
				}
				source := t.Source
				speech := source == "microphone" && c.Transcription && config.Insights
				visual := (source == "camera" || source == "screen_share") && c.Safety
				if !speech && !visual {
					continue
				}
				if source == "camera" && seat.Kind == string(callrules.KindAgent) {
					source = "agent_video"
				}
				seen[t.SID] = true
				tracks = append(tracks, processingTrack{RoomID: r.ID, ParticipantID: seat.ID, MediaIdentity: seat.Identity, TrackID: t.SID, DisplayName: seat.DisplayName, ConsentRevision: c.Revision, Source: source, SafetyAllowed: visual, TranscriptionAllowed: speech, InsightsAllowed: speech && c.Insights})
			}
		}
	}
	if len(tracks) > 300 {
		return nil, accountRefusal("processing_limit")
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].TrackID < tracks[j].TrackID })
	return tracks, nil
}

func (i *Integration) handleProcessingRooms(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if e := refuseInARun(ctx, "Processing is available only to the trusted worker outside runs."); e != nil {
		return nil, e
	}
	a, ok := auth.AccessFromContext(ctx)
	if !ok || a == nil || !a.IsClusterOwner() {
		return nil, accountRefusal("processing_operator_required")
	}
	if len(args) != 1 {
		return nil, callrules.ErrInvalid
	}
	siteID, ok := args["siteId"].(string)
	if !ok || siteID == "" || len(siteID) > 128 {
		return nil, callrules.ErrInvalid
	}
	site, e := i.siteFor(memql.ContextWithFreshRead(ctx), siteID)
	if e != nil {
		return nil, accountRefusal("processing_not_available")
	}
	p, e := i.store.PolicyFor(ctx, site.OwnerUserID, site.ID)
	if e != nil {
		return nil, e
	}
	out := []map[string]string{}
	if p.ProcessingEnabled {
		call, e := langparser.RenderCall("callsProcessingRoomCandidates", map[string]any{"siteId": site.ID})
		rows, e := i.store.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), site.OwnerUserID, "eligible processing rooms", call, e)
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			r := roomFromRow(row)
			if r.SiteID != site.ID || !processingRoomEligible(r, p, i.now()) {
				continue
			}
			j, e := i.store.processing(ctx, site.OwnerUserID, r.ID)
			if e != nil {
				return nil, e
			}
			if j == nil {
				continue
			}
			seats, e := i.store.seatsForRoom(ctx, site.OwnerUserID, r.ID)
			if e != nil {
				return nil, e
			}
			for _, seat := range seats {
				c := j.consent(seat)
				if !c.Safety && !c.Transcription {
					continue
				}
				allowed, e := i.service.processingSeatAuthorized(ctx, r, seat, c)
				if e != nil {
					return nil, e
				}
				if allowed {
					out = append(out, map[string]string{"room_id": r.ID})
					break
				}
			}
		}
	}
	return i.node("processingRooms", map[string]any{"rooms": out, "limit": 100})
}
