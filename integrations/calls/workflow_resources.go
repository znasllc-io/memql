package calls

import (
	"context"
	"slices"
	"sort"

	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowResourceRefs(bindings map[string]callrules.WorkflowResourceBinding) map[string]callrules.WorkflowResourceRef {
	if len(bindings) == 0 {
		return nil
	}
	refs := make(map[string]callrules.WorkflowResourceRef, len(bindings))
	for slot, binding := range bindings {
		refs[slot] = binding.WorkflowResourceRef
	}
	return refs
}

// A Skill references approved output; it never acquires publishing authority.
// A cohost may reuse an approved reference but cannot discover or bind the
// opening owner's other resources. Agents need their separate read envelope.
func (s *Service) workflowResourceAuthority(ctx context.Context, site callrules.SiteRef, ownerKey, actorKey, roomID, bookingID string, refs, approved map[string]callrules.WorkflowResourceRef) error {
	if len(refs) == 0 {
		return nil
	}
	if grantID, agent := ctx.Value(agentCallContext{}).(string); agent {
		d, err := s.store.delegation(ctx, site.OwnerUserID, grantID)
		if err != nil {
			return err
		}
		if d == nil || d.KeyID != ownerKey || d.SiteID != site.SiteID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
			return accountRefusal("delegation_inactive")
		}
		if !slices.Contains(d.State.Request.Scopes, "resources_read") {
			return accountRefusal("delegation_scope")
		}
		bounded := roomID != "" && (slices.Contains(d.State.Request.RoomIDs, roomID) || len(d.State.Request.RoomIDs) == 0 && delegationOwnsSource(d, "room", roomID))
		bounded = bounded || bookingID != "" && delegationOwnsSource(d, "booking", bookingID)
		if !bounded {
			return accountRefusal("delegation_resource_scope")
		}
		for _, ref := range refs {
			if !slices.Contains(d.State.Request.ResourceIDs, ref.ResourceID) {
				return accountRefusal("delegation_resource_scope")
			}
		}
		return nil
	}
	if actorKey == ownerKey && ownerKey != "" {
		return nil
	}
	for _, ref := range refs {
		found := false
		for _, prior := range approved {
			if prior == ref {
				found = true
				break
			}
		}
		if !found {
			return accountRefusal("workflow_resource_owner_required")
		}
	}
	return nil
}

func (s *Service) workflowCheckResourceRefs(ctx context.Context, site callrules.SiteRef, key string, definition callrules.WorkflowDefinition, refs map[string]callrules.WorkflowResourceRef) error {
	if !callrules.ValidWorkflowResourceRefs(definition, refs) {
		return accountRefusal("workflow_resource_binding_required")
	}
	if len(refs) == 0 {
		return nil
	}
	d, err := s.store.resourceState(ctx, site.OwnerUserID, site.SiteID, key)
	if err != nil {
		return err
	}
	for _, slot := range definition.Resources {
		ref := refs[slot.ID]
		source := resourceSource(d, ref.ResourceID)
		if source == nil {
			return accountRefusal("workflow_resource_unavailable")
		}
		if source.Revision != ref.Revision {
			return accountRefusal("workflow_resource_revision_changed")
		}
		if slot.Kind != "" && slot.Kind != source.Kind || !callrules.ValidWorkflowResourceSource(source.Kind, slot.Source) || !callrules.ValidResourceSpec(source.ResourceSpec) {
			return accountRefusal("workflow_resource_capability")
		}
	}
	return nil
}

func workflowBoundResources(refs map[string]callrules.WorkflowResourceRef) map[string]callrules.WorkflowResourceBinding {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]callrules.WorkflowResourceBinding, len(refs))
	for slot, ref := range refs {
		out[slot] = callrules.WorkflowResourceBinding{WorkflowResourceRef: ref}
	}
	return out
}

// Explicit transitions pin an unbound slot once. Only an explicit global Resume
// may renew an already pinned lease; branch navigation never replaces
// a shared slot behind another active branch. Observation never substitutes one.
// The named source/revision remains fixed until an explicitly reviewed Change.
func (s *Service) workflowPinResourceLeases(ctx context.Context, site callrules.SiteRef, room roomRow, r *callrules.WorkflowRun, nodeID string, renew ...bool) error {
	node, _ := callrules.WorkflowNodeByID(r.Definition, nodeID)
	if len(node.Requirements.ResourceIDs) == 0 {
		return nil
	}
	d, err := s.store.resourceState(ctx, site.OwnerUserID, site.SiteID, room.KeyID)
	if err != nil {
		return err
	}
	for _, slot := range node.Requirements.ResourceIDs {
		binding, ok := r.ResourceBindings[slot]
		if !ok {
			return accountRefusal("workflow_resource_binding_required")
		}
		if binding.LeaseID != "" && (len(renew) == 0 || !renew[0]) {
			continue
		}
		binding.LeaseID, binding.Identity, binding.ExpiresAt = "", "", nil
		source := resourceSource(d, binding.ResourceID)
		if source == nil || source.Revision != binding.Revision {
			r.ResourceBindings[slot] = binding
			continue
		}
		for _, lease := range d.State.Leases {
			if lease.ResourceID != binding.ResourceID || lease.RoomID != room.ID || (lease.Status != "publishing" && lease.Status != "awaiting_source") || !s.now().Before(lease.ExpiresAt) {
				continue
			}
			allowed, err := s.mediaResourceIdentityAllowed(ctx, room, lease.Identity)
			if err != nil {
				return err
			}
			if allowed {
				expires := lease.ExpiresAt
				binding.LeaseID, binding.Identity, binding.ExpiresAt = lease.ID, lease.Identity, &expires
				break
			}
		}
		r.ResourceBindings[slot] = binding
	}
	return nil
}

func (s *Service) workflowPinCurrentResources(ctx context.Context, site callrules.SiteRef, room roomRow, r *callrules.WorkflowRun, renew ...bool) error {
	if err := s.workflowPinResourceLeases(ctx, site, room, r, r.NodeID, renew...); err != nil {
		return err
	}
	if r.Parallel != nil {
		for _, branch := range r.Parallel.Branches {
			if branch.Status != "joined" {
				if err := s.workflowPinResourceLeases(ctx, site, room, r, branch.NodeID, renew...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) workflowResourceRequirements(ctx context.Context, site callrules.SiteRef, room roomRow, r callrules.WorkflowRun, node callrules.WorkflowNode, participants []MediaParticipant, available bool) ([]string, error) {
	if len(node.Requirements.ResourceIDs) == 0 {
		return nil, nil
	}
	d, err := s.store.resourceState(ctx, site.OwnerUserID, site.SiteID, room.KeyID)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, slotID := range node.Requirements.ResourceIDs {
		binding, ok := r.ResourceBindings[slotID]
		source := resourceSource(d, binding.ResourceID)
		if !ok || source == nil || source.Revision != binding.Revision {
			missing = append(missing, "bound_resource_unavailable")
			continue
		}
		leaseValid := false
		for _, lease := range d.State.Leases {
			if lease.ID == binding.LeaseID && lease.ResourceID == binding.ResourceID && lease.RoomID == room.ID && lease.Identity == binding.Identity && binding.ExpiresAt != nil && lease.ExpiresAt.Equal(*binding.ExpiresAt) && s.now().Before(lease.ExpiresAt) && (lease.Status == "awaiting_source" || lease.Status == "publishing") {
				leaseValid, err = s.mediaResourceIdentityAllowed(ctx, room, lease.Identity)
				if err != nil {
					return nil, err
				}
			}
		}
		if !leaseValid {
			missing = append(missing, "bound_resource_lease_unavailable")
			continue
		}
		if !available {
			missing = append(missing, "media_readiness_unavailable")
			continue
		}
		trackSource := ""
		for _, slot := range r.Definition.Resources {
			if slot.ID == slotID {
				trackSource = slot.Source
				break
			}
		}
		ready := false
		for _, participant := range participants {
			if participant.Identity == binding.Identity {
				for _, track := range participant.Tracks {
					if !track.Muted && track.Source == trackSource {
						ready = true
					}
				}
			}
		}
		if !ready {
			missing = append(missing, "bound_resource_source_required")
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing), nil
}

func (s *Service) workflowResourceOptions(ctx context.Context, site callrules.SiteRef, ownerKey, actorKey, roomID, bookingID string, approved map[string]callrules.WorkflowResourceRef, slots []callrules.WorkflowResourceSlot) ([]callrules.WorkflowResourceOption, error) {
	d, err := s.store.resourceState(ctx, site.OwnerUserID, site.SiteID, ownerKey)
	if err != nil {
		return nil, err
	}
	out := []callrules.WorkflowResourceOption{}
	for _, source := range resourceAnswer(d).Resources {
		ref := callrules.WorkflowResourceRef{ResourceID: source.ID, Revision: source.Revision}
		if s.workflowResourceAuthority(ctx, site, ownerKey, actorKey, roomID, bookingID, map[string]callrules.WorkflowResourceRef{"option": ref}, approved) != nil {
			continue
		}
		label := source.Label
		if ctx.Value(agentCallContext{}) == nil && actorKey != ownerKey {
			label = "Approved resource"
			for _, slot := range slots {
				if approved[slot.ID] == ref {
					label = slot.Label
					break
				}
			}
		}
		out = append(out, callrules.WorkflowResourceOption{ResourceID: source.ID, Revision: source.Revision, Label: label, Kind: source.Kind, Status: source.Status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out, nil
}

func workflowValidResources(v *workflowState) bool {
	if v.Run != nil {
		r := v.Run
		if !callrules.ValidWorkflowResourceRefs(r.Definition, workflowResourceRefs(r.ResourceBindings)) {
			return false
		}
		for _, binding := range r.ResourceBindings {
			if binding.LeaseID == "" && binding.Identity == "" && binding.ExpiresAt == nil {
				continue
			}
			if !callrules.ValidWorkflowID(binding.LeaseID) || binding.Identity == "" || len(binding.Identity) > 256 || binding.ExpiresAt == nil || binding.ExpiresAt.IsZero() {
				return false
			}
		}
	}
	if v.Binding != nil {
		if len(v.Binding.ResourceBindings) > 8 {
			return false
		}
		for slot, ref := range v.Binding.ResourceBindings {
			if !callrules.ValidWorkflowID(slot) || !callrules.ValidWorkflowID(ref.ResourceID) || ref.Revision < 1 || ref.Revision > 2147483647 {
				return false
			}
		}
	}
	return true
}

// A skills grant alone may read guidance and stop it, but never reveals another
// resource grant's identifiers. Apply this to every response, including exact
// retries and failure checkpoints, without mutating the stored binding.
func (s *Service) workflowResourceProjection(ctx context.Context, site callrules.SiteRef, owner, actor, room, booking string, out *callrules.WorkflowResult, storedRefs ...map[string]callrules.WorkflowResourceRef) {
	if ctx.Value(agentCallContext{}) == nil {
		return
	}
	refs := map[string]callrules.WorkflowResourceRef(nil)
	if out.Run != nil {
		refs = workflowResourceRefs(out.Run.ResourceBindings)
	}
	if len(storedRefs) > 0 {
		refs = storedRefs[0]
	}
	if out.Run != nil && s.workflowResourceAuthority(ctx, site, owner, actor, room, booking, refs, nil) != nil {
		out.Run.ResourceBindings = nil
		out.Run.AllowedActions = slices.DeleteFunc(out.Run.AllowedActions, func(action string) bool { return action != "pause" && action != "cancel" && action != "change" })
		if out.Run.Parallel != nil {
			for i := range out.Run.Parallel.Branches {
				out.Run.Parallel.Branches[i].AllowedActions = []string{}
			}
		}
	}
	if out.Binding != nil && s.workflowResourceAuthority(ctx, site, owner, actor, room, booking, out.Binding.ResourceBindings, nil) != nil {
		copy := *out.Binding
		copy.ResourceBindings = nil
		out.Binding = &copy
	}
}

// A lost exact pin cannot clear on observation: Resume is the explicit request
// to check a newly owner-started lease. Advertise that check for this blocker,
// while its command still refuses absent sources and every unrelated blocker.
func workflowResourceResumeCheckable(blockers []string) bool {
	for _, blocker := range blockers {
		if blocker != "bound_resource_lease_unavailable" {
			return false
		}
	}
	return true
}
