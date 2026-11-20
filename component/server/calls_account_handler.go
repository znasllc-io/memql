package server

import (
	"context"
	"net/http"

	"github.com/znasllc-io/memql/component/calls"
)

// CallsAccountService is optional for older embedders; absence refuses this
// route instead of weakening the account verification boundary.
type CallsAccountService interface {
	Account(context.Context, calls.SiteRef, string, *calls.AccountRequest) (calls.AccountResult, error)
}

func (h *callsHandler) serveAccount(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsAccountService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	var req *calls.AccountRequest
	if r.Method == http.MethodPost {
		var body calls.AccountRequest
		if !h.decode(w, r, c, &body) {
			return
		}
		req = &body
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	result, err := svc.Account(r.Context(), c.site, c.cookie.token, req)
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	if result.Session != nil {
		setCallsCookie(w, result.Session.Token)
	} else if result.ClearSession {
		clearCallsCookie(w)
	}
	h.write(w, http.StatusOK, result)
}
