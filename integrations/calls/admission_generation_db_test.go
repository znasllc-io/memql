package calls

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type delayedAdmissionMedia struct {
	MediaBackend
	beforeRemove func(context.Context) error
	beforeCreate func(context.Context) error
}

func (b delayedAdmissionMedia) RemoveParticipant(ctx context.Context, room, identity string) error {
	if b.beforeRemove != nil {
		if err := b.beforeRemove(ctx); err != nil {
			return err
		}
	}
	return b.MediaBackend.RemoveParticipant(ctx, room, identity)
}
func (b delayedAdmissionMedia) CreateRoom(ctx context.Context, room string, max uint32) error {
	if b.beforeCreate != nil {
		if err := b.beforeCreate(ctx); err != nil {
			return err
		}
	}
	return b.MediaBackend.CreateRoom(ctx, room, max)
}

func admissionPause(t *testing.T) (<-chan struct{}, func(), func(context.Context) error) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once, resume sync.Once
	finish := func() { resume.Do(func() { close(release) }) }
	t.Cleanup(finish)
	return entered, finish, func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func TestDelayedDepartureCleanupCannotRemoveNewAdmission(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "bound"}[bound], func(t *testing.T) {
			cw := newCallWorld(t, "admission-cleanup", 50)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, started := cw.started("Host")
			first, guest, err := cw.join(started.Call.GuestToken, "Guest")
			require.NoError(t, err)
			peer := allowanceReplica(t, cw)
			ask := callrules.LeaveRequest{}
			if bound {
				ask = departureAsk(t, cw.svc, guest, "", "generation-departure")
			}
			entered, resume, pause := admissionPause(t)
			cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
				backend, err := cw.media.backend(server, credential)
				return delayedAdmissionMedia{MediaBackend: backend, beforeRemove: pause}, err
			}
			finished := make(chan error, 1)
			go func() { finished <- cw.svc.LeaveWithOptions(ctx, guest, ask) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("cleanup was never reached")
			}
			back, session, err := peer.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
			require.NoError(t, err)
			require.Equal(t, guest.SeatID, session.SeatID)
			require.NotEqual(t, first.Identity, back.Identity)
			cw.media.setPresent(started.Call.ID, person(first.Identity), person(back.Identity))
			resume()
			require.NoError(t, <-finished)
			backend := &fakeMediaServer{media: cw.media, server: BootstrapMediaServerID}
			present, err := backend.Participants(ctx, started.Call.ID)
			require.NoError(t, err)
			require.Equal(t, []MediaParticipant{person(back.Identity)}, present, "old cleanup removed the new connection")
			refreshed, _, err := peer.Join(ctx, cw.ref, &session, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
			require.NoError(t, err)
			require.Equal(t, back.Identity, refreshed.Identity, "a connection refresh must retain the admission")
		})
	}
}

func TestDelayedMediaCreationCannotMintRetiredAdmission(t *testing.T) {
	cw := newCallWorld(t, "admission-late-pass", 50)
	if cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, started := cw.started("Host")
	first, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	peer := allowanceReplica(t, cw)
	entered, resume, pause := admissionPause(t)
	cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		backend, err := cw.media.backend(server, credential)
		return delayedAdmissionMedia{MediaBackend: backend, beforeCreate: pause}, err
	}
	type answer struct {
		result callrules.JoinResult
		err    error
	}
	finished := make(chan answer, 1)
	go func() {
		result, _, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
		finished <- answer{result, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("media creation was never reached")
	}
	require.NoError(t, peer.Leave(ctx, guest))
	back, _, err := peer.Join(ctx, cw.ref, &guest, callrules.JoinBody{Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	require.NotEqual(t, first.Identity, back.Identity)
	resume()
	stale := <-finished
	require.ErrorIs(t, stale.err, callrules.ErrConflict)
	require.Equal(t, "admission_changed", callrules.ReasonOf(stale.err))
	require.Empty(t, stale.result.Pass)
}
