package calls

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// Workflow-owned permission intent, not a snapshot to blindly restore. Native
// grants are recomputed on release so this policy never grants another role's
// permissions or reactivates a device. Provider confirmations bind SID + grant.
type presentationIntent struct {
	RunID, VisitID   string
	Revision         int64
	Active, Pending  bool
	Reason           string
	Presenter        presentationAdmission
	Audience         callrules.PublicationPermissions
	Speaker          presentationAdmission
	SpeakerExpiresAt time.Time
	Applied          map[string]presentationApplied
}
type presentationAdmission struct {
	SeatID, Identity, Name, Kind, ResourceID, LeaseID string
	JoinedAt                                          time.Time
}
type presentationApplied struct {
	SID, GrantDigest string
	Grant            callrules.GrantSpec
}

func (p presentationAdmission) matches(seat seatRow) bool {
	return p.SeatID == seat.ID && p.Identity == seat.Identity && p.JoinedAt.Equal(seat.JoinedAt) && seat.occupying()
}
func (s *Service) presentationAdmission(ctx context.Context, r roomRow, seat seatRow) (presentationAdmission, callrules.GrantSpec, error) {
	p := presentationAdmission{SeatID: seat.ID, Identity: seat.Identity, Name: seat.DisplayName, Kind: seat.Kind, JoinedAt: seat.JoinedAt}
	base, e := callrules.GrantFor(callrules.SeatKind(seat.Kind))
	if e != nil {
		return p, base, e
	}
	base.CanPublishData = base.CanPublishData && r.MessageProtocol != callrules.MessageProtocol
	if !seat.occupying() || seat.RoomID != r.ID {
		return p, base, accountRefusal("presentation_admission_changed")
	}
	resource, e := s.mediaResourceIdentityAllowed(ctx, r, seat.Identity)
	if e != nil {
		return p, base, e
	}
	if resource {
		d, e := s.store.resourceState(ctx, r.OwnerUserID, r.SiteID, seat.KeyID)
		if e != nil {
			return p, base, e
		}
		for _, l := range d.State.Leases {
			if l.Identity == seat.Identity && l.SeatID == seat.ID && l.RoomID == r.ID && l.active() {
				source := resourceSource(d, l.ResourceID)
				if source == nil {
					return p, base, accountRefusal("presentation_resource_changed")
				}
				p.Kind, p.ResourceID, p.LeaseID = "resource", l.ResourceID, l.ID
				base = callrules.GrantSpec{RoomJoin: true, CanPublish: true, CanSubscribe: false, Sources: []string{"screen_share", "screen_share_audio"}}
				if source.Kind == "camera" || source.Kind == "microphone" {
					base.Sources = []string{"microphone", "camera"}
				}
				return p, base, nil
			}
		}
		return p, base, accountRefusal("presentation_resource_changed")
	}
	b, e := s.mediaSeatBinding(ctx, r, seat)
	if e != nil {
		return p, base, e
	}
	if e = s.checkMediaBinding(ctx, r, b); e != nil {
		return p, base, e
	}
	return p, base, nil
}
func (s *Service) presentationCurrent(ctx context.Context, r roomRow, p presentationAdmission) bool {
	seat, e := s.store.seatByID(ctx, r.OwnerUserID, p.SeatID)
	if e != nil || seat == nil || !p.matches(*seat) {
		return false
	}
	current, _, e := s.presentationAdmission(ctx, r, *seat)
	return e == nil && current.ResourceID == p.ResourceID && current.LeaseID == p.LeaseID
}
func (s *Service) presentationEffective(ctx context.Context, r roomRow, v *workflowState) (bool, string) {
	if v == nil || v.Run == nil || v.Presentation == nil || !v.Presentation.Active {
		return false, ""
	}
	p := v.Presentation
	if p.RunID != v.Run.ID || p.VisitID != v.Run.VisitID || workflowTerminal(v.Run) {
		return false, "presentation_visit_finished"
	}
	n, ok := callrules.WorkflowNodeByID(v.Run.Definition, v.Run.NodeID)
	if !ok || n.Type != "presentation" {
		return false, "presentation_visit_finished"
	}
	if e := s.workflowEntitlement(ctx, callrules.SiteRef{SiteID: r.SiteID, OwnerUserID: r.OwnerUserID}, r); e != nil {
		return false, "presentation_insights_unavailable"
	}
	if !s.presentationCurrent(ctx, r, p.Presenter) {
		return false, "presentation_presenter_unavailable"
	}
	return true, ""
}
func workflowInvalidatePresentation(v *workflowState, now time.Time) {
	p := v.Presentation
	if p == nil {
		return
	}
	if p.Active && (v.Run == nil || workflowTerminal(v.Run) || p.RunID != v.Run.ID || p.VisitID != v.Run.VisitID) {
		p.Active = false
		p.Pending = true
		p.Reason = "presentation_visit_finished"
		p.Revision++
	}
	if p.Speaker.SeatID != "" && !now.Before(p.SpeakerExpiresAt) {
		p.Speaker = presentationAdmission{}
		p.SpeakerExpiresAt = time.Time{}
		p.Pending = true
		p.Revision++
	}
}
func workflowValidPresentation(v *workflowState) bool {
	p := v.Presentation
	if p == nil {
		return true
	}
	if v.Kind != "run" || p.Revision < 1 || !callrules.ValidWorkflowID(p.RunID) || !callrules.ValidWorkflowID(p.VisitID) || len(p.Applied) > 128 {
		return false
	}
	if p.Active && (p.Presenter.SeatID == "" || p.Presenter.Identity == "" || p.Presenter.JoinedAt.IsZero()) {
		return false
	}
	return p.Speaker.SeatID == "" || p.Speaker.Identity != "" && !p.Speaker.JoinedAt.IsZero() && !p.SpeakerExpiresAt.IsZero()
}
func presentationGrant(base callrules.GrantSpec, p *presentationIntent, seat seatRow, now time.Time) callrules.GrantSpec {
	if p == nil || !p.Active || p.Presenter.matches(seat) {
		return base
	}
	allow := p.Audience
	if p.Speaker.matches(seat) && now.Before(p.SpeakerExpiresAt) {
		allow.Microphone = true
	}
	base.Sources = slices.DeleteFunc(slices.Clone(base.Sources), func(source string) bool {
		switch source {
		case "microphone":
			return !allow.Microphone
		case "camera":
			return !allow.Camera
		case "screen_share", "screen_share_audio":
			return !allow.ScreenShare
		default:
			return true
		}
	})
	base.CanPublish = len(base.Sources) > 0
	return base
}
func mediaGrantDigest(spec callrules.GrantSpec) string {
	raw, _ := json.Marshal(spec)
	return callrules.HistoryDigest(nil, "native-media-permissions", string(raw))
}

func mediaPermissionNarrows(prior, next callrules.GrantSpec) bool {
	if prior.CanSubscribe && !next.CanSubscribe || prior.CanPublishData && !next.CanPublishData || prior.CanUpdateOwnMetadata && !next.CanUpdateOwnMetadata {
		return true
	}
	if !prior.CanPublish {
		return false
	}
	if !next.CanPublish {
		return true
	}
	if len(prior.Sources) == 0 {
		return true
	}
	for _, source := range prior.Sources {
		if !slices.Contains(next.Sources, source) {
			return true
		}
	}
	return false
}
func (s *Service) signalIntentGrant(r roomRow, intent mediaSignalIntent) callrules.GrantSpec {
	if intent.Grant.RoomJoin {
		return intent.Grant
	}
	// Earlier native journals stored only the digest. Their maximum possible
	// grant is the admission kind's native base; never assume it was narrower.
	base, _ := callrules.GrantFor(callrules.SeatKind(intent.Binding.Kind))
	base.CanPublishData = base.CanPublishData && r.MessageProtocol != callrules.MessageProtocol
	return base
}

// Narrowing is stronger than UpdateParticipant readback. Pinned LiveKit can
// schedule publication before its final source-permission check, so remove the
// old participant and fence its writers before claiming completed enforcement.
// Unknown handshakes remain pending even after one empty provider inventory.
func (s *Service) drainMediaPermission(ctx context.Context, r roomRow, j *mediaSignalJournal, identity, settledIntent string) (bool, error) {
	unknown := false
	for i := range j.Intents {
		v := &j.Intents[i]
		if v.Binding.Identity != identity {
			continue
		}
		if (v.State == "pending" || v.State == "uncertain") && v.ID != settledIntent {
			unknown = true
		} else {
			v.State = "draining"
		}
	}
	j.LastVerifiedAt = time.Time{}
	j.LastCleanupReason = "media_permissions_pending"
	if e := s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
		return false, e
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	backend, e := s.backendFor(bounded, r)
	if e != nil {
		return false, accountRefusal("media_permissions_pending")
	}
	if e = backend.RemoveParticipant(bounded, r.MediaRoom, identity); e != nil {
		return false, accountRefusal("media_permissions_pending")
	}
	people, e := backend.Participants(bounded, r.MediaRoom)
	if e != nil {
		return false, accountRefusal("media_permissions_pending")
	}
	for _, person := range people {
		if person.Identity == identity {
			return false, accountRefusal("media_permissions_pending")
		}
	}
	if unknown {
		return false, nil
	}
	for i := len(j.Intents) - 1; i >= 0; i-- {
		if j.Intents[i].Binding.Identity == identity {
			j.remove(j.Intents[i].ID)
		}
	}
	if e = s.store.saveMediaSignals(ctx, r, j, s.now()); e != nil {
		return false, e
	}
	return true, nil
}
func (s *Service) currentMediaGrant(ctx context.Context, r roomRow, b mediaSignalBinding) (callrules.GrantSpec, error) {
	base, e := callrules.GrantFor(callrules.SeatKind(b.Kind))
	if e != nil {
		return base, e
	}
	base.CanPublishData = base.CanPublishData && r.MessageProtocol != callrules.MessageProtocol
	if b.Kind == string(callrules.KindSampler) {
		return base, nil
	}
	v, e := s.store.workflowState(ctx, r.OwnerUserID, workflowStateID(r.SiteID, "run", r.ID))
	if e != nil {
		return base, e
	}
	active, _ := s.presentationEffective(ctx, r, v)
	if !active {
		return base, nil
	}
	seat, e := s.store.seatByID(ctx, r.OwnerUserID, b.SeatID)
	if e != nil {
		return base, e
	}
	if seat == nil {
		return base, accountRefusal("media_admission_closed")
	}
	return presentationGrant(base, v.Presentation, *seat, s.now()), nil
}

// Managed ingress mints its own provider JWT on a private network. An active
// policy cannot pretend an UpdateParticipant would limit its next reconnect.
// Existing exact selected leases can retry; new competing starts/switches wait
// for explicit policy release. Device start remains the owner's own action.
func (s *Service) presentationResourceStartAllowed(ctx context.Context, r roomRow) error {
	v, e := s.store.workflowState(ctx, r.OwnerUserID, workflowStateID(r.SiteID, "run", r.ID))
	if e != nil {
		return e
	}
	if active, _ := s.presentationEffective(ctx, r, v); active {
		return accountRefusal("presentation_stop_other_resources")
	}
	return nil
}

// Include preparing/revoking leases and departed seats. Until DeleteIngress is
// confirmed, their stream keys can still produce a private reconnect; the seat
// roster alone is therefore insufficient evidence for presenter exclusivity.
func (s *Service) presentationResourcesMatch(ctx context.Context, r roomRow, selected presentationAdmission) error {
	states, e := s.store.resourceStates(ctx)
	if e != nil {
		return e
	}
	for _, state := range states {
		for _, lease := range state.State.Leases {
			if lease.RoomID != r.ID || !lease.active() {
				continue
			}
			if selected.ResourceID != lease.ResourceID || selected.LeaseID != lease.ID || selected.Identity != lease.Identity || (lease.Status != "awaiting_source" && lease.Status != "publishing") {
				return accountRefusal("presentation_stop_other_resources")
			}
		}
	}
	return nil
}
