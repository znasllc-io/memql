package server

import (
	"context"
	"github.com/znasllc-io/memql/component/calls"
	"net/http"
)

type CallsFamilyService interface {
	Family(context.Context, calls.SiteRef, string, calls.FamilyRequest) (calls.FamilyResult, error)
	FamilySession(context.Context, calls.SiteRef, string) (*calls.FamilySession, error)
}

func (h *callsHandler) serveFamily(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsFamilyService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	var body calls.FamilyRequest
	if !h.decode(w, r, c, &body) || !h.resolveSite(w, r, &c) {
		return
	}
	token := ""
	if c.cookie.wellFormed {
		token = c.cookie.token
	}
	out, err := svc.Family(r.Context(), c.site, token, body)
	if out.Session != nil && out.Session.Token != "" && out.Session.Token != token {
		setCallsCookie(w, out.Session.Token)
		c.cookie.presented = false
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	h.write(w, http.StatusOK, out)
}
