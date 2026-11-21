package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func messageFamilyFixture(t *testing.T) (*callWorld, accountTestKey, callrules.StartResult, roomRow, callrules.FamilyInvitation, callrules.Session, *messageTestMedia) {
	t.Helper()
	cw, guardian, started, room := familyFixture(t, "message-family-authority")
	if cw == nil {
		return nil, guardian, started, room, callrules.FamilyInvitation{}, callrules.Session{}, nil
	}
	invitation, child := familyInviteClaim(t, cw, guardian, started)
	familyApprove(t, cw, guardian, started, invitation)
	cw.media.setPresent(room.MediaRoom, person(started.Join.Identity))
	joined, child, err := cw.svc.Join(context.Background(), cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: invitation.ID, Name: "Synthetic child fixture"})
	require.NoError(t, err)
	cw.media.setPresent(room.MediaRoom, person(started.Join.Identity), person(joined.Identity))
	backend, err := cw.svc.backendFor(context.Background(), room)
	require.NoError(t, err)
	media := &messageTestMedia{MediaBackend: backend}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	return cw, guardian, started, room, invitation, child, media
}

func TestMessageFamilyExcludesChildWhoseConsentEnded(t *testing.T) {
	for _, change := range []string{"expired", "revoked", "approval_credential_missing", "session_ended"} {
		t.Run(change, func(t *testing.T) {
			cw, guardian, _, _, invitation, child, media := messageFamilyFixture(t)
			if cw == nil {
				return
			}
			ctx := context.Background()
			query := messageAsk(t, cw, *guardian.result.Session, 1, "benign family fixture")
			result, err := cw.svc.Message(ctx, *guardian.result.Session, query)
			require.NoError(t, err)
			require.Equal(t, "published", result.Status)
			seat, err := cw.svc.store.seatByID(ctx, cw.owner, child.SeatID)
			require.NoError(t, err)
			require.Contains(t, media.recipients[0], seat.Identity)
			receipt, err := cw.svc.store.family(ctx, cw.owner, invitation.ID)
			require.NoError(t, err)
			switch change {
			case "expired":
				receipt.Data.ExpiresAt = cw.clock.Now()
			case "revoked":
				receipt.Data.Status = "revoked"
				receipt.Data.RevokedAt = cw.clock.Now()
			case "approval_credential_missing":
				receipt.Data.ApprovalCredential = "no-longer-authorized-consent"
			case "session_ended":
				row, _, err := cw.svc.session(ctx, cw.ref, child.Token, cw.clock.Now())
				require.NoError(t, err)
				require.NoError(t, cw.svc.store.updateAccountSession(ctx, cw.owner, row, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
			}
			require.NoError(t, cw.svc.store.saveFamily(ctx, cw.owner, receipt, cw.clock.Now()))
			cw.clock.Advance(time.Second)
			query.Sequence++
			_, err = cw.svc.Message(ctx, *guardian.result.Session, query)
			if err == nil && len(media.recipients) > 1 {
				require.NotContains(t, media.recipients[1], seat.Identity, "recipient eligibility must be checked before reconciliation catches up")
			} else {
				require.Len(t, media.packets, 1, "a terminal family refusal cannot fan out")
			}
		})
	}
}

func TestMessageFamilyRechecksGuardianAfterProviderInventory(t *testing.T) {
	cw, guardian, _, _, _, child, media := messageFamilyFixture(t)
	if cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	query := messageAsk(t, cw, child, 1, "benign delayed child fixture")
	entered, resume, pause := admissionPause(t)
	media.MediaBackend = delayedHostInventory{MediaBackend: media.MediaBackend, pause: pause}
	done := make(chan error, 1)
	go func() { _, err := cw.svc.Message(ctx, child, query); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("family relay never reached guardian presence inventory")
	}
	// The child's account gate does not cover the guardian's credential. The
	// revocation row commits before best-effort media cleanup takes the room gate.
	peer := allowanceReplica(t, cw)
	release, err := peer.gates(ctx, accountGate(guardian.result.Account.KeyID))
	require.NoError(t, err)
	fresh := memql.ContextWithFreshRead(ctx)
	credential, err := peer.store.credentialByID(fresh, cw.owner, guardian.result.Credentials[0].ID)
	require.NoError(t, err)
	require.NoError(t, peer.store.updateCredential(fresh, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
	release()
	resume()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal("family relay did not finish")
	}
	require.Empty(t, media.packets, "old guardian inventory must not authorize a child message")
}

func TestMessageFamilyGuardianLossAndClosingBarrierStopAllFanout(t *testing.T) {
	for _, change := range []string{"not_connected", "closing"} {
		t.Run(change, func(t *testing.T) {
			cw, guardian, _, room, _, child, media := messageFamilyFixture(t)
			if cw == nil {
				return
			}
			ctx := context.Background()
			query := messageAsk(t, cw, *guardian.result.Session, 1, "benign supervision fixture")
			if change == "not_connected" {
				seat, err := cw.svc.store.seatByID(ctx, cw.owner, child.SeatID)
				require.NoError(t, err)
				cw.media.setPresent(room.MediaRoom, person(seat.Identity))
			} else {
				state, err := cw.svc.familyRoomState(ctx, room)
				require.NoError(t, err)
				state.Data.ClosingAt = cw.clock.Now()
				state.Data.Reason = "family_consent_revoked"
				require.NoError(t, cw.svc.store.saveFamily(ctx, cw.owner, state, cw.clock.Now()))
			}
			_, err := cw.svc.Message(ctx, *guardian.result.Session, query)
			require.Error(t, err)
			require.Empty(t, media.packets)
		})
	}
}
