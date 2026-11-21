package server

import (
	"context"
	"github.com/znasllc-io/memql/component/calls"
	"net/http"
)

type CallsWorkflowService interface {
	Workflow(context.Context, calls.SiteRef, string, calls.WorkflowRequest) (calls.WorkflowResult, error)
	Resource(context.Context, calls.SiteRef, string, calls.ResourceRequest) (calls.ResourceResult, error)
	Invite(context.Context, calls.Session, calls.InviteRequest) (calls.InviteResult, error)
}

// One narrow product route serves both human capabilities; it never dispatches
// arbitrary engine actions. Machine credentials are accepted only on /agent.
func (h *callsHandler) serveWorkflow(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsWorkflowService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	if !h.requireSession(w, c) {
		return
	}
	var req struct {
		Invitation *calls.InviteRequest   `json:"invitation,omitempty"`
		Workflow   *calls.WorkflowRequest `json:"workflow,omitempty"`
		Resource   *calls.ResourceRequest `json:"resource,omitempty"`
	}
	if !h.decode(w, r, c, &req) {
		return
	}
	count := 0
	if req.Workflow != nil {
		count++
	}
	if req.Resource != nil {
		count++
	}
	if req.Invitation != nil {
		count++
	}
	if count != 1 {
		h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
		return
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	var result any
	var err error
	if req.Invitation != nil {
		result, err = svc.Invite(r.Context(), c.session(), *req.Invitation)
	} else if req.Workflow != nil {
		result, err = svc.Workflow(r.Context(), c.site, c.cookie.token, *req.Workflow)
	} else {
		result, err = svc.Resource(r.Context(), c.site, c.cookie.token, *req.Resource)
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	h.write(w, http.StatusOK, result)
}
