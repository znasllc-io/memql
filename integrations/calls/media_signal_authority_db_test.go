package calls

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// Add network/read latency before the real named engine query, preserving its
// context and all native DB gates. This is not an authority-result stub.
type mediaSignalAuthorityLatencyEngine struct {
	Engine
	reads       atomic.Int32
	delay       time.Duration
	journalRead chan struct{}
	journalOnce sync.Once
}

func (e *mediaSignalAuthorityLatencyEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "query ") {
		e.reads.Add(1)
		if e.journalRead != nil && strings.Contains(query, "callsMediaSignalsById") {
			e.journalOnce.Do(func() { close(e.journalRead) })
		}
		delay := e.delay
		if delay == 0 {
			delay = 225 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return e.Engine.Execute(ctx, query)
}

func TestMediaSignalDatabaseReconcileContentionKeepsFreshAuthority(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-reconcile-latency")
	if cw == nil {
		return
	}
	ctx := context.Background()
	joined, _, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	hostLease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	require.NoError(t, hostLease.Confirm(ctx))
	guestLease, err := cw.svc.BeginMediaSignal(ctx, joined.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(hostLease.Identity), person(guestLease.Identity))
	require.NoError(t, guestLease.Confirm(ctx))
	original := cw.svc.store.engine
	delayed := &mediaSignalAuthorityLatencyEngine{Engine: original, delay: 105 * time.Millisecond, journalRead: make(chan struct{})}
	cw.svc.store.engine = delayed
	t.Cleanup(func() { cw.svc.store.engine = original })
	audit := make(chan error, 1)
	started := time.Now()
	go func() {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, err := cw.svc.ReconcileMediaSignals(bounded, start.Call.ID)
		audit <- err
	}()
	select {
	case <-delayed.journalRead:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not reach held-gate journal read")
	}
	var forwarded atomic.Int32
	results := make(chan error, 2)
	for _, lease := range []*callrules.MediaSignalAdmission{hostLease, guestLease} {
		go func() {
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			results <- lease.Forward(bounded, func(context.Context) error { forwarded.Add(1); return nil })
		}()
	}
	first, second := <-results, <-results
	auditErr := <-audit
	cw.svc.store.engine = original
	t.Logf("reconciliation plus two Forward calls: elapsed=%s named_reads=%d forwarded=%d forward_errors=%v,%v audit_error=%v", time.Since(started), delayed.reads.Load(), forwarded.Load(), first, second, auditErr)
	require.NoError(t, auditErr)
	require.NoError(t, first)
	require.NoError(t, second)
	require.Equal(t, int32(2), forwarded.Load())
}

func TestMediaSignalDatabaseConcurrentForwardReadLatencyKeepsFreshAuthority(t *testing.T) {
	cw, host, start := mediaSignalFixture(t, "signal-authority-latency")
	if cw == nil {
		return
	}
	ctx := context.Background()
	joined, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	hostLease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	require.NoError(t, hostLease.Confirm(ctx))
	guestLease, err := cw.svc.BeginMediaSignal(ctx, joined.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(hostLease.Identity), person(guestLease.Identity))
	require.NoError(t, guestLease.Confirm(ctx))
	original := cw.svc.store.engine
	delayed := &mediaSignalAuthorityLatencyEngine{Engine: original}
	cw.svc.store.engine = delayed
	t.Cleanup(func() { cw.svc.store.engine = original })
	var forwarded atomic.Int32
	begin := make(chan struct{})
	results := make(chan error, 2)
	started := time.Now()
	for _, lease := range []*callrules.MediaSignalAdmission{hostLease, guestLease} {
		go func() {
			<-begin
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			results <- lease.Forward(bounded, func(context.Context) error { forwarded.Add(1); return nil })
		}()
	}
	close(begin)
	first, second := <-results, <-results
	cw.svc.store.engine = original
	t.Logf("two contending Forward calls: elapsed=%s named_reads=%d forwarded=%d", time.Since(started), delayed.reads.Load(), forwarded.Load())
	require.NoError(t, first)
	require.NoError(t, second)
	require.Equal(t, int32(2), forwarded.Load())
	// A later native mutation must be observed by the next frame. The prior
	// gate's membership input must never become a connection authority cache.
	require.NoError(t, cw.svc.Leave(ctx, guest))
	require.Error(t, guestLease.Forward(ctx, func(context.Context) error { forwarded.Add(1); return nil }))
	require.Equal(t, int32(2), forwarded.Load())
	require.NoError(t, hostLease.Forward(ctx, func(context.Context) error { forwarded.Add(1); return nil }))
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.Error(t, hostLease.Forward(ctx, func(context.Context) error { forwarded.Add(1); return nil }))
	require.Equal(t, int32(3), forwarded.Load())
}

type mediaSignalFirstSeatsEngine struct {
	Engine
	reads        atomic.Int32
	read, resume chan struct{}
}

func (e *mediaSignalFirstSeatsEngine) Execute(ctx context.Context, query string) (any, error) {
	result, err := e.Engine.Execute(ctx, query)
	if strings.HasPrefix(query, "query ") && strings.Contains(query, "callsSeatsForRoom") && e.reads.Add(1) == 1 {
		close(e.read)
		select {
		case <-e.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, err
}

func TestMediaSignalDatabaseQueuedForwardDiscardsPreGateSeats(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-fresh-membership")
	if cw == nil {
		return
	}
	ctx := context.Background()
	lease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	require.NoError(t, lease.Confirm(ctx))
	r, _, _, binding, _, err := cw.svc.authenticateMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	seat, err := cw.svc.store.seatByID(ctx, cw.owner, binding.SeatID)
	require.NoError(t, err)
	require.NotNil(t, seat)
	release, err := cw.svc.gates(ctx, roomGate(r.ID), accountGate(binding.KeyID), keyGate(binding.KeyID))
	require.NoError(t, err)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(release) })
	original := cw.svc.store.engine
	hook := &mediaSignalFirstSeatsEngine{Engine: original, read: make(chan struct{}), resume: make(chan struct{})}
	cw.svc.store.engine = hook
	t.Cleanup(func() { cw.svc.store.engine = original })
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var forwarded atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- lease.Forward(bounded, func(context.Context) error { forwarded.Store(true); return nil })
	}()
	select {
	case <-hook.read:
	case <-bounded.Done():
		t.Fatal("pre-acquisition membership read did not finish")
	}
	// Commit the native departure while holding the same mutation gates that
	// the queued Forward must acquire; its pre-acquisition row is now stale.
	require.NoError(t, cw.svc.store.updateSeat(ctx, cw.owner, *seat, map[string]any{"status": seatLeft}, cw.clock.Now()))
	close(hook.resume)
	releaseOnce.Do(release)
	require.ErrorContains(t, <-done, "media_admission_closed")
	require.False(t, forwarded.Load())
	require.GreaterOrEqual(t, hook.reads.Load(), int32(2), "authority must use post-acquisition membership")
}

type mediaSignalExpiryInventory struct {
	MediaBackend
	advance func()
	*sync.Once
}

func (b mediaSignalExpiryInventory) Participants(ctx context.Context, room string) ([]MediaParticipant, error) {
	b.Do(b.advance)
	return b.MediaBackend.Participants(ctx, room)
}

func TestMediaSignalDatabaseReconcileRechecksSessionExpiryAfterInventory(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-inventory-expiry")
	if cw == nil {
		return
	}
	ctx := context.Background()
	lease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	require.NoError(t, lease.Confirm(ctx))
	r, _, _, binding, _, err := cw.svc.authenticateMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	session, err := cw.svc.store.sessionByID(ctx, cw.owner, binding.SessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	release, err := cw.svc.gates(ctx, accountGate(binding.KeyID))
	require.NoError(t, err)
	err = cw.svc.store.updateAccountSession(ctx, cw.owner, session, map[string]any{"expiresAt": versionStamp(cw.clock.Now().Add(time.Second))}, cw.clock.Now())
	release()
	require.NoError(t, err)
	original := cw.svc.media
	var once sync.Once
	cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		backend, err := original(server, credential)
		return mediaSignalExpiryInventory{backend, func() { cw.clock.Advance(2 * time.Second) }, &once}, err
	}
	t.Cleanup(func() { cw.svc.media = original })
	status, err := cw.svc.ReconcileMediaSignals(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, "clear", status.State)
	require.Empty(t, cw.media.present[r.MediaRoom], "pre-inventory authority cannot survive native session expiry")
	var forwarded bool
	require.Error(t, lease.Forward(ctx, func(context.Context) error { forwarded = true; return nil }))
	require.False(t, forwarded)
}
