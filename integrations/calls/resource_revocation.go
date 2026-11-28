package calls

import (
	"context"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// resourceRevocation is an unexported, call-local description of an already
// committed refusal. No registry pointer/index or publisher credential crosses
// the unlocked provider phase. Each finalizer rereads and merges only this lease.
type resourceRevocation struct {
	OwnerUserID, SiteID, KeyID string
	Room                       roomRow
	Lease                      resourceLease
	SourcePresent              bool
	SourceRevision             int
	SourceSpec                 callrules.ResourceSpec
}

func resourceGates(r roomRow) []string {
	return []string{accountGate(r.KeyID), keyGate(r.KeyID), commerceGlobalGate, roomGate(r.ID), serverGate(r.MediaServerID)}
}
func sameResourceRoom(a, b roomRow) bool {
	return a.ID == b.ID && a.OwnerUserID == b.OwnerUserID && a.SiteID == b.SiteID && a.KeyID == b.KeyID && a.MediaServerID == b.MediaServerID && a.MediaRoom == b.MediaRoom
}
func sameResourceClose(a, b *resourceRoomClose) bool {
	return (a == nil && b == nil) || (a != nil && b != nil &&
		a.OwnerUserID == b.OwnerUserID && a.SiteID == b.SiteID && a.KeyID == b.KeyID && a.RoomID == b.RoomID &&
		a.MediaServerID == b.MediaServerID && a.MediaRoom == b.MediaRoom && a.RequestedAt.Equal(b.RequestedAt))
}
func (c resourceRoomClose) matches(r roomRow) bool {
	return c.OwnerUserID == r.OwnerUserID && c.SiteID == r.SiteID && c.KeyID == r.KeyID && c.RoomID == r.ID && c.MediaServerID == r.MediaServerID && c.MediaRoom == r.MediaRoom
}
func resourceRevocationSnapshot(d *resourceRow, l resourceLease, room roomRow) resourceRevocation {
	p := resourceRevocation{OwnerUserID: d.State.OwnerUserID, SiteID: d.SiteID, KeyID: d.KeyID, Room: room, Lease: l}
	if l.RoomClosing != nil {
		c := *l.RoomClosing
		p.Lease.RoomClosing = &c
	}
	if source := resourceSource(d, l.ResourceID); source != nil {
		p.SourcePresent = true
		p.SourceRevision = source.Revision
		p.SourceSpec = source.ResourceSpec
	}
	return p
}

// Caller owns the account/key/room/server gates. Revoking still occupies the
// native seat and global resource allowance, even if provider absence is known.
func (s *Service) prepareResourceRevocation(ctx context.Context, d *resourceRow, index int, room roomRow) (resourceRevocation, error) {
	l := &d.State.Leases[index]
	if l.RoomID != room.ID || d.State.OwnerUserID != room.OwnerUserID || d.SiteID != room.SiteID || d.KeyID != room.KeyID || !l.active() {
		return resourceRevocation{}, resourceRefusal("resource_state_changed")
	}
	l.Status = "revoking"
	if err := s.store.saveResourceState(ctx, d, s.now()); err != nil {
		return resourceRevocation{}, err
	}
	return resourceRevocationSnapshot(d, *l, room), nil
}

// No call gate or calls-share slot may span this protocol. A failed or unknown
// outcome leaves the durable refusal/reservation for the existing reconciler.
func (s *Service) applyResourceRevocation(ctx context.Context, p resourceRevocation) error {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	backend, err := s.resourceMediaFor(bounded, p.Room)
	if err != nil {
		return err
	}
	ingress, err := backend.FindResourceIngress(bounded, p.Room.MediaRoom, p.Lease.ID)
	if err != nil {
		return err
	}
	if ingress != nil {
		if ingress.Identity != p.Lease.Identity || ingress.ID == "" || (p.Lease.IngressID != "" && ingress.ID != p.Lease.IngressID) {
			return resourceRefusal("resource_gateway_ambiguous")
		}
		if err = backend.DeleteResourceIngress(bounded, ingress.ID); err != nil {
			return err
		}
	}
	remaining, err := backend.FindResourceIngress(bounded, p.Room.MediaRoom, p.Lease.ID)
	if err != nil {
		return err
	}
	if remaining != nil {
		return resourceRefusal("resource_revocation_pending")
	}
	media, err := s.backendFor(bounded, p.Room)
	if err != nil {
		return err
	}
	if media == nil {
		return resourceRefusal("resource_gateway_unavailable")
	}
	return media.RemoveParticipant(bounded, p.Room.MediaRoom, p.Lease.Identity)
}

func sameResourceLease(a, b resourceLease) bool {
	return a.ID == b.ID && a.ResourceID == b.ResourceID && a.RoomID == b.RoomID && a.SeatID == b.SeatID && a.Identity == b.Identity && a.IngressID == b.IngressID && a.RequestID == b.RequestID && a.Digest == b.Digest && a.GrantID == b.GrantID && a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt) && sameResourceClose(a.RoomClosing, b.RoomClosing)
}
func (s *Service) resourceRevocationState(ctx context.Context, p resourceRevocation) (*resourceRow, roomRow, error) {
	d, err := s.store.resourceState(ctx, p.OwnerUserID, p.SiteID, p.KeyID)
	if err != nil {
		return nil, roomRow{}, err
	}
	room, err := s.store.roomByID(ctx, p.OwnerUserID, p.Room.ID)
	if err != nil {
		return nil, roomRow{}, err
	}
	if room == nil || !sameResourceRoom(*room, p.Room) {
		return nil, roomRow{}, resourceRefusal("resource_state_changed")
	}
	return d, *room, nil
}

// The caller has reacquired the canonical gates and supplies freshly read state.
// A stopped exact lease is a no-op; a changed/missing one never licenses writing
// through an old slice index. Human holds and unrelated registry edits survive.
func (s *Service) finalizeResourceRevocation(ctx context.Context, d *resourceRow, room roomRow, p resourceRevocation) error {
	if !sameResourceRoom(room, p.Room) || d.State.OwnerUserID != p.OwnerUserID || d.SiteID != p.SiteID || d.KeyID != p.KeyID {
		return resourceRefusal("resource_state_changed")
	}
	for i := range d.State.Leases {
		l := &d.State.Leases[i]
		if l.ID != p.Lease.ID {
			continue
		}
		if !sameResourceLease(*l, p.Lease) {
			return resourceRefusal("resource_state_changed")
		}
		if l.Status == "stopped" {
			return nil
		}
		if l.Status != "revoking" || (l.RoomClosing != nil && room.open()) {
			return resourceRefusal("resource_revocation_pending")
		}
		source := resourceSource(d, l.ResourceID)
		if (source != nil) != p.SourcePresent || (source != nil && (source.Revision != p.SourceRevision || source.ResourceSpec != p.SourceSpec)) {
			return resourceRefusal("resource_state_changed")
		}
		seat, err := s.store.seatByID(ctx, p.OwnerUserID, l.SeatID)
		if err != nil {
			return err
		}
		if seat != nil {
			// The seat's site is inherited from the exact room checked above.
			if seat.RoomID != l.RoomID || seat.KeyID != p.KeyID || seat.Identity != l.Identity {
				return resourceRefusal("resource_state_changed")
			}
			if seat.occupying() {
				if err = s.store.updateSeat(ctx, p.OwnerUserID, *seat, map[string]any{"status": seatLeft, "leftAt": s.now()}, memql.VersionTimeAfter(seat.Version, s.now())); err != nil {
					return err
				}
			}
		}
		l.Status = "stopped"
		return s.store.saveResourceState(ctx, d, s.now())
	}
	return resourceRefusal("resource_state_changed")
}
func (s *Service) completeResourceRevocation(ctx context.Context, p resourceRevocation) error {
	if p.Lease.RoomClosing != nil {
		return s.resumeResourceRoomEnd(ctx, resourceRoomEnd{Room: p.Room, Close: *p.Lease.RoomClosing})
	}
	if err := s.applyResourceRevocation(ctx, p); err != nil {
		return err
	}
	release, err := s.gates(ctx, resourceGates(p.Room)...)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, room, err := s.resourceRevocationState(fresh, p)
	if err != nil {
		return err
	}
	return s.finalizeResourceRevocation(fresh, d, room, p)
}

// A response exposing the private registry is not authorized by a stale caller
// snapshot. Revocation itself remains durable if that caller has since expired.
func (s *Service) resourceAnswerAfterRevocation(ctx context.Context, sess callrules.Session, req callrules.ResourceRequest, key string) (callrules.ResourceResult, error) {
	var empty callrules.ResourceResult
	release, err := s.gates(ctx, accountGate(key), keyGate(key))
	if err != nil {
		return empty, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err := s.authenticatedKey(fresh, sess)
	if err != nil {
		return empty, err
	}
	if h.row.KeyID != key {
		return empty, callrules.ErrForbidden
	}
	if err = s.delegatedEffectAllowed(fresh, h.site, key); err != nil {
		return empty, err
	}
	if err = s.resourceDelegationAllowed(fresh, h, req, roomRow{}); err != nil {
		return empty, err
	}
	d, err := s.store.resourceState(fresh, h.site.OwnerUserID, h.site.SiteID, key)
	if err != nil {
		return empty, err
	}
	return resourceAnswer(d), nil
}

// The old source must not be replaced by an earlier switch after a later stop,
// credential loss or registry edit. Cleanup can finish; replacement authority
// always requires fresh checks and an unchanged continuation version.
func (s *Service) continueResourceSwitch(ctx context.Context, sess callrules.Session, req callrules.ResourceRequest, version time.Time, plans []resourceRevocation) (callrules.ResourceResult, error) {
	var empty callrules.ResourceResult
	for _, p := range plans {
		if err := s.applyResourceRevocation(ctx, p); err != nil {
			return empty, err
		}
	}
	p := plans[0]
	release, err := s.gates(ctx, resourceGates(p.Room)...)
	if err != nil {
		return empty, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, room, err := s.resourceRevocationState(fresh, p)
	if err != nil {
		return empty, err
	}
	unchanged := d.Version.Equal(version)
	for _, plan := range plans {
		if err = s.finalizeResourceRevocation(fresh, d, room, plan); err != nil {
			return empty, err
		}
	}
	if !unchanged {
		return empty, resourceRefusal("resource_state_changed")
	}
	h, err := s.authenticatedKey(fresh, sess)
	if err != nil {
		return empty, err
	}
	if h.row.KeyID != p.KeyID || h.site.OwnerUserID != p.OwnerUserID || h.site.SiteID != p.SiteID {
		return empty, callrules.ErrForbidden
	}
	host, err := s.hostOf(fresh, sess)
	if err != nil {
		return empty, err
	}
	if !sameResourceRoom(host.room, room) {
		return empty, callrules.ErrForbidden
	}
	if err = s.delegatedEffectAllowed(fresh, h.site, h.row.KeyID); err != nil {
		return empty, err
	}
	if err = s.resourceDelegationAllowed(fresh, h, req, host.room); err != nil {
		return empty, err
	}
	// startResource repeats source revision, human hold, funding, presentation,
	// policy and capacity checks on this fresh state. No stale pointer survives.
	return s.startResource(fresh, h, d, host.room, req)
}

// prepareResourceRoomEnd is reached only by the freshly authorized host End.
// Persisted close authority is scoped to the immutable room tuple and can only
// finish that end, including if the initiating session expires during I/O.
type resourceRoomEnd struct {
	Room  roomRow
	Close resourceRoomClose
}

func (s *Service) prepareResourceRoomEnd(ctx context.Context, h acting) (*resourceRoomEnd, error) {
	if h.room.KeyID == "" {
		return nil, nil
	}
	keys := append(resourceGates(h.room), endGates(h.room)...)
	if h.seat.KeyID != "" && h.seat.KeyID != h.room.KeyID {
		keys = append(keys, accountGate(h.seat.KeyID), keyGate(h.seat.KeyID))
	}
	// gates sorts keys; avoid duplicate advisory acquisitions for shared keys.
	keys = uniqueResourceGates(keys)
	release, err := s.gates(ctx, keys...)
	if err != nil {
		return nil, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.stillHost(fresh, h)
	if err != nil {
		return nil, err
	}
	if !sameResourceRoom(room, h.room) {
		return nil, resourceRefusal("resource_state_changed")
	}
	d, err := s.store.resourceState(fresh, room.OwnerUserID, room.SiteID, room.KeyID)
	if err != nil {
		return nil, err
	}
	close := &resourceRoomClose{OwnerUserID: room.OwnerUserID, SiteID: room.SiteID, KeyID: room.KeyID, RoomID: room.ID, MediaServerID: room.MediaServerID, MediaRoom: room.MediaRoom, RequestedAt: s.now()}
	// Replica clocks can differ. A new narrowing intent cannot precede one of
	// the commitments it captures; this timestamp never grants extra lifetime.
	for _, l := range d.State.Leases {
		if l.RoomID == room.ID && l.active() && l.CreatedAt.After(close.RequestedAt) {
			close.RequestedAt = l.CreatedAt
		}
	}
	// A retry joins the earlier durable intent; its timestamp never replaces it.
	found := false
	for i := range d.State.ClosingRooms {
		if d.State.ClosingRooms[i].RoomID == room.ID {
			close = &d.State.ClosingRooms[i]
			if !close.matches(room) {
				return nil, resourceRefusal("resource_state_changed")
			}
			found = true
			break
		}
	}
	if !found {
		if len(d.State.ClosingRooms) >= callrules.MaxResourceLeases {
			return nil, resourceRefusal("resource_limit")
		}
		d.State.ClosingRooms = append(d.State.ClosingRooms, *close)
	}
	for i := range d.State.Leases {
		l := &d.State.Leases[i]
		if l.RoomID != room.ID || !l.active() {
			continue
		}
		if l.RoomClosing != nil && !sameResourceClose(l.RoomClosing, close) {
			return nil, resourceRefusal("resource_state_changed")
		}
		l.RoomClosing = close
		l.Status = "revoking"
	}
	// One version commits the complete room fence. A crash cannot leave one
	// closing lease beside another unmarked lease that recovery cannot own.
	if err = s.store.saveResourceState(fresh, d, s.now()); err != nil {
		return nil, err
	}
	return &resourceRoomEnd{Room: room, Close: *close}, nil
}
func uniqueResourceGates(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Recovery never depends on the original caller remaining signed in. Only a
// validated durable close intent can authorize this exact room end. Absence of
// an ingress alone cannot release its resource fence while the room is open.
func (s *Service) resumeResourceRoomEnd(ctx context.Context, ending resourceRoomEnd) error {
	close := &ending.Close
	p := resourceRevocation{OwnerUserID: close.OwnerUserID, SiteID: close.SiteID, KeyID: close.KeyID, Room: ending.Room}
	if !close.matches(p.Room) {
		return resourceRefusal("resource_state_changed")
	}
	keys := uniqueResourceGates(append(resourceGates(p.Room), endGates(p.Room)...))
	release, err := s.gates(ctx, keys...)
	if err != nil {
		return err
	}
	var plans []resourceRevocation
	completed := false
	err = func() error {
		defer release()
		fresh := memql.ContextWithFreshRead(ctx)
		d, room, e := s.resourceRevocationState(fresh, p)
		if e != nil {
			return e
		}
		matched := slices.ContainsFunc(d.State.ClosingRooms, func(c resourceRoomClose) bool { return sameResourceClose(&c, close) })
		for _, l := range d.State.Leases {
			if l.RoomID != room.ID || !l.active() {
				continue
			}
			if l.Status != "revoking" || !sameResourceClose(l.RoomClosing, close) {
				return resourceRefusal("resource_state_changed")
			}
			plans = append(plans, resourceRevocationSnapshot(d, l, room))
		}
		if !matched {
			if room.open() || len(plans) != 0 {
				return resourceRefusal("resource_state_changed")
			}
			completed = true
		}
		return nil
	}()
	if err != nil {
		return err
	}
	if completed {
		return nil
	}
	for _, plan := range plans {
		if err = s.applyResourceRevocation(ctx, plan); err != nil {
			return err
		}
	}
	// The room deletion has its own existing effect; give internal recovery a
	// finite provider budget too, rather than borrowing an expired HTTP request.
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	backend, err := s.backendFor(bounded, p.Room)
	if err == nil {
		if backend != nil {
			err = backend.DeleteRoom(bounded, p.Room.MediaRoom)
		} else if len(plans) != 0 {
			err = resourceRefusal("resource_gateway_unavailable")
		}
	}
	cancel()
	if err != nil {
		return err
	}
	release, err = s.gates(ctx, keys...)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, room, err := s.resourceRevocationState(fresh, p)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(d.State.ClosingRooms, func(c resourceRoomClose) bool { return sameResourceClose(&c, close) }) {
		if room.open() || slices.ContainsFunc(d.State.Leases, func(l resourceLease) bool { return l.RoomID == room.ID && l.active() }) {
			return resourceRefusal("resource_state_changed")
		}
		return nil
	}
	// No new active lease may be swept into a previously authorized close.
	for _, l := range d.State.Leases {
		if l.RoomID == room.ID && l.active() {
			found := false
			for _, plan := range plans {
				if sameResourceLease(l, plan.Lease) {
					found = true
				}
			}
			if !found || l.Status != "revoking" {
				return resourceRefusal("resource_state_changed")
			}
		}
	}
	if room.open() {
		v, e := s.viewRoom(fresh, room)
		if e != nil {
			return e
		}
		if e = s.endRows(fresh, v, callrules.EndReasonEnded, s.now()); e != nil {
			return e
		}
		current, e := s.store.roomByID(fresh, p.OwnerUserID, room.ID)
		if e != nil {
			return e
		}
		if current == nil || current.open() || !sameResourceRoom(*current, room) {
			return resourceRefusal("resource_state_changed")
		}
		room = *current
	}
	for _, plan := range plans {
		if err = s.finalizeResourceRevocation(fresh, d, room, plan); err != nil {
			return err
		}
	}
	d.State.ClosingRooms = slices.DeleteFunc(d.State.ClosingRooms, func(c resourceRoomClose) bool { return sameResourceClose(&c, close) })
	return s.store.saveResourceState(fresh, d, s.now())
}
