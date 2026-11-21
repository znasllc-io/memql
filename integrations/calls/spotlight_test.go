package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSpotlightProjectionRequiresExactAdmissionEvenIfIdentityWasReused(t *testing.T) {
	joined := time.Now().UTC()
	state := spotlightState{Seat: "seat", Identity: "identity", JoinedAt: joined}
	seat := seatRow{ID: "seat", Identity: "identity", JoinedAt: joined, Status: seatAdmitted, DisplayName: "Name"}
	require.NotNil(t, spotlightParticipant(state, []seatRow{seat}))
	seat.JoinedAt = joined.Add(time.Second)
	require.Nil(t, spotlightParticipant(state, []seatRow{seat}))
	seat.JoinedAt, seat.Status = joined, seatWaiting
	require.Nil(t, spotlightParticipant(state, []seatRow{seat}))
}
