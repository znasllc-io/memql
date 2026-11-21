package calls

import (
	"context"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"slices"
)

func agentRequestScope(r callrules.AgentRequest) string {
	if r.Spotlight != nil && (r.Action != "host" || r.Control != "spotlight") {
		return ""
	}
	if reportAction(r.Action) {
		if r.Report == nil {
			return ""
		}
		return "report"
	}
	if r.Report != nil {
		return ""
	}
	if r.Action == "processing_consent" {
		if r.Processing == nil {
			return ""
		}
		return "processing"
	}
	if r.Processing != nil {
		return ""
	}
	if r.Action == "invite" && r.Invitation != nil && r.Workflow == nil && r.Resource == nil && r.Proposal == nil {
		return "invite"
	}
	if r.Invitation != nil || r.Action == "invite" {
		return ""
	}
	if (r.Action == "proposal" && r.Proposal != nil || r.Action == "proposal_status" && r.Proposal == nil) && r.Workflow == nil && r.Resource == nil {
		return "read"
	}
	if r.Proposal != nil {
		return ""
	}
	if r.Action == "workflow" && r.Workflow != nil && r.Resource == nil {
		return WorkflowScope(r.Workflow.Action)
	}
	if r.Action == "resource" && r.Resource != nil && r.Workflow == nil {
		return callrules.ResourceScope(r.Resource.Action)
	}
	if r.Workflow != nil || r.Resource != nil {
		return ""
	}
	return agentScope(r.Action)
}
func delegationAllowsSkill(d *delegationRow, skill string, version int) bool {
	return slices.Contains(d.State.Request.SkillVersions, callrules.SkillVersionAuthority{SkillID: skill, Version: version})
}
func (s *Service) agentWorkflow(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, sess callrules.Session, r callrules.WorkflowRequest) (callrules.WorkflowResult, error) {
	if WorkflowScope(r.Action) == "skills_control" {
		if r.Action == "start" || r.Action == "change" || r.Action == "bind_booking" {
			if !delegationAllowsSkill(d, r.SkillID, r.Version) {
				return callrules.WorkflowResult{}, accountRefusal("delegation_skill_version")
			}
		}
	}
	// The runtime rechecks active delegated authority under its room gate before
	// committing controls. The earlier dispatcher decision alone is not a lease.
	return s.Workflow(ctx, site, sess.Token, r)
}
func (s *Service) agentResource(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, sess callrules.Session, r callrules.ResourceRequest) (callrules.ResourceResult, error) {
	none := callrules.ResourceResult{}
	if r.Action == "switch" && !slices.Contains(d.State.Request.Scopes, "resources_publish") {
		return none, accountRefusal("delegation_scope")
	}
	if r.Action != "register" && r.Action != "list" && !slices.Contains(d.State.Request.ResourceIDs, r.ResourceID) {
		return none, accountRefusal("delegation_resource_scope")
	}
	if r.Action == "start" || r.Action == "switch" {
		if r.RoomID == "" {
			return none, accountRefusal("delegation_room_scope")
		}
		if len(d.State.Request.RoomIDs) > 0 {
			if !slices.Contains(d.State.Request.RoomIDs, r.RoomID) {
				return none, accountRefusal("delegation_room_scope")
			}
		} else {
			room, err := s.store.roomByID(memql.ContextWithFreshRead(ctx), site.OwnerUserID, r.RoomID)
			if err != nil {
				return none, err
			}
			if room == nil || room.SiteID != site.SiteID || room.KeyID != d.KeyID || (!delegationOwnsSource(d, "room", room.ID) && !delegationOwnsSource(d, "booking", room.ScheduledCallID)) {
				return none, accountRefusal("delegation_room_scope")
			}
		}
	}
	out, err := s.Resource(ctx, site, sess.Token, r)
	if err != nil {
		return out, err
	}
	registeredID := ""
	if r.Action == "register" {
		state, e := s.store.resourceState(memql.ContextWithFreshRead(ctx), site.OwnerUserID, site.SiteID, d.KeyID)
		if e != nil {
			return none, e
		}
		registeredID = state.State.Registrations[r.RequestID]
	}
	filtered := out.Resources[:0]
	for _, resource := range out.Resources {
		if slices.Contains(d.State.Request.ResourceIDs, resource.ID) || resource.ID == registeredID {
			filtered = append(filtered, resource)
		}
	}
	out.Resources = filtered
	return out, nil
}
