package calls

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type delayedHostInventory struct {
	MediaBackend
	pause func(context.Context) error
}

func (b delayedHostInventory) Participants(ctx context.Context, room string) ([]MediaParticipant, error) {
	if e := b.pause(ctx); e != nil {
		return nil, e
	}
	return b.MediaBackend.Participants(ctx, room)
}

func TestDelayedHostControlCannotReuseRevokedCredentialOrNewAdmission(t *testing.T) {
	for _, change := range []string{"revoked", "reentered"} {
		t.Run(change, func(t *testing.T) {
			cw, k, host, start, _ := processingFixture(t, true)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			joined, guest, e := cw.join(start.Call.GuestToken, "Guest")
			require.NoError(t, e)
			cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(joined.Identity, MediaTrack{SID: "TR_guest_mic", Source: "microphone"}))
			captured, e := cw.svc.hostOf(ctx, host)
			require.NoError(t, e)
			peer := allowanceReplica(t, cw)
			peer.commerce = cw.svc.commerce
			entered, resume, pause := admissionPause(t)
			cw.svc.media = func(server MediaServer, cred MediaCredential) (MediaBackend, error) {
				b, e := cw.media.backend(server, cred)
				return delayedHostInventory{MediaBackend: b, pause: pause}, e
			}
			finished := make(chan error, 1)
			go func() {
				finished <- cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostMute, Seat: guest.SeatID})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("host inventory did not block")
			}
			if change == "revoked" {
				cred, e := peer.store.credentialByID(ctx, cw.owner, k.result.Credentials[0].ID)
				require.NoError(t, e)
				require.NoError(t, peer.store.updateCredential(ctx, cw.owner, *cred, map[string]any{"revokedAt": cw.clock.Now()}, cw.clock.Now()))
				// A failed provider disconnect leaves the seat occupying until
				// reconciliation; host authority must still be gone immediately.
				peer.media = func(server MediaServer, cred MediaCredential) (MediaBackend, error) {
					b, e := cw.media.backend(server, cred)
					return delayedAdmissionMedia{MediaBackend: b, beforeRemove: func(context.Context) error { return errors.New("provider unavailable") }}, e
				}
				peer.disconnectCredentialSeats(ctx, cw.ref, k.result.Credentials[0].ID)
				require.True(t, seatFromRow(cw.row(seatConcept, captured.seat.ID)).occupying())
			} else {
				require.NoError(t, peer.LeaveWithOptions(ctx, host, departureAsk(t, peer, host, callrules.LeaveKeepRunning, "host-authority-race-leave-0001")))
				back, _, e := peer.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Host returned", Confirm: confirmed})
				require.NoError(t, e)
				require.NotEqual(t, start.Join.Identity, back.Identity)
			}
			resume()
			require.Error(t, <-finished, "a stale request mutated media after its authority changed")
			require.Empty(t, cw.media.mutes())
			deletes := len(cw.media.deletions())
			require.Error(t, cw.svc.end(ctx, captured), "captured old host authority ended the current call")
			require.Len(t, cw.media.deletions(), deletes, "end must recheck before dispatching provider deletion")
			require.Equal(t, "open", cw.row(roomConcept, start.Call.ID)["status"])
		})
	}
}
