package server

import (
	"context"
	"net/http"

	"github.com/znasllc-io/memql/component/calls"
)

type callsHistoryService interface {
	History(context.Context, calls.Session, calls.HistoryRequest) (calls.HistoryPage, error)
	HistoryAction(context.Context, calls.Session, calls.HistoryActionRequest) (calls.HistoryActionResult, error)
}

func (h *callsHandler) serveHistory(w http.ResponseWriter, r *http.Request, c callsCall) {
	service, ok := h.svc.(callsHistoryService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	if r.Method == http.MethodGet {
		req, err := calls.ParseHistoryQuery(r.URL.RawQuery)
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
		if !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
			return
		}
		result, err := service.History(r.Context(), c.session(), req)
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
		h.write(w, http.StatusOK, result)
		return
	}
	var req calls.HistoryActionRequest
	if r.URL.RawQuery != "" {
		h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
		return
	}
	if !h.decode(w, r, c, &req) || !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	result, err := service.HistoryAction(r.Context(), c.session(), req)
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	h.write(w, http.StatusOK, result)
}
