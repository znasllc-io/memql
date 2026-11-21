package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/znasllc-io/memql/component/calls"
)

type CallsPortraitService interface {
	Portrait(context.Context, calls.Session, string) (calls.PortraitImage, error)
}

func (h *callsHandler) servePortrait(w http.ResponseWriter, r *http.Request, c callsCall, id string) {
	svc, ok := h.svc.(CallsPortraitService)
	if !ok {
		h.refuse(w, c, calls.ErrNotFound, calls.JoinResult{})
		return
	}
	image, err := svc.Portrait(r.Context(), c.session(), id)
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	if len(image.Data) == 0 || len(image.Data) > calls.PortraitCanonicalBytes || (image.Portrait.MimeType != "image/png" && image.Portrait.MimeType != "image/jpeg") || calls.PortraitContentDigest(image.Data) != image.Portrait.Digest {
		h.refuse(w, c, calls.ErrNotFound, calls.JoinResult{})
		return
	}
	w.Header().Set("Content-Type", image.Portrait.MimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(image.Data)))
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image.Data)
}
