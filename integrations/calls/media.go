package calls

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"
)

// media.go -- the media server's control API (design section 7.2).
//
// MEDIA NEVER PASSES THROUGH MEMQL. Participants connect to the media server directly
// with a pass (pass.go); what the engine does is create and end rooms, ask who is in
// one, and act on a participant for the host. MediaBackend is that surface, and
// LiveKit's room service is the one implementation. It is an interface so the call
// logic can be tested against a fake, and so a room's server is a value the logic is
// handed rather than a global it reaches for.

// MediaBackend is one media server's control API.
type MediaBackend interface {
	// CreateRoom opens a room that admits at most maxParticipants connections.
	CreateRoom(ctx context.Context, name string, maxParticipants uint32) error
	// DeleteRoom ends a room and disconnects everyone in it. A room that is already
	// gone is not an error: the state asked for holds.
	DeleteRoom(ctx context.Context, name string) error
	// Participants is who is connected to a room now. A room that is gone has nobody.
	Participants(ctx context.Context, room string) ([]MediaParticipant, error)
	// RemoveParticipant disconnects one participant. One already gone is not an
	// error. It does not stop them rejoining with a pass that is still valid: a
	// self-hosted server cannot revoke a pass, so the caller refuses the rejoin.
	RemoveParticipant(ctx context.Context, room, identity string) error
	// MuteTrack mutes one published track. A track that is not there is an error:
	// nothing was muted.
	MuteTrack(ctx context.Context, room, identity, trackSID string) error
}

// MediaParticipant is one connection to a room. Kind is the participant's
// memql.calls.kind attribute, the seat kind MintPass stamped on their pass, and empty
// for a participant MemQL never admitted.
type MediaParticipant struct {
	Identity, Kind string
	SID            string
	JoinedAt       time.Time
	Tracks         []MediaTrack
}

// MediaTrack is one published track. Source is the LiveKit track source in the
// spelling the grants use (microphone, camera, screen_share, screen_share_audio).
type MediaTrack struct {
	SID, Source string
	Muted       bool
}

// roomService is the part of LiveKit's room service client the backend uses, so a
// test can stand in for the server.
type roomService interface {
	CreateRoom(context.Context, *livekit.CreateRoomRequest) (*livekit.Room, error)
	DeleteRoom(context.Context, *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error)
	ListParticipants(context.Context, *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error)
	RemoveParticipant(context.Context, *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error)
	MutePublishedTrack(context.Context, *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error)
}

// NewLiveKitBackend is the control API of a LiveKit server at apiURL, signed with one
// of its API key pairs. apiURL may be http(s) or ws(s): the client speaks HTTP to
// either. It refuses what cannot work, and the refusal never repeats the secret.
func NewLiveKitBackend(apiURL, key, secret string) (MediaBackend, error) {
	u, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil || !slices.Contains([]string{"http", "https", "ws", "wss"}, u.Scheme) || u.Host == "" {
		return nil, errors.New("calls: a media server's control API needs an http, https, ws or wss URL with a host")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("calls: a media server's control API needs its API key")
	}
	if len(secret) < minMediaSecretLength {
		return nil, fmt.Errorf("calls: a media server's API secret must be at least %d characters", minMediaSecretLength)
	}
	return newLiveKitBackend(lksdk.NewRoomServiceClient(u.String(), key, secret)), nil
}

func newLiveKitBackend(rooms roomService) MediaBackend { return &liveKitBackend{rooms: rooms} }

type liveKitBackend struct{ rooms roomService }

// CreateRoom sends the room's name and its participant cap and nothing else: how long
// an empty room lives and the room's codecs are the server's configuration, and the
// call logic ends rooms itself.
func (b *liveKitBackend) CreateRoom(ctx context.Context, name string, maxParticipants uint32) error {
	if _, err := b.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: name, MaxParticipants: maxParticipants}); err != nil {
		return fmt.Errorf("calls: create media room: %w", err)
	}
	return nil
}

func (b *liveKitBackend) DeleteRoom(ctx context.Context, name string) error {
	if _, err := b.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: name}); err != nil && !isNotFound(err) {
		return fmt.Errorf("calls: delete media room: %w", err)
	}
	return nil
}

func (b *liveKitBackend) Participants(ctx context.Context, room string) ([]MediaParticipant, error) {
	res, err := b.rooms.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("calls: list media room participants: %w", err)
	}
	out := make([]MediaParticipant, 0, len(res.GetParticipants()))
	for _, p := range res.GetParticipants() {
		out = append(out, participantFrom(p))
	}
	return out, nil
}

func (b *liveKitBackend) RemoveParticipant(ctx context.Context, room, identity string) error {
	if _, err := b.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity}); err != nil && !isNotFound(err) {
		return fmt.Errorf("calls: remove media room participant: %w", err)
	}
	return nil
}

func (b *liveKitBackend) MuteTrack(ctx context.Context, room, identity, trackSID string) error {
	req := &livekit.MuteRoomTrackRequest{Room: room, Identity: identity, TrackSid: trackSID, Muted: true}
	if _, err := b.rooms.MutePublishedTrack(ctx, req); err != nil {
		return fmt.Errorf("calls: mute media track: %w", err)
	}
	return nil
}

// participantFrom maps LiveKit's view of a participant. The join time prefers the
// millisecond field, which newer servers fill, over the whole-second one.
func participantFrom(p *livekit.ParticipantInfo) MediaParticipant {
	joined := time.Time{}
	switch {
	case p.GetJoinedAtMs() > 0:
		joined = time.UnixMilli(p.GetJoinedAtMs()).UTC()
	case p.GetJoinedAt() > 0:
		joined = time.Unix(p.GetJoinedAt(), 0).UTC()
	}
	tracks := make([]MediaTrack, 0, len(p.GetTracks()))
	for _, t := range p.GetTracks() {
		tracks = append(tracks, MediaTrack{SID: t.GetSid(), Source: strings.ToLower(t.GetSource().String()), Muted: t.GetMuted()})
	}
	return MediaParticipant{
		SID: p.GetSid(), Identity: p.GetIdentity(),
		Kind:     p.GetAttributes()[KindAttribute],
		JoinedAt: joined,
		Tracks:   tracks,
	}
}

// isNotFound reports whether the server answered that what was asked about does not
// exist. LiveKit's room service answers that as a Twirp not_found.
func isNotFound(err error) bool {
	var twerr twirp.Error
	return errors.As(err, &twerr) && twerr.Code() == twirp.NotFound
}

// Server-origin packets have an empty participant identity. A peer data packet
// must never be accepted as trusted processing merely because its sender is not
// currently resolved in the client's roster.
func (b *liveKitBackend) PublishProcessing(ctx context.Context, room string, data []byte, recipients []string) error {
	sender, ok := b.rooms.(interface {
		SendData(context.Context, *livekit.SendDataRequest) (*livekit.SendDataResponse, error)
	})
	if !ok {
		return errors.New("calls: server data unavailable")
	}
	topic := processingTopic
	_, e := sender.SendData(ctx, &livekit.SendDataRequest{Room: room, Data: data, Kind: livekit.DataPacket_RELIABLE, DestinationIdentities: recipients, Topic: &topic})
	return e
}
