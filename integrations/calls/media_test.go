package calls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

// fakeRooms is a LiveKit room service that records what it was asked and
// answers what a test tells it to.
type fakeRooms struct {
	created  []*livekit.CreateRoomRequest
	deleted  []*livekit.DeleteRoomRequest
	listed   []*livekit.ListParticipantsRequest
	removed  []*livekit.RoomParticipantIdentity
	muted    []*livekit.MuteRoomTrackRequest
	answer   []*livekit.ParticipantInfo
	failWith error
}

func (f *fakeRooms) CreateRoom(_ context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	f.created = append(f.created, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &livekit.Room{Name: req.Name}, nil
}

func (f *fakeRooms) DeleteRoom(_ context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	f.deleted = append(f.deleted, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &livekit.DeleteRoomResponse{}, nil
}

func (f *fakeRooms) ListParticipants(_ context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	f.listed = append(f.listed, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &livekit.ListParticipantsResponse{Participants: f.answer}, nil
}

func (f *fakeRooms) RemoveParticipant(_ context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	f.removed = append(f.removed, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &livekit.RemoveParticipantResponse{}, nil
}

func (f *fakeRooms) MutePublishedTrack(_ context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	f.muted = append(f.muted, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &livekit.MuteRoomTrackResponse{}, nil
}

func TestNewLiveKitBackendRefusesWhatItCannotUse(t *testing.T) {
	secret := strings.Repeat("s", 32)
	for _, tc := range []struct{ name, url, key, secret string }{
		{"no URL", "", "key", secret},
		{"a URL with a scheme LiveKit does not serve", "ftp://media.example.test", "key", secret},
		{"a URL with no host", "http://", "key", secret},
		{"no key", "http://media.example.test", "", secret},
		{"a secret one short of 32 characters", "http://media.example.test", "key", strings.Repeat("s", 31)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := NewLiveKitBackend(tc.url, tc.key, tc.secret)
			if err == nil || backend != nil {
				t.Fatalf("%s built a backend (%v, %v)", tc.name, backend, err)
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("the refusal repeats the secret: %v", err)
			}
		})
	}
	for _, url := range []string{"http://host.k3d.internal:7880", "https://media.example.test", "wss://media.example.test", "ws://127.0.0.1:7880"} {
		if backend, err := NewLiveKitBackend(url, "key", secret); err != nil || backend == nil {
			t.Fatalf("%s was refused: %v", url, err)
		}
	}
}

// TestLiveKitBackendSendsWhatItWasAsked: every call reaches the room service
// with the room, the identity and the track it was given, and nothing more.
func TestLiveKitBackendSendsWhatItWasAsked(t *testing.T) {
	rooms := &fakeRooms{}
	backend := newLiveKitBackend(rooms)
	ctx := context.Background()

	if err := backend.CreateRoom(ctx, "room-a", 11); err != nil {
		t.Fatal(err)
	}
	if err := backend.RemoveParticipant(ctx, "room-a", "seat-1"); err != nil {
		t.Fatal(err)
	}
	if err := backend.MuteTrack(ctx, "room-a", "seat-1", "TR_mic"); err != nil {
		t.Fatal(err)
	}
	if err := backend.DeleteRoom(ctx, "room-a"); err != nil {
		t.Fatal(err)
	}

	if len(rooms.created) != 1 || rooms.created[0].Name != "room-a" || rooms.created[0].MaxParticipants != 11 {
		t.Fatalf("CreateRoom sent %v", rooms.created)
	}
	if len(rooms.removed) != 1 || rooms.removed[0].Room != "room-a" || rooms.removed[0].Identity != "seat-1" {
		t.Fatalf("RemoveParticipant sent %v", rooms.removed)
	}
	if len(rooms.muted) != 1 || rooms.muted[0].Room != "room-a" || rooms.muted[0].Identity != "seat-1" ||
		rooms.muted[0].TrackSid != "TR_mic" || !rooms.muted[0].Muted {
		t.Fatalf("MuteTrack sent %v", rooms.muted)
	}
	if len(rooms.deleted) != 1 || rooms.deleted[0].Room != "room-a" {
		t.Fatalf("DeleteRoom sent %v", rooms.deleted)
	}
}

// TestLiveKitBackendMapsParticipants: what reconcile compares against seats is
// the identity, the seat kind MintPass stamped, when the participant joined and
// each track's id, source and mute state.
func TestLiveKitBackendMapsParticipants(t *testing.T) {
	joined := time.Date(2026, 11, 18, 10, 30, 15, 250_000_000, time.UTC)
	rooms := &fakeRooms{answer: []*livekit.ParticipantInfo{
		{
			Identity:   "seat-person",
			JoinedAtMs: joined.UnixMilli(),
			JoinedAt:   joined.Unix(),
			Attributes: map[string]string{KindAttribute: "person", "other": "x"},
			Tracks: []*livekit.TrackInfo{
				{Sid: "TR_mic", Source: livekit.TrackSource_MICROPHONE, Muted: true},
				{Sid: "TR_screen", Source: livekit.TrackSource_SCREEN_SHARE},
			},
		},
		{Identity: "stranger", JoinedAt: joined.Unix()},
	}}
	got, err := newLiveKitBackend(rooms).Participants(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms.listed) != 1 || rooms.listed[0].Room != "room-a" {
		t.Fatalf("ListParticipants sent %v", rooms.listed)
	}
	if len(got) != 2 {
		t.Fatalf("got %d participants, want 2: %v", len(got), got)
	}
	person := got[0]
	if person.Identity != "seat-person" || person.Kind != "person" || !person.JoinedAt.Equal(joined.Truncate(time.Millisecond)) {
		t.Fatalf("the person maps to %+v", person)
	}
	if len(person.Tracks) != 2 ||
		person.Tracks[0] != (MediaTrack{SID: "TR_mic", Source: "microphone", Muted: true}) ||
		person.Tracks[1] != (MediaTrack{SID: "TR_screen", Source: "screen_share"}) {
		t.Fatalf("the person's tracks map to %+v", person.Tracks)
	}
	// A participant MemQL never admitted carries no kind, and a join time in whole
	// seconds is read when no milliseconds were reported.
	if got[1].Kind != "" || !got[1].JoinedAt.Equal(joined.Truncate(time.Second)) || len(got[1].Tracks) != 0 {
		t.Fatalf("the stranger maps to %+v", got[1])
	}
}

// TestLiveKitBackendTreatsWhatIsGoneAsDone: ending a room that is already gone,
// or removing a participant who already left, has reached the state it was
// asked for, so a retried end or removal is not a failure. Muting a track that
// is not there is: nothing was muted.
func TestLiveKitBackendTreatsWhatIsGoneAsDone(t *testing.T) {
	gone := twirp.NotFoundError("requested room does not exist")
	rooms := &fakeRooms{failWith: gone}
	backend := newLiveKitBackend(rooms)
	ctx := context.Background()

	if err := backend.DeleteRoom(ctx, "room-a"); err != nil {
		t.Fatalf("ending a room that is gone failed: %v", err)
	}
	if err := backend.RemoveParticipant(ctx, "room-a", "seat-1"); err != nil {
		t.Fatalf("removing a participant who is gone failed: %v", err)
	}
	if got, err := backend.Participants(ctx, "room-a"); err != nil || len(got) != 0 {
		t.Fatalf("a room that is gone answered %v, %v; want nobody in it", got, err)
	}
	if err := backend.MuteTrack(ctx, "room-a", "seat-1", "TR_mic"); err == nil {
		t.Fatal("muting a track that is not there reported success")
	}

	broken := errors.New("connection refused")
	rooms.failWith = broken
	for name, call := range map[string]func() error{
		"CreateRoom":        func() error { return backend.CreateRoom(ctx, "room-a", 3) },
		"DeleteRoom":        func() error { return backend.DeleteRoom(ctx, "room-a") },
		"RemoveParticipant": func() error { return backend.RemoveParticipant(ctx, "room-a", "seat-1") },
		"MuteTrack":         func() error { return backend.MuteTrack(ctx, "room-a", "seat-1", "TR_mic") },
		"Participants": func() error {
			_, err := backend.Participants(ctx, "room-a")
			return err
		},
	} {
		if err := call(); !errors.Is(err, broken) {
			t.Errorf("%s hid a real failure: %v", name, err)
		}
	}
}
