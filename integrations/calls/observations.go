package calls

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type observationPublisher interface {
	PublishObservation(context.Context, string, []byte, []string) error
}

func observationAvailable(r roomRow, now time.Time) bool {
	c, e := decodeUsageConfig(r.UsageConfig)
	return e == nil && c.Insights && !familyRoom(r) && r.open() && fundingRoomAllowed(r, now) == nil && now.Before(r.OpenedAt.Add(24*time.Hour))
}
func observationSameAdmission(p observationParty, seat seatRow) bool {
	return seat.occupying() && p.SeatID == seat.ID && p.Identity == seat.Identity && p.KeyID == seat.KeyID && p.CredentialID == seat.CredentialID && p.JoinedAt.Equal(seat.JoinedAt) && p.Kind == seat.Kind
}
func observationActor(p observationParty, session sessionRow) string {
	return callrules.HistoryDigest(nil, "observation-actor", p.SeatID, p.Identity, versionStamp(p.JoinedAt), session.ID, p.GrantID, p.ApprovalDigest)
}
func (s *Service) observationParty(ctx context.Context, room roomRow, seat seatRow) (observationParty, error) {
	p := observationParty{ObservationPerson: callrules.ObservationPerson{Identity: seat.Identity, Name: seat.DisplayName, Kind: seat.Kind}, SeatID: seat.ID, KeyID: seat.KeyID, CredentialID: seat.CredentialID, JoinedAt: seat.JoinedAt}
	if seat.Kind != "person" {
		var binding messageAgentBinding
		if len(seat.AgentData) > 4096 || json.Unmarshal([]byte(seat.AgentData), &binding) != nil || binding.GrantID == "" || !callrules.QuestionDigest(binding.ApprovalDigest) {
			return p, accountRefusal("observations_admission_changed")
		}
		p.GrantID, p.ApprovalDigest = binding.GrantID, binding.ApprovalDigest
		if admission := seatAgentAdmissionID(seat.AgentData); admission != "" {
			j, e := s.store.agentAdmissions(ctx, room.OwnerUserID, room.ID)
			if e != nil {
				return p, e
			}
			a := j.find(admission)
			if a == nil || a.GrantID != p.GrantID || a.ApprovalDigest != p.ApprovalDigest {
				return p, accountRefusal("observations_admission_changed")
			}
			p.ReviewerKeyID = a.ReviewerKeyID
		}
	}
	return p, nil
}
func observationGates(room string, parties []observationParty) []string {
	keys := []string{roomGate(room)}
	for _, p := range parties {
		if p.KeyID != "" {
			keys = append(keys, accountGate(p.KeyID), keyGate(p.KeyID))
		}
		if p.GrantID != "" {
			keys = append(keys, delegationGate(p.GrantID))
		}
		if p.ReviewerKeyID != "" {
			keys = append(keys, accountGate(p.ReviewerKeyID), keyGate(p.ReviewerKeyID))
		}
	}
	return keys
}

type observationAuthority struct {
	service  *Service
	ctx      context.Context
	room     roomRow
	seats    map[string]seatRow
	eligible map[string]bool
	checked  map[string]bool
}

func (s *Service) observationAuthority(ctx context.Context, room roomRow) (*observationAuthority, error) {
	seats, e := s.store.seatsForRoom(ctx, room.OwnerUserID, room.ID)
	if e != nil {
		return nil, e
	}
	allowed, e := s.messageRecipients(ctx, room, seats)
	if e != nil {
		return nil, e
	}
	a := &observationAuthority{service: s, ctx: ctx, room: room, seats: map[string]seatRow{}, eligible: map[string]bool{}, checked: map[string]bool{}}
	for _, seat := range seats {
		if seat.occupying() {
			a.seats[seat.Identity] = seat
		}
	}
	for _, identity := range allowed {
		a.eligible[identity] = true
	}
	return a, nil
}
func (a *observationAuthority) allowed(p observationParty) bool {
	identity, _ := json.Marshal(p)
	key := string(identity)
	if allowed, ok := a.checked[key]; ok {
		return allowed
	}
	a.checked[key] = false
	seat, ok := a.seats[p.Identity]
	if !ok || !a.eligible[p.Identity] || !observationSameAdmission(p, seat) {
		return false
	}
	s := a.service
	ctx := memql.ContextWithFreshRead(workflowEffectContext{a.ctx})
	if p.SessionID != "" {
		held, e := s.store.sessionByID(ctx, a.room.OwnerUserID, p.SessionID)
		if e != nil || !held.usable(s.now()) || held.SiteID != a.room.SiteID || held.RoomID != a.room.ID || held.SeatID != p.SeatID || held.KeyID != p.KeyID || held.CredentialID != p.CredentialID {
			return false
		}
	}
	if p.GrantID != "" {
		var binding messageAgentBinding
		if len(seat.AgentData) > 4096 || json.Unmarshal([]byte(seat.AgentData), &binding) != nil || binding.GrantID != p.GrantID || binding.ApprovalDigest != p.ApprovalDigest {
			return false
		}
		d, e := s.store.delegation(ctx, a.room.OwnerUserID, p.GrantID)
		if e != nil || d == nil || d.SiteID != a.room.SiteID || d.KeyID != p.KeyID || delegationApprovalDigest(d) != p.ApprovalDigest || !slices.Contains(d.State.Request.Scopes, "observations") || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
			return false
		}
		if len(d.State.Request.RoomIDs) > 0 {
			if !slices.Contains(d.State.Request.RoomIDs, a.room.ID) {
				return false
			}
		} else if !delegationOwnsSource(d, "room", a.room.ID) && !delegationOwnsSource(d, "booking", a.room.ScheduledCallID) {
			return false
		}
		ctx = context.WithValue(ctx, agentCallContext{}, p.GrantID)
		ctx = context.WithValue(ctx, agentApprovalContext{}, p.ApprovalDigest)
		if admission := seatAgentAdmissionID(seat.AgentData); admission != "" {
			j, e := s.store.agentAdmissions(ctx, a.room.OwnerUserID, a.room.ID)
			if e != nil {
				return false
			}
			v := j.find(admission)
			if v == nil || v.ReviewerKeyID != p.ReviewerKeyID || v.GrantID != p.GrantID || v.ApprovalDigest != p.ApprovalDigest {
				return false
			}
			ctx = context.WithValue(ctx, guestAgentContext{}, guestAgentContext{RoomID: a.room.ID, RequestID: v.ID, GrantID: v.GrantID, ApprovalDigest: v.ApprovalDigest, AdmissionKey: v.AdmissionKey})
		}
		if s.commerceAdultAllowed(ctx, callrules.SiteRef{SiteID: a.room.SiteID, OwnerUserID: a.room.OwnerUserID}, p.KeyID) != nil || s.agentSeatAllowed(ctx, a.room, seat) != nil {
			return false
		}
	}
	a.checked[key] = true
	return true
}
func (a *observationAuthority) permissionAllowed(p *observationPermission) bool {
	if !a.allowed(p.Observer) || !a.allowed(p.Subject) {
		return false
	}
	for _, recipient := range p.Recipients {
		if !a.allowed(recipient) {
			return false
		}
	}
	return true
}
func observationFind(j *observationJournal, id string) *observationPermission {
	for i := range j.Permissions {
		if j.Permissions[i].ID == id {
			return &j.Permissions[i]
		}
	}
	return nil
}
func observationVisible(p *observationPermission, actor observationParty) bool {
	for _, r := range p.Recipients {
		if r.Identity == actor.Identity && r.SeatID == actor.SeatID && r.JoinedAt.Equal(actor.JoinedAt) && r.KeyID == actor.KeyID && r.CredentialID == actor.CredentialID && r.GrantID == actor.GrantID && r.ApprovalDigest == actor.ApprovalDigest && (r.SessionID == "" || r.SessionID == actor.SessionID) {
			return true
		}
	}
	return false
}
func observationProjection(j *observationJournal, actor observationParty, a *observationAuthority, now time.Time) *callrules.Observations {
	available := observationAvailable(a.room, now)
	out := &callrules.Observations{Available: available, Protocol: callrules.ObservationProtocol, Notice: callrules.ObservationNotice, Participants: []callrules.ObservationPerson{}, Permissions: []callrules.ObservationPermission{}}
	if !available {
		out.Reason = "observations_insights_required"
	}
	// Only public participant identity is offered, never a Callkey, grant or session.
	if available {
		for _, seat := range a.seats {
			if a.eligible[seat.Identity] {
				party, err := a.service.observationParty(a.ctx, a.room, seat)
				if err != nil || !a.allowed(party) {
					continue
				}
				out.Participants = append(out.Participants, callrules.ObservationPerson{Identity: seat.Identity, Name: seat.DisplayName, Kind: seat.Kind})
			}
		}
	}
	slices.SortFunc(out.Participants, func(a, b callrules.ObservationPerson) int {
		if a.Identity < b.Identity {
			return -1
		}
		if a.Identity > b.Identity {
			return 1
		}
		return 0
	})
	for i := range j.Permissions {
		p := &j.Permissions[i]
		status := p.Status
		current := available && now.Before(p.ExpiresAt) && a.permissionAllowed(p)
		if status != "withdrawn" && !current {
			status = "expired"
		}
		if status == "approved" {
			out.ActiveCount++
		}
		if !observationVisible(p, actor) {
			continue
		}
		recipients := []callrules.ObservationPerson{}
		for _, person := range p.Recipients {
			recipients = append(recipients, person.ObservationPerson)
		}
		out.Permissions = append(out.Permissions, callrules.ObservationPermission{ID: p.ID, Revision: p.Revision, Status: status, Purpose: p.Purpose, Observer: p.Observer.ObservationPerson, Subject: p.Subject.ObservationPerson, Recipients: recipients, ExpiresAt: p.ExpiresAt, PublishedCount: p.PublishedCount, CanConsent: current && status == "pending" && actor.Identity == p.Subject.Identity, CanWithdraw: p.Status != "withdrawn" && (actor.Identity == p.Subject.Identity || actor.Identity == p.Observer.Identity), CanPublish: current && status == "approved" && actor.Identity == p.Observer.Identity && p.PublishedCount < 20})
	}
	return out
}

// All authority gates precede the room gate through the shared sorted gate
// acquisition. Consent withdrawal, departure and credential/grant revocation
// cannot complete between final authorization and the bounded server data send.
func (s *Service) workflowObservations(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	if !callrules.ValidObservationRequest(req) {
		return out, callrules.ErrInvalid
	}
	q := req.Observation
	held, pepper, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return out, e
	}
	seat, room, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
	if e != nil {
		return out, e
	}
	if seat == nil || room == nil || room.ID != q.RoomID || seat.Identity != q.Identity || !seat.occupying() || !room.open() {
		return out, accountRefusal("observations_admission_changed")
	}
	if familyRoom(*room) {
		return out, accountRefusal("observations_family_unavailable")
	}
	actor, e := s.observationParty(ctx, *room, *seat)
	if e != nil {
		return out, e
	}
	actor.SessionID = held.ID
	if actor.GrantID != agentContextGrant(ctx) || actor.ApprovalDigest != agentContextApproval(ctx) {
		return out, accountRefusal("observations_admission_changed")
	}
	j, e := s.store.observationState(ctx, site.OwnerUserID, site.SiteID, room.ID)
	if e != nil {
		return out, e
	}
	parties := []observationParty{actor}
	if req.Action == "observation_request" {
		seats, e := s.store.seatsForRoom(ctx, site.OwnerUserID, room.ID)
		if e != nil {
			return out, e
		}
		for _, identity := range q.RecipientIdentities {
			found := false
			for _, member := range seats {
				if member.Identity == identity && member.occupying() {
					p, e := s.observationParty(ctx, *room, member)
					if e != nil {
						return out, e
					}
					parties = append(parties, p)
					found = true
					break
				}
			}
			if !found {
				return out, accountRefusal("observations_admission_changed")
			}
		}
	} else if p := observationFind(j, q.ID); p != nil {
		parties = append(parties, p.Observer, p.Subject)
		parties = append(parties, p.Recipients...)
	}
	release, e := s.gates(ctx, observationGates(room.ID, parties)...)
	if e != nil {
		return out, e
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	current, _, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return out, e
	}
	if current.ID != held.ID || current.RoomID != room.ID || current.SeatID != seat.ID || current.CredentialID != held.CredentialID {
		return out, accountRefusal("observations_admission_changed")
	}
	room, e = s.store.roomByID(ctx, site.OwnerUserID, room.ID)
	if e != nil {
		return out, e
	}
	if room == nil || room.SiteID != site.SiteID || !room.open() || familyRoom(*room) {
		return out, accountRefusal("observations_admission_changed")
	}
	a, e := s.observationAuthority(ctx, *room)
	if e != nil {
		return out, e
	}
	if !a.allowed(actor) {
		return out, accountRefusal("observations_admission_changed")
	}
	j, e = s.store.observationState(ctx, site.OwnerUserID, site.SiteID, room.ID)
	if e != nil {
		return out, e
	}
	now := s.now()
	if req.Action == "observations" {
		out.Observations = observationProjection(j, actor, a, now)
		return out, nil
	}
	actorID := observationActor(actor, *held)
	raw, _ := json.Marshal(req)
	digest := callrules.HistoryDigest(pepper, "observation-request", site.SiteID, room.ID, actorID, string(raw))
	for i := range j.Permissions {
		for _, op := range j.Permissions[i].Operations {
			if op.Actor == actorID && op.Key == q.IdempotencyKey {
				if op.Digest != digest {
					return out, accountRefusal("observations_idempotency_conflict")
				}
				if !observationVisible(&j.Permissions[i], actor) {
					return out, accountRefusal("observations_admission_changed")
				}
				receipt := op.Receipt
				if receipt.EventID != "" {
					receipt.Status = "duplicate"
				}
				out.ObservationReceipt = &receipt
				out.Observations = observationProjection(j, actor, a, now)
				return out, nil
			}
		}
	}
	p := observationFind(j, q.ID)
	if req.Action != "observation_request" && (p == nil || !observationVisible(p, actor)) {
		return out, accountRefusal("observations_not_found")
	}
	if req.Action != "observation_withdraw" && !observationAvailable(*room, now) {
		return out, accountRefusal("observations_insights_required")
	}
	if p != nil && p.Revision != q.Revision {
		return out, accountRefusal("observations_revision_conflict")
	}
	if req.Action == "observation_request" {
		if len(j.Permissions) >= observationMaxPermissions {
			return out, accountRefusal("observations_limit")
		}
		open := 0
		for _, prior := range j.Permissions {
			if prior.Observer.KeyID == actor.KeyID {
				if now.Sub(prior.CreatedAt) < 10*time.Second {
					return out, accountRefusal("observations_rate_limited")
				}
				if prior.Status != "withdrawn" && now.Before(prior.ExpiresAt) {
					open++
				}
			}
		}
		if open >= 4 {
			return out, accountRefusal("observations_limit")
		}
		if actor.GrantID != "" {
			d, e := s.store.delegation(ctx, site.OwnerUserID, actor.GrantID)
			if e != nil {
				return out, e
			}
			if d == nil || d.State.Request.Purpose != q.Purpose {
				return out, accountRefusal("observations_purpose_changed")
			}
		}
		expires := minTime(now.Add(time.Duration(q.DurationSeconds)*time.Second), room.FundingDeadline)
		if !expires.After(now) {
			return out, accountRefusal("observations_expired")
		}
		permission := observationPermission{ID: callrules.HistoryDigest(pepper, "observation-permission", room.ID, actorID, q.IdempotencyKey), Purpose: q.Purpose, Status: "pending", Revision: 1, Observer: actor, CreatedAt: now, ExpiresAt: expires, Recipients: []observationParty{}, Operations: []observationOperation{}}
		for _, person := range parties[1:] {
			if !a.allowed(person) {
				return out, accountRefusal("observations_admission_changed")
			}
			if person.Identity == actor.Identity {
				person = actor
			}
			if person.Identity == q.SubjectIdentity {
				permission.Subject = person
			}
			permission.Recipients = append(permission.Recipients, person)
		}
		j.Permissions = append(j.Permissions, permission)
		p = &j.Permissions[len(j.Permissions)-1]
	} else if req.Action == "observation_withdraw" {
		if actor.Identity != p.Subject.Identity && actor.Identity != p.Observer.Identity {
			return out, accountRefusal("observations_subject_required")
		}
		if p.Status == "withdrawn" {
			return out, accountRefusal("observations_transition")
		}
		p.Status = "withdrawn"
		p.Revision++
	} else {
		if !now.Before(p.ExpiresAt) || !a.permissionAllowed(p) {
			return out, accountRefusal("observations_expired")
		}
		if req.Action == "observation_consent" {
			if actor.Identity != p.Subject.Identity {
				return out, accountRefusal("observations_subject_required")
			}
			if p.Status != "pending" {
				return out, accountRefusal("observations_transition")
			}
			p.Subject.SessionID = held.ID
			for i := range p.Recipients {
				if p.Recipients[i].Identity == actor.Identity {
					p.Recipients[i].SessionID = held.ID
				}
			}
			p.Status = "approved"
			p.Revision++
		} else {
			if p.Status != "approved" || actor.Identity != p.Observer.Identity {
				return out, accountRefusal("observations_consent_required")
			}
			if p.PublishedCount >= 20 {
				return out, accountRefusal("observations_limit")
			}
			for _, prior := range j.Permissions {
				if prior.Observer.KeyID == actor.KeyID && !prior.PublishedAt.IsZero() && now.Sub(prior.PublishedAt) < 10*time.Second {
					return out, accountRefusal("observations_rate_limited")
				}
			}
		}
	}
	now = questionStamp(now, p.CreatedAt)
	for _, op := range p.Operations {
		now = questionStamp(now, op.At)
	}
	receipt := callrules.ObservationReceipt{ID: p.ID, Revision: p.Revision, Status: p.Status}
	var publisher observationPublisher
	var payload []byte
	recipients := []string{}
	if req.Action == "observation_publish" {
		backend, e := s.backendFor(ctx, *room)
		if e != nil {
			return out, e
		}
		var ok bool
		publisher, ok = backend.(observationPublisher)
		if !ok {
			return out, accountRefusal("observations_unavailable")
		}
		receipt.EventID = callrules.HistoryDigest(pepper, "observation-event", p.ID, actorID, q.IdempotencyKey)
		receipt.Status = "attempted"
		for _, person := range p.Recipients {
			recipients = append(recipients, person.Identity)
		}
		packet := callrules.ObservationPacket{Protocol: callrules.ObservationProtocol, EventID: receipt.EventID, RoomID: room.ID, PermissionID: p.ID, Revision: p.Revision, Observer: p.Observer.ObservationPerson, Subject: p.Subject.ObservationPerson, Purpose: p.Purpose, Text: q.Text, PublishedAt: s.now().Unix(), ExpiresAt: minTime(s.now().Add(30*time.Second), p.ExpiresAt).Unix()}
		payload, _ = json.Marshal(packet)
		for _, budget := range []struct{ kind, id string }{{"server", room.MediaServerID}, {"room", room.ID}} {
			delay, e := s.store.takeMessageRate(ctx, site.OwnerUserID, budget.kind, budget.id, int64(len(payload)*len(recipients)), s.now())
			if e != nil {
				return out, e
			}
			if delay > 0 {
				return out, accountRefusal("observations_rate_limited")
			}
		}
		p.PublishedCount++
		p.PublishedAt = now
	}
	p.Operations = append(p.Operations, observationOperation{Key: q.IdempotencyKey, Digest: digest, Actor: actorID, Action: req.Action, Receipt: receipt, At: now})
	if e = s.store.saveObservation(ctx, site.OwnerUserID, j, now); e != nil {
		return out, e
	}
	if publisher != nil {
		// Persist the attempt BEFORE sending. A timeout never becomes an automatic
		// second send. An identical retry returns duplicate even after withdrawal.
		latest, checkErr := s.observationAuthority(ctx, *room)
		if latest != nil {
			a = latest
		}
		if checkErr != nil || !s.now().Before(p.ExpiresAt) || !observationAvailable(*room, s.now()) || !a.permissionAllowed(p) {
			receipt.Status = "delivery_uncertain"
		} else {
			remaining := p.ExpiresAt.Sub(s.now())
			if remaining > 2*time.Second {
				remaining = 2 * time.Second
			}
			sendCtx, cancel := context.WithTimeout(ctx, remaining)
			e = publisher.PublishObservation(sendCtx, room.MediaRoom, payload, recipients)
			cancel()
			receipt.Status = "published"
			if e != nil {
				receipt.Status = "delivery_uncertain"
			}
		}
	}
	out.ObservationReceipt = &receipt
	out.Observations = observationProjection(j, actor, a, s.now())
	return out, nil
}
