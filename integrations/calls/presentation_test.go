package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestPresentationGrantNeverExpandsNativeKindOrActivatesDevices(t *testing.T) {
	now := time.Now().UTC()
	seat := seatRow{ID: "seat", Identity: "identity", Status: seatAdmitted, JoinedAt: now}
	p := &presentationIntent{Active: true, Presenter: presentationAdmission{SeatID: "other"}, Audience: callrules.PublicationPermissions{}}
	base, _ := callrules.GrantFor(callrules.KindPerson)
	got := presentationGrant(base, p, seat, now)
	require.False(t, got.CanPublish)
	require.Empty(t, got.Sources)
	require.True(t, got.CanSubscribe)
	p.Speaker = presentationAdmission{SeatID: seat.ID, Identity: seat.Identity, JoinedAt: seat.JoinedAt}
	p.SpeakerExpiresAt = now.Add(time.Second)
	got = presentationGrant(base, p, seat, now)
	require.Equal(t, []string{"microphone"}, got.Sources)
	got = presentationGrant(base, p, seat, now.Add(time.Second))
	require.False(t, got.CanPublish)
	p.Presenter = p.Speaker
	avatar, _ := callrules.GrantFor(callrules.KindAvatar)
	require.Equal(t, avatar, presentationGrant(avatar, p, seat, now), "presenter cannot expand avatar to screen publication")
	p.Active = false
	require.Equal(t, base, presentationGrant(base, p, seat, now), "release restores only current native grant")
}

func TestMediaPermissionNarrowsRequiresFenceForRemovedAuthority(t *testing.T) {
	base, _ := callrules.GrantFor(callrules.KindPerson)
	for _, tc := range []struct {
		name string
		edit func(*callrules.GrantSpec)
	}{
		{"publish", func(s *callrules.GrantSpec) { s.CanPublish = false; s.Sources = nil }},
		{"source", func(s *callrules.GrantSpec) { s.Sources = []string{"microphone", "camera"} }},
		{"subscribe", func(s *callrules.GrantSpec) { s.CanSubscribe = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			narrow := base
			tc.edit(&narrow)
			require.True(t, mediaPermissionNarrows(base, narrow))
			require.False(t, mediaPermissionNarrows(narrow, base), "restoration never starts a device")
		})
	}
	require.False(t, mediaPermissionNarrows(base, base))
}
