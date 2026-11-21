package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func resourceRefusal(reason string) error {
	return callrules.Refuse(callrules.ErrConflict, reason, "the resource operation was refused")
}
func resourceDigest(req callrules.ResourceRequest) string {
	data, _ := json.Marshal(req)
	return callrules.HistoryDigest(nil, "resource-operation", string(data))
}
func resourceSource(d *resourceRow, id string) *callrules.ResourceInfo {
	for i := range d.State.Sources {
		if d.State.Sources[i].ID == id {
			return &d.State.Sources[i]
		}
	}
	return nil
}
func resourceAnswer(d *resourceRow) callrules.ResourceResult {
	out := callrules.ResourceResult{Resources: slices.Clone(d.State.Sources), Capabilities: callrules.ResourceCapabilities{Protocols: []string{"whip"}, MaxSources: callrules.MaxResources, MaxActivePerRoom: 1, MaxLeaseSeconds: int(callrules.ResourceLeaseLifetime.Seconds()), Available: true, Readiness: "checked_on_start"}}
	for i := range out.Resources {
		v := &out.Resources[i]
		v.Status = "registered"
		v.RoomID, v.LeaseID, v.ExpiresAt = "", "", nil
		for _, l := range d.State.Leases {
			if l.ResourceID == v.ID && l.StoppedReason != "" {
				v.LastReason = l.StoppedReason
			}
			if l.ResourceID == v.ID && l.active() {
				if l.StoppedReason == "" {
					v.LastReason = ""
				}
				v.Status = l.Status
				v.RoomID, v.LeaseID = l.RoomID, l.ID
				expires := l.ExpiresAt
				v.ExpiresAt = &expires
			}
		}
	}
	return out
}

// Resource is the account/room gate shared by the human UI and delegated agent
// dispatcher. Sources contain output descriptions only. The gateway key is
// returned solely by an approved start; private source/device credentials never
// enter this service.
func (s *Service) Resource(ctx context.Context, site callrules.SiteRef, token string, req callrules.ResourceRequest) (callrules.ResourceResult, error) {
	var empty callrules.ResourceResult
	if callrules.ResourceScope(req.Action) == "" || len(req.ResourceID) > 128 || len(req.RoomID) > 128 || len(req.RequestID) > 128 {
		return empty, callrules.ErrInvalid
	}
	sess := callrules.Session{Site: site, Token: token}
	h, err := s.authenticatedKey(ctx, sess)
	if err != nil {
		return empty, err
	}
	site = h.site
	publishing := req.Action == "start" || req.Action == "switch" || req.Action == "stop"
	var host acting
	gateKeys := []string{accountGate(h.row.KeyID), keyGate(h.row.KeyID)}
	if publishing {
		host, err = s.hostOf(ctx, sess)
		if err != nil {
			return empty, err
		}
		if req.RoomID == "" || host.room.ID != req.RoomID || host.room.KeyID != h.row.KeyID {
			return empty, callrules.ErrForbidden
		}
		gateKeys = append(gateKeys, commerceGlobalGate, roomGate(host.room.ID), serverGate(host.room.MediaServerID))
	}
	release, err := s.gates(ctx, gateKeys...)
	if err != nil {
		return empty, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err = s.authenticatedKey(fresh, sess)
	if err != nil {
		return empty, err
	}
	if req.Action != "list" && req.Action != "status" {
		if err = s.delegatedEffectAllowed(fresh, site, h.row.KeyID); err != nil {
			return empty, err
		}
	}
	d, err := s.store.resourceState(fresh, site.OwnerUserID, site.SiteID, h.row.KeyID)
	if err != nil {
		return empty, err
	}
	if publishing {
		host, err = s.hostOf(fresh, sess)
		if err != nil {
			return empty, err
		}
		if host.room.ID != req.RoomID || host.room.KeyID != h.row.KeyID {
			return empty, callrules.ErrForbidden
		}
	}
	if err = s.resourceDelegationAllowed(fresh, h, req, host.room); err != nil {
		return empty, err
	}
	source := resourceSource(d, req.ResourceID)
	switch req.Action {
	case "list", "status":
		return resourceAnswer(d), nil
	case "register":
		if req.Resource == nil || !callrules.ValidResourceSpec(*req.Resource) || !fundingOperationID.MatchString(req.RequestID) || req.ResourceID != "" {
			return empty, callrules.ErrInvalid
		}
		if prior := d.State.Registrations[req.RequestID]; prior != "" {
			if v := resourceSource(d, prior); v != nil && v.ResourceSpec == *req.Resource {
				return resourceAnswer(d), nil
			}
			return empty, resourceRefusal("resource_request_conflict")
		}
		if len(d.State.Sources) >= callrules.MaxResources || len(d.State.Registrations) >= 128 {
			return empty, resourceRefusal("resource_limit")
		}
		sourceID := id.NewShortId()
		d.State.Sources = append(d.State.Sources, callrules.ResourceInfo{ID: sourceID, ResourceSpec: *req.Resource, Revision: 1, Status: "ready"})
		d.State.Registrations[req.RequestID] = sourceID
	case "update", "unregister":
		if source == nil {
			return empty, callrules.ErrNotFound
		}
		if source.Revision != req.ExpectedRevision {
			return empty, resourceRefusal("resource_revision_conflict")
		}
		for _, l := range d.State.Leases {
			if l.ResourceID == source.ID && l.active() {
				return empty, resourceRefusal("resource_in_use")
			}
		}
		if req.Action == "update" {
			if req.Resource == nil || !callrules.ValidResourceSpec(*req.Resource) {
				return empty, callrules.ErrInvalid
			}
			source.ResourceSpec = *req.Resource
			source.Revision++
		} else {
			d.State.Sources = slices.DeleteFunc(d.State.Sources, func(v callrules.ResourceInfo) bool { return v.ID == req.ResourceID })
		}
	case "stop":
		if source == nil {
			return empty, callrules.ErrNotFound
		}
		if ctx.Value(agentCallContext{}) == nil {
			source.HumanHold = true
			if err = s.store.saveResourceState(fresh, d, s.now()); err != nil {
				return empty, err
			}
		}
		for i := range d.State.Leases {
			l := &d.State.Leases[i]
			if l.RoomID == host.room.ID && l.ResourceID == req.ResourceID && l.active() {
				l.StoppedReason = "host_stopped"
				if err = s.revokeResource(fresh, d, i, host.room); err != nil {
					return empty, err
				}
			}
		}
		return resourceAnswer(d), nil
	case "start", "switch":
		return s.startResource(fresh, h, d, host.room, req)
	}
	if err = s.store.saveResourceState(fresh, d, s.now()); err != nil {
		return empty, err
	}
	return resourceAnswer(d), nil
}

func (s *Service) startResource(ctx context.Context, h keyHolder, d *resourceRow, room roomRow, req callrules.ResourceRequest) (callrules.ResourceResult, error) {
	var empty callrules.ResourceResult
	source := resourceSource(d, req.ResourceID)
	if source == nil {
		return empty, callrules.ErrNotFound
	}
	if !callrules.ValidResourceSpec(source.ResourceSpec) {
		return empty, resourceRefusal("unsupported_track_isolation")
	}
	if source.Revision != req.ExpectedRevision {
		return empty, resourceRefusal("resource_revision_conflict")
	}
	if source.HumanHold && ctx.Value(agentCallContext{}) != nil {
		return empty, resourceRefusal("resource_human_hold")
	}
	if !fundingOperationID.MatchString(req.RequestID) {
		return empty, callrules.ErrInvalid
	}
	now := s.now()
	if err := fundingRoomAllowed(room, now); err != nil {
		return empty, err
	}
	if err := s.resourceBudgetAllowed(room); err != nil {
		return empty, err
	}
	if err := s.delegatedEffectAllowed(ctx, h.site, h.row.KeyID); err != nil {
		return empty, err
	}
	if err := s.commerceAdultAllowed(ctx, h.site, h.row.KeyID); err != nil {
		return empty, err
	}
	digest := resourceDigest(req)
	for i := range d.State.Leases {
		l := &d.State.Leases[i]
		if l.RequestID == req.RequestID {
			if l.Digest != digest {
				return empty, resourceRefusal("resource_request_conflict")
			}
			if !l.active() || l.Status == "revoking" || !now.Before(l.ExpiresAt) {
				return empty, resourceRefusal("resource_lease_ended")
			}
			return s.finishResourceStart(ctx, d, i, room, *source)
		}
	}
	for i := range d.State.Leases {
		l := &d.State.Leases[i]
		if !l.active() {
			continue
		}
		if l.ResourceID == source.ID && l.RoomID != room.ID {
			return empty, resourceRefusal("resource_in_use")
		}
		if l.RoomID == room.ID {
			if req.Action != "switch" {
				return empty, resourceRefusal("presenter_in_use")
			}
			if err := s.revokeResource(ctx, d, i, room); err != nil {
				return empty, err
			}
		}
	}
	all, err := s.store.resourceStates(ctx)
	if err != nil {
		return empty, err
	}
	active := 0
	for _, row := range all {
		for _, l := range row.State.Leases {
			if l.active() {
				active++
			}
		}
	}
	if active >= callrules.MaxActiveResources {
		return empty, resourceRefusal("resource_capacity")
	}
	// A source is an ordinary metered participant commitment. It may not jump
	// waiting people, exceed the prepaid room size or exceed the media seats.
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil {
		return empty, err
	}
	if server == nil || server.State != "active" {
		return empty, resourceRefusal("resource_capacity")
	}
	policy, err := s.store.PolicyFor(ctx, h.site.OwnerUserID, h.site.SiteID)
	if err != nil {
		return empty, err
	}
	policy = fundedPolicy(policy, room.roomFunding)
	load, err := s.store.loadOn(ctx, *server)
	if err != nil {
		return empty, err
	}
	ttls, err := s.queueLifetimes(ctx, h.site, policy, load)
	if err != nil {
		return empty, err
	}
	waiting, _, _ := load.queue(now, ttls)
	// Funded roomState folds all machine occupants into its prepaid people cap,
	// exactly as ordinary delegated joins do. The stored seat remains KindAgent.
	decision := callrules.DecideJoin(load.roomState(room), callrules.JoinAsk{Kind: callrules.KindPerson, Host: true}, policy, load.free()-len(waiting))
	if decision.Outcome != callrules.OutcomeAdmit {
		return empty, resourceRefusal("resource_capacity")
	}
	if _, err = s.resourceMediaFor(ctx, room); err != nil {
		return empty, err
	}
	// Compact only completed receipts. Active/ambiguous effects always retain
	// their full reservation until the ingress deletion is confirmed.
	if len(d.State.Leases) >= callrules.MaxResourceLeases {
		for i, l := range d.State.Leases {
			if !l.active() {
				d.State.Leases = append(d.State.Leases[:i], d.State.Leases[i+1:]...)
				break
			}
		}
	}
	if len(d.State.Leases) >= callrules.MaxResourceLeases {
		return empty, resourceRefusal("resource_limit")
	}
	identity, err := callrules.NewToken(s.random, identityBytes)
	if err != nil {
		return empty, err
	}
	expires := now.Add(callrules.ResourceLeaseLifetime)
	if !room.FundingDeadline.IsZero() && room.FundingDeadline.Before(expires) {
		expires = room.FundingDeadline
	}
	l := resourceLease{ID: id.NewShortId(), ResourceID: source.ID, RoomID: room.ID, SeatID: id.NewShortId(), Identity: identity, RequestID: req.RequestID, Digest: digest, CreatedAt: now, ExpiresAt: expires, Status: "preparing"}
	if ctx.Value(agentCallContext{}) == nil {
		source.HumanHold = false
		source.LastReason = ""
	}
	if grant, ok := ctx.Value(agentCallContext{}).(string); ok {
		l.GrantID = grant
	}
	d.State.Leases = append(d.State.Leases, l)
	if err = s.store.saveResourceState(ctx, d, now); err != nil {
		return empty, err
	}
	if err = s.store.createSeat(ctx, h.site.OwnerUserID, newSeat{ID: l.SeatID, SiteID: h.site.SiteID, RoomID: room.ID, Identity: identity, Kind: string(callrules.KindAgent), DisplayName: source.Label, Status: seatAdmitted, KeyID: h.row.KeyID, CredentialID: h.row.CredentialID, JoinedAt: now}, now); err != nil {
		return empty, err
	}
	return s.finishResourceStart(ctx, d, len(d.State.Leases)-1, room, *source)
}

func (s *Service) finishResourceStart(ctx context.Context, d *resourceRow, index int, room roomRow, source callrules.ResourceInfo) (callrules.ResourceResult, error) {
	var empty callrules.ResourceResult
	l := &d.State.Leases[index]
	seat, err := s.store.seatByID(ctx, d.State.OwnerUserID, l.SeatID)
	if err != nil {
		return empty, err
	}
	if seat == nil || !seat.occupying() {
		return empty, resourceRefusal("resource_lease_ended")
	}
	backend, err := s.resourceMediaFor(ctx, room)
	if err != nil {
		return empty, err
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ingress, err := backend.FindResourceIngress(bounded, room.MediaRoom, l.ID)
	if err != nil {
		return empty, err
	}
	if ingress == nil {
		if l.Status != "preparing" {
			return empty, resourceRefusal("resource_lease_ended")
		}
		ingress, err = backend.CreateResourceIngress(bounded, room.MediaRoom, l.ID, l.Identity, source.ResourceSpec)
		if err != nil {
			return empty, err
		}
	}
	if ingress == nil || ingress.Identity != l.Identity {
		return empty, resourceRefusal("resource_gateway_unavailable")
	}
	l.IngressID = ingress.ID
	if l.Status == "preparing" {
		l.Status = "awaiting_source"
	}
	if err = s.store.saveResourceState(ctx, d, s.now()); err != nil {
		return empty, err
	}
	out := resourceAnswer(d)
	out.Publication = &callrules.ResourcePublication{ResourceID: source.ID, LeaseID: l.ID, RoomID: room.ID, Identity: l.Identity, Protocol: "whip", URL: ingress.URL, StreamKey: ingress.StreamKey, ExpiresAt: l.ExpiresAt}
	return out, nil
}

// revokeResource commits refusal before contacting the provider and retains its
// commitment on every uncertain result. A stop never relies on JWT expiry.
func (s *Service) revokeResource(ctx context.Context, d *resourceRow, index int, room roomRow) error {
	l := &d.State.Leases[index]
	if !l.active() {
		return nil
	}
	l.Status = "revoking"
	if err := s.store.saveResourceState(ctx, d, s.now()); err != nil {
		return err
	}
	backend, err := s.resourceMediaFor(ctx, room)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ingress, err := backend.FindResourceIngress(bounded, room.MediaRoom, l.ID)
	if err != nil {
		return err
	}
	if ingress != nil {
		if err = backend.DeleteResourceIngress(bounded, ingress.ID); err != nil {
			return err
		}
	}
	remaining, err := backend.FindResourceIngress(bounded, room.MediaRoom, l.ID)
	if err != nil {
		return err
	}
	if remaining != nil {
		return resourceRefusal("resource_revocation_pending")
	}
	media, err := s.backendFor(bounded, room)
	if err != nil {
		return err
	}
	if media == nil {
		return resourceRefusal("resource_gateway_unavailable")
	}
	if err = media.RemoveParticipant(bounded, room.MediaRoom, l.Identity); err != nil {
		return err
	}
	seat, err := s.store.seatByID(ctx, d.State.OwnerUserID, l.SeatID)
	if err != nil {
		return err
	}
	if seat != nil && seat.occupying() {
		if err = s.store.updateSeat(ctx, d.State.OwnerUserID, *seat, map[string]any{"status": seatLeft, "leftAt": s.now()}, memql.VersionTimeAfter(seat.Version, s.now())); err != nil {
			return err
		}
	}
	l.Status = "stopped"
	return s.store.saveResourceState(ctx, d, s.now())
}

// StopRoomResources must be called outside room/end gates before deleting its
// media room. Ingress definitions otherwise outlive a deleted LiveKit room.
func (s *Service) StopRoomResources(ctx context.Context, room roomRow) error {
	if room.KeyID == "" {
		return nil
	}
	release, err := s.gates(ctx, accountGate(room.KeyID), keyGate(room.KeyID), commerceGlobalGate, roomGate(room.ID), serverGate(room.MediaServerID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, err := s.store.resourceState(fresh, room.OwnerUserID, room.SiteID, room.KeyID)
	if err != nil {
		return err
	}
	for i, l := range d.State.Leases {
		if l.RoomID == room.ID && l.active() {
			if err = s.revokeResource(fresh, d, i, room); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReconcileResources also visits ended rooms and unknown provider outcomes. It
// must run before normal room reconciliation, on the same periodic automation.
func (s *Service) ReconcileResources(ctx context.Context) error {
	if s.lifecycle == nil {
		return fmt.Errorf("calls: resource scanner is unavailable")
	}
	first := s.lifecycle.resources.run(ctx, s.store, "callsResourceStates", 200, func(row map[string]any) error {
		snapshot, err := resourceFromRow(row)
		if err != nil {
			return err
		}
		return s.reconcileResourceLeases(ctx, snapshot)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	// Legacy metadata is normalized in a separate bounded queue after current
	// lease cleanup. Its independent cursor also passes unrepairable old rows;
	// neither a full migration queue nor one broken state may block revocation.
	err := s.lifecycle.legacyResources.run(ctx, s.store, "callsResourceLegacyStates", 100, func(row map[string]any) error {
		snapshot, err := resourceFromRow(row)
		if err != nil {
			return err
		}
		if err = s.projectLegacyResources(ctx, snapshot); err != nil {
			return err
		}
		return s.reconcileResourceLeases(ctx, snapshot)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if first == nil {
		first = err
	}
	return first
}

func (s *Service) reconcileResourceLeases(ctx context.Context, snapshot *resourceRow) error {
	var first error
	for _, prior := range snapshot.State.Leases {
		if err := ctx.Err(); err != nil {
			return err
		}
		if prior.active() {
			if err := s.reconcileResource(ctx, snapshot, prior); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *Service) projectLegacyResources(ctx context.Context, snapshot *resourceRow) error {
	release, err := s.gates(ctx, accountGate(snapshot.KeyID), keyGate(snapshot.KeyID), commerceGlobalGate)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.store.resourceState(fresh, snapshot.State.OwnerUserID, snapshot.SiteID, snapshot.KeyID)
	if err != nil {
		return err
	}
	if current.ProjectionKnown {
		return nil
	}
	return s.store.saveResourceState(fresh, current, s.now())
}
func (s *Service) reconcileResource(ctx context.Context, snapshot *resourceRow, prior resourceLease) error {
	room, err := s.store.roomByID(ctx, snapshot.State.OwnerUserID, prior.RoomID)
	if err != nil {
		return err
	}
	if room == nil {
		return resourceRefusal("resource_room_unavailable")
	}
	release, err := s.gates(ctx, accountGate(snapshot.KeyID), keyGate(snapshot.KeyID), commerceGlobalGate, roomGate(room.ID), serverGate(room.MediaServerID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, err := s.store.resourceState(fresh, snapshot.State.OwnerUserID, snapshot.SiteID, snapshot.KeyID)
	if err != nil {
		return err
	}
	room, err = s.store.roomByID(fresh, snapshot.State.OwnerUserID, prior.RoomID)
	if err != nil {
		return err
	}
	if room == nil {
		return resourceRefusal("resource_room_unavailable")
	}
	for i, l := range d.State.Leases {
		if l.ID != prior.ID || !l.active() {
			continue
		}
		now := s.now()
		stop := !room.open() || !now.Before(l.ExpiresAt) || l.Status == "revoking" || (l.Status == "preparing" && now.Sub(l.CreatedAt) > time.Minute)
		if source := resourceSource(d, l.ResourceID); source == nil || !callrules.ValidResourceSpec(source.ResourceSpec) {
			stop = true
			d.State.Leases[i].StoppedReason = "unsupported_track_isolation"
		}
		seat, e := s.store.seatByID(fresh, d.State.OwnerUserID, l.SeatID)
		if e != nil {
			return e
		}
		if seat == nil || !seat.occupying() {
			stop = true
		}
		if seat != nil && seat.CredentialID != "" {
			credential, e := s.store.credentialByID(fresh, d.State.OwnerUserID, seat.CredentialID)
			if e != nil {
				return e
			}
			if credential == nil || !credential.RevokedAt.IsZero() {
				stop = true
			}
		}
		if l.GrantID != "" {
			grant, e := s.store.delegation(fresh, d.State.OwnerUserID, l.GrantID)
			if e != nil {
				return e
			}
			if grant == nil || !grant.State.RevokedAt.IsZero() || !now.Before(grant.State.Request.ExpiresAt) || !resourceGrantAllows(grant, l.ResourceID, *room) {
				stop = true
			}
		}
		if fundingRoomAllowed(*room, now) != nil {
			stop = true
		}
		if stop {
			return s.revokeResource(fresh, d, i, *room)
		}
		if l.Status == "awaiting_source" || l.Status == "publishing" {
			backend, e := s.backendFor(fresh, *room)
			if e != nil {
				return e
			}
			if backend == nil {
				return resourceRefusal("resource_gateway_unavailable")
			}
			bounded, cancel := context.WithTimeout(fresh, 5*time.Second)
			present, e := backend.Participants(bounded, room.MediaRoom)
			cancel()
			if e != nil {
				return e
			}
			status := "awaiting_source"
			for _, participant := range present {
				if participant.Identity == l.Identity {
					for _, track := range participant.Tracks {
						if !track.Muted {
							status = "publishing"
							break
						}
					}
				}
			}
			// A human's later screen-share ownership is a durable override of an
			// automated source, not only a mute that its next start may undo.
			if room.ShareSeatID != "" && room.ShareSeatID != l.SeatID {
				presenter, e := s.store.seatByID(fresh, d.State.OwnerUserID, room.ShareSeatID)
				if e != nil {
					return e
				}
				if presenter != nil && presenter.occupying() {
					for _, participant := range present {
						if participant.Identity != presenter.Identity {
							continue
						}
						for _, track := range participant.Tracks {
							if !track.Muted && track.Source == "screen_share" {
								d.State.Leases[i].StoppedReason = "presenter_handoff"
								if presenter.Kind == string(callrules.KindPerson) {
									if source := resourceSource(d, l.ResourceID); source != nil {
										source.HumanHold = true
									}
								}
								return s.revokeResource(fresh, d, i, *room)
							}
						}
					}
				}
			}
			if l.Status != status {
				d.State.Leases[i].Status = status
				return s.store.saveResourceState(fresh, d, now)
			}
		}
	}
	return nil
}

func resourceGrantAllows(grant *delegationRow, resource string, room roomRow) bool {
	if !slices.Contains(grant.State.Request.Scopes, "resources_publish") || !slices.Contains(grant.State.Request.ResourceIDs, resource) || grant.KeyID != room.KeyID || grant.SiteID != room.SiteID {
		return false
	}
	if len(grant.State.Request.RoomIDs) > 0 {
		return slices.Contains(grant.State.Request.RoomIDs, room.ID)
	}
	return delegationOwnsSource(grant, "room", room.ID) || delegationOwnsSource(grant, "booking", room.ScheduledCallID)
}

// stopHostedResource is the moderator-only boundary: a co-host may revoke a
// resource in their current room without reading its account registry or
// acquiring the owner's right to publish a replacement. Called before ordinary
// track muting and outside all room gates.
func (s *Service) stopHostedResource(ctx context.Context, sess callrules.Session, h acting, target string) (bool, error) {
	if h.room.KeyID == "" {
		return false, nil
	}
	keys := []string{h.room.KeyID}
	if h.seat.KeyID != "" && h.seat.KeyID != h.room.KeyID {
		keys = append(keys, h.seat.KeyID)
	}
	sort.Strings(keys)
	locks := []string{}
	for _, key := range keys {
		locks = append(locks, accountGate(key))
	}
	for _, key := range keys {
		locks = append(locks, keyGate(key))
	}
	locks = append(locks, commerceGlobalGate, roomGate(h.room.ID), serverGate(h.room.MediaServerID))
	release, err := s.gates(ctx, locks...)
	if err != nil {
		return false, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.hostOf(fresh, sess)
	if err != nil {
		return false, err
	}
	if current.room.ID != h.room.ID {
		return false, notHost()
	}
	room, err := s.stillHost(fresh, current)
	if err != nil {
		return false, err
	}
	if _, err = s.targetIn(fresh, current, target); err != nil {
		return false, err
	}
	if err = s.delegatedEffectAllowed(fresh, current.site, current.seat.KeyID); err != nil {
		return false, err
	}
	d, err := s.store.resourceState(fresh, room.OwnerUserID, room.SiteID, room.KeyID)
	if err != nil {
		return false, err
	}
	for i, l := range d.State.Leases {
		if l.RoomID != room.ID || l.SeatID != target {
			continue
		}
		if !l.active() {
			return true, nil
		}
		if ctx.Value(agentCallContext{}) == nil {
			if source := resourceSource(d, l.ResourceID); source != nil {
				source.HumanHold = true
			}
		}
		d.State.Leases[i].StoppedReason = "host_stopped"
		return true, s.revokeResource(fresh, d, i, room)
	}
	return false, nil
}
func (s *Service) resourceDelegationAllowed(ctx context.Context, h keyHolder, req callrules.ResourceRequest, room roomRow) error {
	grant, ok := ctx.Value(agentCallContext{}).(string)
	if !ok {
		return nil
	}
	d, err := s.store.delegation(ctx, h.site.OwnerUserID, grant)
	if err != nil {
		return err
	}
	if d == nil || d.KeyID != h.row.KeyID || d.SiteID != h.site.SiteID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
		return accountRefusal("delegation_inactive")
	}
	if !slices.Contains(d.State.Request.Scopes, callrules.ResourceScope(req.Action)) {
		return accountRefusal("delegation_scope")
	}
	if req.Action != "list" && req.Action != "register" && !slices.Contains(d.State.Request.ResourceIDs, req.ResourceID) {
		return accountRefusal("delegation_resource_scope")
	}
	if (req.Action == "start" || req.Action == "switch") && !resourceGrantAllows(d, req.ResourceID, room) {
		return accountRefusal("delegation_resource_scope")
	}
	return nil
}
