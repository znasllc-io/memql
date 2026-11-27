package calls

import (
	"context"
	"maps"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func (s *Service) workflowPresentation(ctx context.Context, site callrules.SiteRef, token string, req callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	out := callrules.WorkflowResult{ServerNow: s.now()}
	if !callrules.ValidPresentationRequest(req) {
		return out, callrules.ErrInvalid
	}
	held, _, e := s.session(ctx, site, token, s.now())
	if e != nil {
		return out, e
	}
	seat, hint, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
	if e != nil {
		return out, e
	}
	if hint == nil || seat == nil || !seat.occupying() {
		return out, notHost()
	}
	// Any response may retry an already-authorized pending provider effect. It
	// cannot invent a new policy, start a device or transfer resource ownership.
	if e = s.reconcilePresentation(ctx, *hint); e != nil {
		return out, e
	}
	e = s.mediaSignalGated(ctx, *hint, mediaSignalBinding{}, func(ctx context.Context, room roomRow) error {
		held, _, e := s.session(ctx, site, token, s.now())
		if e != nil {
			return e
		}
		seat, current, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
		if e != nil {
			return e
		}
		if seat == nil || current == nil || current.ID != room.ID || !seat.occupying() {
			return notHost()
		}
		if e = s.workflowCurrentActor(ctx, site, token, *held, *seat, room, req.Action); e != nil {
			return e
		}
		v, e := s.store.workflowState(ctx, site.OwnerUserID, workflowStateID(site.SiteID, "run", room.ID))
		if e != nil {
			return e
		}
		if v == nil || v.Run == nil {
			return accountRefusal("workflow_run_required")
		}
		if req.Action == "presentation_status" {
			out.Presentation = s.presentationProjection(ctx, room, v, *seat)
			return nil
		}
		if !seat.Host {
			return notHost()
		}
		if e = s.workflowAgentVersionAllowed(ctx, site, held.KeyID, room, req, v.Run); e != nil {
			return e
		}
		seen, e := workflowOperationSeen(v, req, held.ID)
		if e != nil {
			return e
		}
		if seen {
			out.Presentation = s.presentationProjection(ctx, room, v, *seat)
			return nil
		}
		if req.RunID != v.Run.ID || req.ExpectedRevision != v.Run.Revision || req.ExpectedEpoch != v.Run.ControlEpoch || req.Presentation.VisitID != v.Run.VisitID {
			return accountRefusal("workflow_revision_conflict")
		}
		if agentContextGrant(ctx) != "" && v.Run.Hold == "human" {
			return accountRefusal("workflow_human_control_hold")
		}
		revision := int64(0)
		if v.Presentation != nil {
			revision = v.Presentation.Revision
		}
		if req.Presentation.ExpectedRevision != revision {
			return accountRefusal("presentation_revision_conflict")
		}
		if req.Action == "presentation_set" {
			if e = s.workflowEntitlement(ctx, site, room); e != nil {
				return e
			}
			n, ok := callrules.WorkflowNodeByID(v.Run.Definition, v.Run.NodeID)
			if !ok || n.Type != "presentation" || workflowTerminal(v.Run) {
				return accountRefusal("presentation_step_required")
			}
			presenter, e := s.store.seatByID(ctx, room.OwnerUserID, req.Presentation.PresenterSeatID)
			if e != nil {
				return e
			}
			if presenter == nil {
				return accountRefusal("presentation_admission_changed")
			}
			bound, _, e := s.presentationAdmission(ctx, room, *presenter)
			if e != nil {
				return e
			}
			if e = s.presentationResourcesMatch(ctx, room, bound); e != nil {
				return e
			}
			p := &presentationIntent{RunID: v.Run.ID, VisitID: v.Run.VisitID, Revision: revision + 1, Active: true, Pending: true, Presenter: bound, Audience: *req.Presentation.Audience, Applied: map[string]presentationApplied{}}
			if v.Presentation != nil {
				p.Applied = maps.Clone(v.Presentation.Applied)
			}
			if req.Presentation.SpeakerSeatID != "" {
				speaker, e := s.store.seatByID(ctx, room.OwnerUserID, req.Presentation.SpeakerSeatID)
				if e != nil {
					return e
				}
				if speaker == nil {
					return accountRefusal("presentation_speaker_unavailable")
				}
				p.Speaker, _, e = s.presentationAdmission(ctx, room, *speaker)
				if e != nil {
					return e
				}
				if p.Speaker.Kind == "resource" || speaker.ID == presenter.ID {
					return accountRefusal("presentation_speaker_unavailable")
				}
				p.SpeakerExpiresAt = s.now().Add(time.Duration(req.Presentation.SpeakerSeconds) * time.Second)
			}
			v.Presentation = p
		} else {
			if v.Presentation == nil {
				v.Presentation = &presentationIntent{RunID: v.Run.ID, VisitID: v.Run.VisitID, Revision: 1, Applied: map[string]presentationApplied{}}
			} else {
				v.Presentation.Revision++
			}
			v.Presentation.Active = false
			v.Presentation.Pending = true
			v.Presentation.Reason = ""
			v.Presentation.Speaker = presentationAdmission{}
			v.Presentation.SpeakerExpiresAt = time.Time{}
		}
		v.Run.Revision++
		workflowRecord(v, req, held.ID, v.Run.Revision)
		if e = s.store.saveWorkflow(ctx, room.OwnerUserID, v, s.now()); e != nil {
			return e
		}
		out.Presentation = s.presentationProjection(ctx, room, v, *seat)
		return nil
	})
	if e != nil {
		return out, e
	}
	// A pending result is a durable desired policy; provider failures do not
	// roll it back or re-run an idempotent request under different authority.
	if req.Action == "presentation_status" {
		return out, nil
	}
	if e = s.reconcilePresentation(ctx, *hint); e != nil {
		return out, e
	}
	e = s.mediaSignalGated(ctx, *hint, mediaSignalBinding{}, func(ctx context.Context, room roomRow) error {
		held, _, e := s.session(ctx, site, token, s.now())
		if e != nil {
			return e
		}
		seat, current, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
		if e != nil {
			return e
		}
		if seat == nil || current == nil || current.ID != room.ID || !seat.occupying() {
			return notHost()
		}
		if e = s.workflowCurrentActor(ctx, site, token, *held, *seat, room, req.Action); e != nil {
			return e
		}
		v, e := s.store.workflowState(ctx, room.OwnerUserID, workflowStateID(room.SiteID, "run", room.ID))
		if e != nil {
			return e
		}
		out.Presentation = s.presentationProjection(ctx, room, v, *seat)
		return nil
	})
	return out, e
}

func (s *Service) presentationProjection(ctx context.Context, room roomRow, v *workflowState, actor seatRow) *callrules.PresentationStatus {
	if v == nil || v.Run == nil {
		return nil
	}
	out := &callrules.PresentationStatus{RunID: v.Run.ID, VisitID: v.Run.VisitID, Audience: callrules.PublicationPermissions{Microphone: true, Camera: true, ScreenShare: true}, Enforcement: "applied", ServerNow: s.now()}
	canControl := actor.Host && s.workflowAgentAllowed(ctx, callrules.SiteRef{SiteID: room.SiteID, OwnerUserID: room.OwnerUserID}, actor.KeyID, "presentation_set") == nil
	if canControl && agentContextGrant(ctx) != "" {
		canControl = s.workflowAgentVersionAllowed(ctx, callrules.SiteRef{SiteID: room.SiteID, OwnerUserID: room.OwnerUserID}, actor.KeyID, room, callrules.WorkflowRequest{Action: "presentation_set"}, v.Run) == nil
	}
	n, ok := callrules.WorkflowNodeByID(v.Run.Definition, v.Run.NodeID)
	out.CanControl = canControl && (v.Presentation != nil || ok && n.Type == "presentation" && !workflowTerminal(v.Run))
	if p := v.Presentation; p != nil {
		out.Revision = p.Revision
		out.Active = p.Active
		out.Reason = p.Reason
		if p.Pending {
			out.Enforcement = "pending"
		}
		if p.Active {
			out.PresenterSeatID = p.Presenter.SeatID
			out.PresenterIdentity = p.Presenter.Identity
			out.PresenterName = p.Presenter.Name
			out.ResourceID = p.Presenter.ResourceID
			out.Audience = p.Audience
		}
		if p.Active && p.Speaker.SeatID != "" {
			out.SpeakerSeatID = p.Speaker.SeatID
			out.SpeakerName = p.Speaker.Name
			out.SpeakerIdentity = p.Speaker.Identity
			expires := p.SpeakerExpiresAt
			out.SpeakerExpiresAt = &expires
		}
	}
	if out.CanControl {
		seats, e := s.store.seatsForRoom(ctx, room.OwnerUserID, room.ID)
		if e == nil {
			for _, seat := range seats {
				if !seat.occupying() {
					continue
				}
				p, _, e := s.presentationAdmission(ctx, room, seat)
				if e != nil {
					continue
				}
				out.Choices = append(out.Choices, callrules.PresentationChoice{SeatID: p.SeatID, Name: p.Name, Identity: p.Identity, Kind: p.Kind, ResourceID: p.ResourceID})
			}
		}
	}
	return out
}

func (s *Service) reconcilePresentation(ctx context.Context, hint roomRow) error {
	return s.mediaSignalGated(ctx, hint, mediaSignalBinding{}, func(ctx context.Context, room roomRow) error {
		v, e := s.store.workflowState(ctx, room.OwnerUserID, workflowStateID(room.SiteID, "run", room.ID))
		if e != nil {
			return e
		}
		if v == nil || v.Presentation == nil {
			return nil
		}
		p := v.Presentation
		workflowInvalidatePresentation(v, s.now())
		if p.Active {
			if active, reason := s.presentationEffective(ctx, room, v); !active {
				p.Active = false
				p.Pending = true
				p.Revision++
				p.Reason = reason
			}
		}
		if p.Speaker.SeatID != "" && !s.presentationCurrent(ctx, room, p.Speaker) {
			p.Speaker = presentationAdmission{}
			p.SpeakerExpiresAt = time.Time{}
			p.Pending = true
			p.Revision++
		}
		if p.Applied == nil {
			p.Applied = map[string]presentationApplied{}
		}
		p.Pending = true
		if e = s.store.saveWorkflow(ctx, room.OwnerUserID, v, s.now()); e != nil {
			return e
		}
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		backend, e := s.backendFor(bounded, room)
		if e != nil {
			return nil
		}
		present, e := backend.Participants(bounded, room.MediaRoom)
		if e != nil {
			p.Reason = "presentation_media_unavailable"
			return s.store.saveWorkflow(ctx, room.OwnerUserID, v, s.now())
		}
		permissions, capable := backend.(MediaPermissionBackend)
		seats, e := s.store.seatsForRoom(ctx, room.OwnerUserID, room.ID)
		if e != nil {
			return e
		}
		journal, e := s.store.mediaSignals(ctx, room)
		if e != nil {
			return e
		}
		pending := false
		if p.Active && s.presentationResourcesMatch(ctx, room, p.Presenter) != nil {
			pending = true
			p.Reason = "presentation_stop_other_resources"
		}
		seen := map[string]bool{}
		for _, participant := range present {
			var seat *seatRow
			for i := range seats {
				if seats[i].Identity == participant.Identity && seats[i].occupying() {
					seat = &seats[i]
					break
				}
			}
			if seat == nil {
				continue
			}
			_, base, e := s.presentationAdmission(ctx, room, *seat)
			if e != nil {
				pending = true
				continue
			}
			prior := base
			if confirmed := p.Applied[participant.Identity]; confirmed.SID == participant.SID && confirmed.Grant.RoomJoin {
				prior = confirmed.Grant
			}
			for _, intent := range journal.Intents {
				if intent.Binding.Identity == participant.Identity && intent.State == "active" {
					prior = s.signalIntentGrant(room, intent)
					break
				}
			}
			if p.Active {
				base = presentationGrant(base, p, *seat, s.now())
			}
			digest := mediaGrantDigest(base)
			seen[participant.Identity] = true
			if mediaPermissionNarrows(prior, base) {
				settled, err := s.drainMediaPermission(bounded, room, journal, participant.Identity, "")
				if err != nil || !settled {
					pending = true
					p.Reason = "presentation_disconnect_pending"
				}
				delete(p.Applied, participant.Identity)
				delete(seen, participant.Identity)
				continue
			}
			last := p.Applied[participant.Identity]
			if last.SID != participant.SID || last.GrantDigest != digest {
				if !capable {
					pending = true
					p.Reason = "presentation_provider_unavailable"
					continue
				}
				if _, e = permissions.UpdatePermissions(bounded, room.MediaRoom, participant.Identity, base); e != nil {
					pending = true
					p.Reason = "presentation_permission_pending"
					continue
				}
				p.Applied[participant.Identity] = presentationApplied{SID: participant.SID, GrantDigest: digest, Grant: base}
			}
			for i := range journal.Intents {
				intent := &journal.Intents[i]
				if intent.Binding.Identity == participant.Identity && intent.State == "active" {
					intent.GrantDigest = digest
					intent.Grant = base
				}
			}
		}
		for identity := range p.Applied {
			if !seen[identity] {
				delete(p.Applied, identity)
			}
		}
		for _, intent := range journal.Intents {
			if intent.State == "pending" || intent.State == "uncertain" {
				pending = true
				p.Reason = "presentation_handshake_pending"
			}
		}
		if e = s.store.saveMediaSignals(ctx, room, journal, s.now()); e != nil {
			return e
		}
		p.Pending = pending
		if !pending && slices.Contains([]string{"presentation_media_unavailable", "presentation_provider_unavailable", "presentation_permission_pending", "presentation_handshake_pending", "presentation_disconnect_pending", "presentation_stop_other_resources"}, p.Reason) {
			p.Reason = ""
		}
		return s.store.saveWorkflow(ctx, room.OwnerUserID, v, s.now())
	})
}

func (s *Service) presentationReconcileSession(ctx context.Context, site callrules.SiteRef, token string) error {
	held, _, e := s.session(memql.ContextWithFreshRead(ctx), site, token, s.now())
	if e != nil {
		return e
	}
	seat, room, e := s.seatAndRoom(ctx, site.OwnerUserID, held)
	if e != nil {
		return e
	}
	if seat == nil || room == nil || !seat.occupying() {
		return nil
	}
	return s.reconcilePresentation(ctx, *room)
}
