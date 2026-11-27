package server

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/sdp/v3"
	"google.golang.org/protobuf/proto"
)

const (
	mediaSignalQueryLimit   = 64 << 10
	mediaSignalJoinLimit    = 128 << 10
	mediaSignalSDPLimit     = 128 << 10
	mediaSignalMessageRate  = 64
	mediaSignalMessageBurst = 128
	mediaSignalByteRate     = 256 << 10
	mediaSignalByteBurst    = 1 << 20
	mediaSignalAddRate      = 1
	mediaSignalAddBurst     = 8
)

// These are per-connection signalling/negotiation budgets. They do not police
// RTP or establish a durable publication count across native replicas. A token
// bucket admits ordinary ICE/initial-negotiation bursts without an unbounded
// queue; an exceeded budget closes the connection and invokes native cleanup.
type mediaSignalBucket struct {
	amount, rate, burst float64
	at                  time.Time
}

func newMediaSignalBucket(now time.Time, rate, burst int) mediaSignalBucket {
	return mediaSignalBucket{float64(burst), float64(rate), float64(burst), now}
}
func (b *mediaSignalBucket) allow(now time.Time, amount int) bool {
	if now.After(b.at) {
		b.amount = min(b.burst, b.amount+now.Sub(b.at).Seconds()*b.rate)
		b.at = now
	}
	if amount < 0 || float64(amount) > b.amount {
		return false
	}
	b.amount -= float64(amount)
	return true
}

type mediaSignalTrack struct {
	source livekit.TrackSource
	sid    string
	mid    string
}
type mediaSignalBudget struct {
	mu                                   sync.Mutex
	messages, bytes, additions, controls mediaSignalBucket
	tracks                               map[string]mediaSignalTrack
	offer                                *livekit.SessionDescription
}

func newMediaSignalBudget(now time.Time) *mediaSignalBudget {
	return &mediaSignalBudget{
		messages:  newMediaSignalBucket(now, mediaSignalMessageRate, mediaSignalMessageBurst),
		bytes:     newMediaSignalBucket(now, mediaSignalByteRate, mediaSignalByteBurst),
		additions: newMediaSignalBucket(now, mediaSignalAddRate, mediaSignalAddBurst),
		controls:  newMediaSignalBucket(now, 4, 16), tracks: map[string]mediaSignalTrack{},
	}
}
func (b *mediaSignalBudget) frame(now time.Time, size int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messages.allow(now, 1) && b.bytes.allow(now, size)
}
func (b *mediaSignalBudget) control(now time.Time, size int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return size <= 125 && b.controls.allow(now, 1)
}

func mediaSignalSource(source livekit.TrackSource, kind livekit.TrackType) bool {
	switch source {
	case livekit.TrackSource_MICROPHONE, livekit.TrackSource_SCREEN_SHARE_AUDIO:
		return kind == livekit.TrackType_AUDIO
	case livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE:
		return kind == livekit.TrackType_VIDEO
	default:
		return false
	}
}
func mediaSignalLayers(layers []*livekit.VideoLayer) bool {
	if len(layers) > 3 {
		return false
	}
	for _, layer := range layers {
		if layer == nil || layer.Quality < livekit.VideoQuality_LOW || layer.Quality > livekit.VideoQuality_HIGH || len(layer.Rid) > 32 {
			return false
		}
	}
	return true
}
func mediaSignalAddShape(track *livekit.AddTrackRequest) bool {
	if track == nil || track.Cid == "" || len(track.Cid) > 256 || len(track.Sid) > 128 || len(track.Name) > 256 || len(track.Stream) > 128 || !mediaSignalSource(track.Source, track.Type) || len(track.SimulcastCodecs) > 2 || !mediaSignalLayers(track.Layers) || len(track.AudioFeatures) > 16 || len(track.PacketTrailerFeatures) > 16 {
		return false
	}
	for _, codec := range track.SimulcastCodecs {
		if codec == nil || len(codec.Codec) > 32 || len(codec.Cid) > 256 || !mediaSignalLayers(codec.Layers) {
			return false
		}
	}
	return true
}
func (b *mediaSignalBudget) add(now time.Time, track *livekit.AddTrackRequest) bool {
	if !mediaSignalAddShape(track) || !b.additions.allow(now, 1) {
		return false
	}
	if prior, ok := b.tracks[track.Cid]; ok {
		return prior.source == track.Source && (track.Sid == "" || prior.sid == track.Sid)
	}
	if track.Sid != "" {
		for _, prior := range b.tracks {
			if prior.sid == track.Sid && prior.source == track.Source {
				return true
			}
		}
		return false
	}
	// Reserve before the provider sees AddTrack. Muting or a caller-supplied
	// SyncState cannot release a reservation; only a provider unpublish can.
	if len(b.tracks) >= 4 {
		return false
	}
	for _, prior := range b.tracks {
		if prior.source == track.Source {
			return false
		}
	}
	b.tracks[track.Cid] = mediaSignalTrack{source: track.Source}
	return true
}

func mediaSignalSDP(description *livekit.SessionDescription) bool {
	if description == nil {
		return true
	}
	if len(description.Sdp) > mediaSignalSDPLimit || (description.Type != "offer" && description.Type != "answer") {
		return false
	}
	var value sdp.SessionDescription
	if value.Unmarshal([]byte(description.Sdp)) != nil || len(value.MediaDescriptions) > 256 {
		return false
	}
	direction := "sendrecv"
	for _, attribute := range value.Attributes {
		if attribute.Key == "sendonly" || attribute.Key == "recvonly" || attribute.Key == "inactive" || attribute.Key == "sendrecv" {
			direction = attribute.Key
		}
	}
	audio, video, data := 0, 0, 0
	for _, media := range value.MediaDescriptions {
		if media.MediaName.Port.Value == 0 {
			continue
		}
		if media.MediaName.Media == "application" {
			data++
			continue
		}
		if media.MediaName.Media != "audio" && media.MediaName.Media != "video" {
			return false
		}
		current := direction
		for _, attribute := range media.Attributes {
			if attribute.Key == "sendonly" || attribute.Key == "recvonly" || attribute.Key == "inactive" || attribute.Key == "sendrecv" {
				current = attribute.Key
			}
		}
		if current == "recvonly" || current == "inactive" {
			continue
		}
		if media.MediaName.Media == "audio" {
			audio++
		} else {
			video++
		}
		tracks, ssrcs := map[string]bool{}, map[string]bool{}
		sendRIDs := 0
		for _, attribute := range media.Attributes {
			fields := strings.Fields(attribute.Value)
			switch attribute.Key {
			case "msid":
				if len(fields) >= 2 {
					tracks[fields[1]] = true
				}
			case "ssrc":
				if len(fields) > 0 {
					ssrcs[fields[0]] = true
				}
				if len(fields) >= 3 && strings.HasPrefix(fields[1], "msid:") {
					tracks[fields[2]] = true
				}
			case "rid":
				if len(fields) >= 2 && fields[1] == "send" {
					sendRIDs++
				}
			}
		}
		// One logical track per sending m-section, with bounded simulcast and
		// its RTX streams. This does not claim to police undeclared RTP bitrate.
		maxSSRC := 6
		if media.MediaName.Media == "audio" {
			maxSSRC = 2
		}
		if len(tracks) > 1 || len(ssrcs) > maxSSRC || sendRIDs > 3 {
			return false
		}
	}
	return audio <= 2 && video <= 2 && data <= 1
}

func mediaSignalSync(state *livekit.SyncState) bool {
	if state == nil {
		return false
	}
	if len(state.PublishTracks) > 4 || len(state.PublishDataTracks) != 0 || len(state.DataChannels) > 2 || !mediaSignalSDP(state.Offer) || !mediaSignalSDP(state.Answer) {
		return false
	}
	sources := map[livekit.TrackSource]bool{}
	for _, published := range state.PublishTracks {
		track := published.GetTrack()
		if track == nil || !mediaSignalSource(track.Source, track.Type) || sources[track.Source] || !mediaSignalLayers(track.Layers) {
			return false
		}
		sources[track.Source] = true
	}
	return true
}

func (b *mediaSignalBudget) request(now time.Time, request *livekit.SignalRequest) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch body := request.Message.(type) {
	case *livekit.SignalRequest_AddTrack:
		return b.add(now, body.AddTrack)
	case *livekit.SignalRequest_Offer:
		return b.publisherOffer(body.Offer)
	case *livekit.SignalRequest_Answer:
		return body.Answer != nil && mediaSignalSDP(body.Answer)
	case *livekit.SignalRequest_SyncState:
		return mediaSignalSync(body.SyncState)
	case *livekit.SignalRequest_UpdateLayers:
		return body.UpdateLayers != nil && mediaSignalLayers(body.UpdateLayers.Layers)
	case *livekit.SignalRequest_Trickle:
		return body.Trickle != nil && len(body.Trickle.CandidateInit) <= 4096
	case *livekit.SignalRequest_Mute, *livekit.SignalRequest_Subscription, *livekit.SignalRequest_TrackSetting,
		*livekit.SignalRequest_Leave, *livekit.SignalRequest_SubscriptionPermission,
		*livekit.SignalRequest_Ping, *livekit.SignalRequest_PingReq,
		*livekit.SignalRequest_UpdateAudioTrack, *livekit.SignalRequest_UpdateVideoTrack:
		return true
	default:
		// No client metadata mutation, provider simulation, data-track or blob
		// service is part of Callkey's scoped signalling protocol.
		return false
	}
}

func (b *mediaSignalBudget) provider(response *livekit.SignalResponse, identity string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if answer := response.GetAnswer(); answer != nil {
		b.publisherAnswer(answer)
	}
	if published := response.GetTrackPublished(); published != nil {
		track := published.GetTrack()
		prior, ok := b.tracks[published.Cid]
		if track == nil || track.Sid == "" || !mediaSignalSource(track.Source, track.Type) {
			return false
		}
		if !ok {
			// A secondary codec keeps the original provider SID and does not
			// create another logical publication or consume another source.
			for _, existing := range b.tracks {
				if existing.sid == track.Sid && existing.source == track.Source {
					return true
				}
			}
			return false
		}
		if prior.source != track.Source {
			return false
		}
		prior.sid = track.Sid
		b.tracks[published.Cid] = prior
	}
	if unpublished := response.GetTrackUnpublished(); unpublished != nil {
		if unpublished.TrackSid == "" {
			return false
		}
		for cid, prior := range b.tracks {
			if prior.sid == unpublished.TrackSid {
				delete(b.tracks, cid)
			}
		}
	}
	if refused := response.GetRequestResponse(); refused != nil && refused.Reason != livekit.RequestResponse_OK && refused.Reason != livekit.RequestResponse_QUEUED {
		if request := refused.GetAddTrack(); request != nil {
			if prior, ok := b.tracks[request.Cid]; ok && prior.sid == "" && prior.source == request.Source {
				delete(b.tracks, request.Cid)
			}
		}
	}
	if joined := response.GetJoin(); joined != nil {
		return b.seed(joined.Participant)
	}
	if update := response.GetUpdate(); update != nil {
		for _, participant := range update.Participants {
			if participant.Identity == identity && !mediaSignalInventory(participant) {
				return false
			}
		}
	}
	return true
}
func (b *mediaSignalBudget) seed(participant *livekit.ParticipantInfo) bool {
	if !mediaSignalInventory(participant) {
		return false
	}
	for _, track := range participant.Tracks {
		matched := false
		for cid, prior := range b.tracks {
			if prior.source == track.Source {
				prior.sid = track.Sid
				b.tracks[cid] = prior
				matched = true
			}
		}
		if !matched {
			b.tracks["provider:"+track.Sid] = mediaSignalTrack{source: track.Source, sid: track.Sid, mid: track.Mid}
		}
	}
	return len(b.tracks) <= 4
}

func mediaSignalInventory(participant *livekit.ParticipantInfo) bool {
	if participant == nil || len(participant.Tracks) > 4 {
		return false
	}
	sources := map[livekit.TrackSource]bool{}
	for _, track := range participant.Tracks {
		if track == nil || track.Sid == "" || !mediaSignalSource(track.Source, track.Type) || sources[track.Source] {
			return false
		}
		sources[track.Source] = true
	}
	return true
}

func (b *mediaSignalBudget) join(now time.Time, query url.Values) bool {
	values := query["join_request"]
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 || values[0] == "" {
		return false
	}
	raw, err := base64.URLEncoding.DecodeString(values[0])
	if err != nil || len(raw) > mediaSignalQueryLimit {
		return false
	}
	var wrapped livekit.WrappedJoinRequest
	if proto.Unmarshal(raw, &wrapped) != nil {
		return false
	}
	decoded := wrapped.JoinRequest
	switch wrapped.Compression {
	case livekit.WrappedJoinRequest_NONE:
	case livekit.WrappedJoinRequest_GZIP:
		reader, e := gzip.NewReader(bytes.NewReader(decoded))
		if e != nil {
			return false
		}
		decoded, err = io.ReadAll(io.LimitReader(reader, mediaSignalJoinLimit+1))
		reader.Close()
		if err != nil {
			return false
		}
	default:
		return false
	}
	if len(decoded) > mediaSignalJoinLimit {
		return false
	}
	var join livekit.JoinRequest
	if proto.Unmarshal(decoded, &join) != nil || len(join.AddTrackRequests) > 4 || join.Metadata != "" || len(join.ParticipantAttributes) != 0 || !mediaSignalSDP(join.PublisherOffer) || join.SyncState != nil && !mediaSignalSync(join.SyncState) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, track := range join.AddTrackRequests {
		if !b.add(now, track) {
			return false
		}
	}
	return (join.PublisherOffer == nil || b.publisherOffer(join.PublisherOffer)) && b.bytes.allow(now, len(decoded))
}

// A normal SDK unpublish receives a publisher SDP answer, but LiveKit does not
// send its originating participant a TrackUnpublished response. Only the matched
// provider answer may release a reservation whose prior sending m-section was
// explicitly disabled. A caller offer or SyncState alone never does so.
func (b *mediaSignalBudget) publisherOffer(offer *livekit.SessionDescription) bool {
	if offer == nil || offer.Type != "offer" || !mediaSignalSDP(offer) || b.offer != nil {
		return false
	}
	var parsed sdp.SessionDescription
	if parsed.Unmarshal([]byte(offer.Sdp)) != nil {
		return false
	}
	for _, media := range parsed.MediaDescriptions {
		mid, _ := media.Attribute("mid")
		for _, attribute := range media.Attributes {
			if attribute.Key != "msid" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 {
				continue
			}
			if track, ok := b.tracks[fields[1]]; ok && track.mid == "" {
				track.mid = mid
				b.tracks[fields[1]] = track
			}
		}
	}
	b.offer = proto.Clone(offer).(*livekit.SessionDescription)
	return true
}
func (b *mediaSignalBudget) publisherAnswer(answer *livekit.SessionDescription) {
	if b.offer == nil || answer.Id != b.offer.Id || answer.Type != "answer" {
		return
	}
	offer := b.offer
	b.offer = nil
	var offered, answered sdp.SessionDescription
	if offered.Unmarshal([]byte(offer.Sdp)) != nil || answered.Unmarshal([]byte(answer.Sdp)) != nil {
		return
	}
	disabled := func(value *sdp.SessionDescription) map[string]bool {
		result := map[string]bool{}
		for _, media := range value.MediaDescriptions {
			mid, present := media.Attribute("mid")
			_, inactive := media.Attribute("inactive")
			if present && (inactive || media.MediaName.Port.Value == 0) {
				result[mid] = true
			}
		}
		return result
	}
	request, confirmed := disabled(&offered), disabled(&answered)
	for cid, track := range b.tracks {
		if track.mid != "" && request[track.mid] && confirmed[track.mid] {
			delete(b.tracks, cid)
		}
	}
}
