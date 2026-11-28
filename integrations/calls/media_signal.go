package calls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

const mediaHandshakeTTL = 20 * time.Second
const mediaSignalLeaseTTL = 30 * time.Second
const mediaForwardBound = 5 * time.Second

func (s *Service) CheckMediaSignal(ctx context.Context, token string) error {
	r, _, _, b, _, err := s.authenticateMediaSignal(ctx, token)
	if err != nil {
		return err
	}
	return s.mediaSignalGated(ctx, *r, b, func(ctx context.Context, current roomRow) error { return s.checkMediaBinding(ctx, current, b) })
}

// BeginMediaSignal is a native admission, not a JWT-only preflight. A durable
// pending intent precedes any upstream effect. Cross-replica room/account gates
// order each bounded write with revocation; no gate spans the WS lifetime.
func (s *Service) BeginMediaSignal(ctx context.Context, token string) (admission *callrules.MediaSignalAdmission, resultErr error) {
	started := time.Now()
	r, server, credential, b, proof, e := s.authenticateMediaSignal(ctx, token)
	if e != nil {
		return nil, e
	}
	trace := &mediaSignalTrace{logger: s.logger, started: started}
	trace.record("authenticate", time.Since(started), nil)
	ctx = mediaSignalTraceContext(ctx, trace)
	defer func() {
		trace.record("begin", time.Since(started), resultErr)
		if resultErr != nil {
			trace.finish()
		}
	}()
	// A cryptographically valid but natively revoked pass cannot trigger
	// journal writes or provider work. Admission is rechecked again after the
	// cleanup gap before registering its pending effect.
	if e = s.mediaSignalGated(ctx, *r, b, func(ctx context.Context, current roomRow) error {
		return s.checkMediaBinding(ctx, current, b)
	}); e != nil {
		return nil, e
	}
	// A refresh follows the old socket's definitive close. Drain and read back
	// that prior connection before allowing the same identity to reconnect.
	if _, e = s.ReconcileMediaSignals(ctx, r.ID); e != nil {
		return nil, e
	}
	intentID, e := callrules.NewToken(s.random, 24)
	if e != nil {
		return nil, e
	}
	var expires time.Time
	name := "Callkey processing"
	var grant callrules.GrantSpec
	e = s.mediaSignalGated(ctx, *r, b, func(ctx context.Context, r roomRow) error {
		if e := s.checkMediaBinding(ctx, r, b); e != nil {
			return e
		}
		var err error
		grant, err = s.currentMediaGrant(ctx, r, b)
		if err != nil {
			return err
		}
		j, e := s.store.mediaSignals(ctx, r)
		if e != nil {
			return e
		}
		if len(j.Intents) >= maxMediaSignalIntents {
			return accountRefusal("media_barrier_limit")
		}
		count := 0
		supersede := ""
		for _, v := range j.Intents {
			if v.Binding.Identity == b.Identity {
				count++
				if v.State == "uncertain" || v.State == "draining" || !s.now().Before(v.ExpiresAt) {
					return accountRefusal("media_cleanup_pending")
				}
				if v.State == "active" && v.Binding == b {
					supersede = v.ID
				}
			}
		}
		if count >= 1 && !(count == 1 && supersede != "") {
			return accountRefusal("media_connection_limit")
		}
		if b.SeatID != "" {
			seat, e := s.store.seatByID(ctx, r.OwnerUserID, b.SeatID)
			if e != nil {
				return e
			}
			if seat == nil {
				return accountRefusal("media_admission_closed")
			}
			name = seat.DisplayName
		}
		// SDK resume may overlap the old signalling socket. Replace only a
		// settled connection for the exact same binding, atomically fencing its
		// callbacks. An old Close cannot remove the replacement. The new pending
		// intent now carries uncertainty for both until LiveKit responds.
		if supersede != "" {
			prior := j.find(supersede)
			if mediaPermissionNarrows(s.signalIntentGrant(r, *prior), grant) {
				// A reconnect can race the gap between committing a policy and
				// its deferred provider reconciliation. Never replace the broad
				// active evidence with a narrow pending grant before removing it.
				settled, err := s.drainMediaPermission(ctx, r, j, b.Identity, "")
				if err != nil {
					return err
				}
				if !settled {
					return accountRefusal("media_cleanup_pending")
				}
			} else {
				j.remove(supersede)
			}
		}
		// Admission reads and gate waits precede the pending effect. Start its
		// bounded lifetime here, using one epoch for both timestamps: a slow
		// admission must not persist ExpiresAt <= CreatedAt and make the entire
		// journal unreadable. The durable write still precedes every upstream call.
		created := s.now()
		expires = created.Add(mediaHandshakeTTL)
		j.Intents = append(j.Intents, mediaSignalIntent{ID: intentID, Binding: b, Grant: grant, GrantDigest: mediaGrantDigest(grant), State: "pending", CreatedAt: created, ExpiresAt: expires})
		return s.store.saveMediaSignals(ctx, r, j, s.now())
	})
	if e != nil {
		return nil, e
	}
	// Ignore incoming provider permission claims: recompute the current native
	// grant. The signed native proof survives provider refreshes unchanged.
	upstream, e := mintPassWithSpec(credential.Key, credential.Secret, callrules.SeatKind(b.Kind), r.MediaRoom, b.Identity, name, map[string]string{mediaAdmissionAttribute: proof}, MaxPassTTL, grant)
	if e != nil {
		return nil, e
	}
	guard := sha256.Sum256([]byte("memql-media-signal-v1:" + credential.Secret))
	lease := &mediaSignalConnection{service: s, room: *r, binding: b, id: intentID, trace: trace}
	return &callrules.MediaSignalAdmission{UpstreamURL: server.APIURL, UpstreamToken: upstream, UpstreamGuard: hex.EncodeToString(guard[:]), Room: r.MediaRoom, Identity: b.Identity, IntentID: intentID, ExpiresAt: expires, Forward: lease.forward, Confirm: lease.confirm, Close: lease.close, Observe: trace.record, Finish: trace.finish}, nil
}

type mediaSignalConnection struct {
	mu      sync.Mutex
	service *Service
	room    roomRow
	binding mediaSignalBinding
	id      string
	closed  bool
	trace   *mediaSignalTrace
}

func (c *mediaSignalConnection) forward(ctx context.Context, fn func(context.Context) error) (resultErr error) {
	started := time.Now()
	ctx = mediaSignalTraceContext(ctx, c.trace)
	defer func() { c.trace.record("forward", time.Since(started), resultErr) }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || fn == nil {
		return accountRefusal("media_connection_closed")
	}
	s := c.service
	return s.mediaSignalGatedSeats(ctx, c.room, c.binding, func(ctx context.Context, r roomRow, seats []seatRow) error {
		readAt := time.Now()
		j, e := s.store.mediaSignals(ctx, r)
		c.trace.record("journal_read", time.Since(readAt), e)
		if e != nil {
			return e
		}
		v := j.find(c.id)
		if v == nil || (v.State != "pending" && v.State != "active") || !s.now().Before(v.ExpiresAt) {
			return accountRefusal("media_connection_expired")
		}
		if e := mediaSignalMeasure(ctx, "authority", func() error { return s.checkMediaBindingSeats(ctx, r, c.binding, true, &seats) }); e != nil {
			return e
		}
		grantAt := time.Now()
		grant, e := s.currentMediaGrant(ctx, r, c.binding)
		c.trace.record("grant", time.Since(grantAt), e)
		if e != nil {
			return e
		}
		digest := mediaGrantDigest(grant)
		if v.GrantDigest != digest {
			// A pending pass was minted for the previous policy. Do not send
			// it upstream after a policy change, even if Begin already returned.
			if v.State == "pending" {
				return accountRefusal("media_permissions_changed")
			}
			if mediaPermissionNarrows(s.signalIntentGrant(r, *v), grant) {
				if _, e = s.drainMediaPermission(ctx, r, j, c.binding.Identity, ""); e != nil {
					return e
				}
				return accountRefusal("media_permissions_changed")
			}
			if e = s.applyMediaPermission(ctx, r, c.binding.Identity, grant); e != nil {
				return e
			}
			v.GrantDigest = digest
			v.Grant = grant
			if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
				return e
			}
		}
		// A provider operation is bounded, and a transport must respect the
		// deadline on both dial and write. An error does not settle the intent.
		bound := mediaForwardBound
		if v.State == "pending" {
			bound = min(mediaForwardBound, v.ExpiresAt.Sub(s.now()))
		}
		write, cancel := context.WithTimeout(ctx, bound)
		defer cancel()
		if e := mediaSignalMeasure(ctx, "provider_write", func() error { return fn(write) }); e != nil {
			return e
		}
		if e := write.Err(); e != nil {
			return e
		}
		if v.State == "active" && v.ExpiresAt.Sub(s.now()) < 20*time.Second {
			v.ExpiresAt = s.now().Add(mediaSignalLeaseTTL)
			err := s.store.saveMediaSignals(ctx, r, j, s.now())
			if err == nil {
				c.trace.record("lease_renewed", 0, nil)
			}
			return err
		}
		return nil
	})
}
func (c *mediaSignalConnection) confirm(ctx context.Context) (resultErr error) {
	started := time.Now()
	ctx = mediaSignalTraceContext(ctx, c.trace)
	defer func() { c.trace.record("confirm", time.Since(started), resultErr) }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accountRefusal("media_connection_closed")
	}
	s := c.service
	return s.mediaSignalGated(ctx, c.room, c.binding, func(ctx context.Context, r roomRow) error {
		j, e := s.store.mediaSignals(ctx, r)
		if e != nil {
			return e
		}
		v := j.find(c.id)
		if v == nil {
			return accountRefusal("media_connection_closed")
		}
		// A definitive response settles uncertainty even after the handshake
		// deadline or revocation. Never turn that late response into authority.
		allowed := mediaSignalMeasure(ctx, "authority", func() error { return s.checkMediaBinding(ctx, r, c.binding) })
		if allowed == nil && !s.now().Before(v.ExpiresAt) {
			allowed = accountRefusal("media_connection_expired")
		}
		if allowed == nil {
			grant, err := s.currentMediaGrant(ctx, r, c.binding)
			if err != nil {
				allowed = err
			} else if digest := mediaGrantDigest(grant); digest != v.GrantDigest {
				// The provider handshake may have landed after narrowing. Fence
				// and remove it; never release a broad initial response. Widening
				// can instead update/read back the current native grant.
				if mediaPermissionNarrows(s.signalIntentGrant(r, *v), grant) {
					_, allowed = s.drainMediaPermission(ctx, r, j, c.binding.Identity, c.id)
					if allowed == nil {
						allowed = accountRefusal("media_permissions_changed")
					}
				} else {
					allowed = s.applyMediaPermission(ctx, r, c.binding.Identity, grant)
					if allowed == nil {
						v.GrantDigest = digest
						v.Grant = grant
					}
				}
			}
		}
		if allowed != nil {
			v.State = "draining"
		} else {
			v.State = "active"
			v.ExpiresAt = s.now().Add(mediaSignalLeaseTTL)
		}
		if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
			return e
		}
		return allowed
	})
}

func (s *Service) applyMediaPermission(ctx context.Context, room roomRow, identity string, grant callrules.GrantSpec) error {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	backend, err := s.backendFor(bounded, room)
	if err != nil {
		return accountRefusal("media_permissions_pending")
	}
	permissions, ok := backend.(MediaPermissionBackend)
	if !ok {
		return accountRefusal("media_permissions_pending")
	}
	if _, err := permissions.UpdatePermissions(bounded, room.MediaRoom, identity, grant); err != nil {
		return accountRefusal("media_permissions_pending")
	}
	return nil
}
func (c *mediaSignalConnection) close(ctx context.Context, definitive bool) (resultErr error) {
	started := time.Now()
	ctx = mediaSignalTraceContext(ctx, c.trace)
	defer func() { c.trace.record("close", time.Since(started), resultErr) }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	s := c.service
	err := s.mediaSignalGated(ctx, c.room, c.binding, func(ctx context.Context, r roomRow) error {
		j, e := s.store.mediaSignals(ctx, r)
		if e != nil {
			return e
		}
		v := j.find(c.id)
		if v == nil {
			return nil
		}
		if v.State == "active" || v.State == "draining" || definitive {
			v.State = "draining"
		} else {
			v.State = "uncertain"
		}
		// Close does not remove the record: established media can outlive the
		// signalling socket. Reconciliation must remove and read back provider.
		return s.store.saveMediaSignals(ctx, r, j, s.now())
	})
	if err != nil {
		return err
	}
	// Closing a socket can be the last event of an ended room, which the open
	// room lifecycle scan no longer visits. Attempt the same explicit provider
	// cleanup now; its durable pending state survives any provider failure.
	_, err = s.ReconcileMediaSignals(ctx, c.room.ID)
	return err
}

// ReconcileMediaSignals completes only provider effects whose handshakes are
// known to have settled. Timeout/crash of a pending handshake is quarantined;
// clearing it requires a definitive transport callback or an explicit verified
// provider-generation reset. This method does not infer reset from an empty room.
// The caller is native lifecycle/status code; no public room-ID-only RPC exists.
func (s *Service) ReconcileMediaSignals(ctx context.Context, roomID string) (callrules.MediaCleanupStatus, error) {
	out := callrules.MediaCleanupStatus{State: "clear"}
	r, e := s.store.roomForMediaRoom(ctx, roomID)
	if e != nil {
		return out, e
	}
	if r == nil {
		return out, callrules.ErrNotFound
	}
	e = s.mediaSignalGatedSeats(ctx, *r, mediaSignalBinding{}, func(ctx context.Context, r roomRow, seats []seatRow) error {
		j, e := s.store.mediaSignals(ctx, r)
		if e != nil {
			return e
		}
		changed := false
		remove := map[string]bool{}
		unknown := map[string]bool{}
		for i := range j.Intents {
			v := &j.Intents[i]
			if v.State == "pending" {
				if !s.now().Before(v.ExpiresAt) {
					v.State = "uncertain"
					changed = true
				}
				unknown[v.Binding.Identity] = true
				continue
			}
			if v.State == "uncertain" {
				unknown[v.Binding.Identity] = true
				continue
			}
			if v.State == "draining" || !s.now().Before(v.ExpiresAt) || s.checkMediaBindingSeats(ctx, r, v.Binding, true, &seats) != nil {
				v.State = "draining"
				remove[v.Binding.Identity] = true
				changed = true
			}
		}
		// Withdraw the earlier readback before provider I/O. A failed new audit
		// must not leave a recent "clear" projection from the preceding pass.
		j.LastVerifiedAt = time.Time{}
		j.LastCleanupReason = "media_cleanup_pending"
		if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
			return e
		}
		// Audit provider inventory even when no journal exists (first rollout,
		// delayed webhook or old unguarded JWT). A seat is not proof its current
		// provider connection passed this boundary. Trusted ingress is exempt
		// only through its exact current native lease, never an identity prefix.
		inventory, cancel := context.WithTimeout(ctx, 3*time.Second)
		backend, e := s.backendFor(inventory, r)
		if e == nil && backend == nil {
			e = accountRefusal("media_server_unavailable")
		}
		var present []MediaParticipant
		if e == nil {
			present, e = backend.Participants(inventory, r.MediaRoom)
		}
		cancel()
		if e != nil {
			return e
		}
		for _, p := range present {
			known := false
			for _, v := range j.Intents {
				if v.Binding.Identity == p.Identity && (v.State == "active" || v.State == "pending") && s.now().Before(v.ExpiresAt) && s.checkMediaBindingSeats(ctx, r, v.Binding, true, &seats) == nil {
					known = true
					break
				}
			}
			if !known {
				allowed, e := s.mediaResourceIdentityAllowed(ctx, r, p.Identity)
				if e != nil {
					return e
				}
				known = allowed
			}
			if !known && !remove[p.Identity] && !unknown[p.Identity] {
				// During rollout a currently authorized legacy participant may
				// remain until reconnect. Preserve it, but never report verified
				// cleanup for a connection with no guarded handshake evidence.
				_, seat, e := s.roomAndSeatOf(ctx, r, p.Identity)
				if e != nil {
					return e
				}
				if seat != nil && seat.occupying() {
					binding, e := s.mediaSeatBinding(ctx, r, *seat)
					if e == nil && s.checkMediaBindingSeats(ctx, r, binding, true, &seats) == nil {
						known = true
						out.State = "pending"
						out.Reason = "legacy_media_unverified"
					}
				}
			}
			if !known {
				remove[p.Identity] = true
			}
		}
		// Persist the deny before asking the provider. Concurrent replica Begin
		// sees draining and refuses that identity until successful readback.
		if changed {
			if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
				return e
			}
		}
		for identity := range remove {
			cleanup, cancel := context.WithTimeout(ctx, 3*time.Second)
			backend, err := s.backendFor(cleanup, r)
			if err == nil && backend == nil {
				err = accountRefusal("media_server_unavailable")
			}
			if err == nil {
				err = backend.RemoveParticipant(cleanup, r.MediaRoom, identity)
			}
			var present []MediaParticipant
			if err == nil {
				present, err = backend.Participants(cleanup, r.MediaRoom)
			}
			cancel()
			if err != nil {
				out.State = "pending"
				out.Reason = "provider_cleanup_pending"
				continue
			}
			found := false
			for _, p := range present {
				if p.Identity == identity {
					found = true
					break
				}
			}
			if found {
				out.State = "pending"
				out.Reason = "provider_cleanup_pending"
				continue
			}
			// Remove a revoked/expired unknown connection too, but absence is
			// not final while its original upstream handshake may still land.
			if unknown[identity] {
				continue
			}
			for i := len(j.Intents) - 1; i >= 0; i-- {
				if j.Intents[i].Binding.Identity == identity {
					j.remove(j.Intents[i].ID)
				}
			}
			if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
				return e
			}
		}
		for _, v := range j.Intents {
			if v.State != "active" {
				out.PendingSignals++
				out.State = "pending"
				if v.State == "uncertain" {
					out.Reason = "handshake_outcome_unknown"
				} else if out.Reason == "" {
					out.Reason = "media_connection_pending"
				}
			}
		}
		// A private ingress can reconnect after RemoveParticipant. Native
		// denial is not completed resource cleanup until DeleteIngress has
		// confirmed the revoked lease is stopped, even on an empty inventory.
		resources, err := s.store.resourceStates(ctx)
		if err != nil {
			return err
		}
		for _, resource := range resources {
			for _, lease := range resource.State.Leases {
				if lease.RoomID != r.ID || !lease.active() {
					continue
				}
				allowed, err := s.mediaResourceIdentityAllowed(ctx, r, lease.Identity)
				if err != nil {
					return err
				}
				if !allowed {
					out.State = "pending"
					if out.Reason == "" {
						out.Reason = "resource_cleanup_pending"
					}
				}
			}
		}
		j.LastCleanupReason = out.Reason
		if out.State == "clear" {
			j.LastVerifiedAt = s.now()
		} else {
			j.LastVerifiedAt = time.Time{}
		}
		return s.store.saveMediaSignals(ctx, r, j, s.now())
	})
	if e != nil {
		out.State = "pending"
		out.Reason = "media_cleanup_unavailable"
	}
	return out, e
}

func (s *Service) mediaResourceIdentityAllowed(ctx context.Context, r roomRow, identity string) (bool, error) {
	if !r.open() || fundingRoomAllowed(r, s.now()) != nil {
		return false, nil
	}
	_, seat, e := s.roomAndSeatOf(ctx, r, identity)
	if e != nil || seat == nil || !seat.occupying() {
		return false, e
	}
	if seat.KeyID == "" {
		return false, nil
	}
	d, e := s.store.resourceState(ctx, r.OwnerUserID, r.SiteID, seat.KeyID)
	if e != nil {
		return false, e
	}
	for _, l := range d.State.Leases {
		if l.RoomID != r.ID || l.SeatID != seat.ID || l.Identity != identity || (l.Status != "awaiting_source" && l.Status != "publishing") || !s.now().Before(l.ExpiresAt) || l.IngressID == "" {
			continue
		}
		source := resourceSource(d, l.ResourceID)
		if source == nil || !callrules.ValidResourceSpec(source.ResourceSpec) {
			return false, nil
		}
		if seat.CredentialID != "" {
			credential, e := s.store.credentialByID(ctx, r.OwnerUserID, seat.CredentialID)
			if e != nil {
				return false, e
			}
			if credential == nil || !credential.RevokedAt.IsZero() {
				return false, nil
			}
		}
		if l.GrantID != "" {
			grant, e := s.store.delegation(ctx, r.OwnerUserID, l.GrantID)
			if e != nil {
				return false, e
			}
			if grant == nil || !grant.State.RevokedAt.IsZero() || !s.now().Before(grant.State.Request.ExpiresAt) || !resourceGrantAllows(grant, l.ResourceID, r) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
