package server

import (
	"context"
	"github.com/znasllc-io/memql/component/calls"
	"net/http"
	"strconv"
)

type CallsMessageService interface {
	Message(context.Context, calls.Session, calls.MessageRequest) (calls.MessageResult, error)
}

func (h *callsHandler) serveMessage(w http.ResponseWriter, r *http.Request, c callsCall) {
	svc, ok := h.svc.(CallsMessageService)
	if !ok {
		h.refuse(w, c, errCallsNotHere, calls.JoinResult{})
		return
	}
	if !h.requireSession(w, c) {
		return
	}
	var q calls.MessageRequest
	if !h.decode(w, r, c, &q) {
		return
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	result, e := svc.Message(r.Context(), c.session(), q)
	if e != nil {
		h.refuse(w, c, e, calls.JoinResult{})
		return
	}
	h.writeMessage(w, result)
}
func (h *callsHandler) writeMessage(w http.ResponseWriter, r calls.MessageResult) {
	status := http.StatusOK
	switch r.Status {
	case "stale":
		status = http.StatusConflict
	case "rate_limited":
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.FormatInt(max(1, (r.RetryAfterMS+999)/1000), 10))
	case "unavailable", "delivery_uncertain":
		status = http.StatusServiceUnavailable
	}
	h.write(w, status, r)
}
