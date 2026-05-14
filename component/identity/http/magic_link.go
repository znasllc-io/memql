package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/visionarys-io/memql/component/identity/magiclink"
)

// magicLinkRequest is the JSON body POST /auth/magic-link expects.
// Mirrors the OAuth code flow: the browser application sends its
// clientId + redirectURI + state up front so the issuer can stamp the
// context onto the magic-link row and the verifier knows where to
// redirect after consume.
type magicLinkRequest struct {
	Email        string `json:"email"`
	ClientId     string `json:"clientId"`
	RedirectURI  string `json:"redirectURI"`
	State        string `json:"state,omitempty"`
	InvitationId string `json:"invitationId,omitempty"`
}

// magicLinkResponse is intentionally bland: it tells the caller "we
// accepted the request" without leaking whether the email is known
// (timing-safe answer regardless of registration state).
type magicLinkResponse struct {
	Status string `json:"status"`
	Action string `json:"action,omitempty"` // "magic_link_sent" or "access_request_created"
}

func (s *Server) handleMagicLink(w http.ResponseWriter, r *http.Request) {
	var body magicLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "request body must be JSON")
		return
	}
	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || body.ClientId == "" || body.RedirectURI == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing_field", "email, clientId, and redirectURI are required")
		return
	}

	in := magiclink.IssueInput{
		Email:        body.Email,
		ClientId:     body.ClientId,
		RedirectURI:  body.RedirectURI,
		State:        body.State,
		InvitationId: body.InvitationId,
		SourceIP:     clientIP(r),
		UserAgent:    r.Header.Get("User-Agent"),
	}

	err := s.MLIssuer.Issue(r.Context(), in)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, magicLinkResponse{Status: "ok", Action: "magic_link_sent"})
		return

	case errors.Is(err, magiclink.ErrAccessRequestPath):
		// Waitlist mode — we recorded an access request but did not
		// send a magic link. The frontend renders a "thanks, an admin
		// will review" message.
		writeJSON(w, http.StatusOK, magicLinkResponse{Status: "ok", Action: "access_request_created"})
		return

	case errors.Is(err, magiclink.ErrEmailNotAllowed):
		// Don't leak whether the email is known. Respond 200 with the
		// same shape as a successful issue; the operator's audit log
		// captures the actual block reason.
		writeJSON(w, http.StatusOK, magicLinkResponse{Status: "ok", Action: "magic_link_sent"})
		return

	case errors.Is(err, magiclink.ErrUnknownClient), errors.Is(err, magiclink.ErrRedirectMismatch):
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client", "clientId or redirectURI is not registered")
		return

	case errors.Is(err, magiclink.ErrClusterNotBootstrapped):
		// 423 Locked + a stable error code so SPAs can surface a
		// "this cluster hasn't been set up yet" message and direct
		// the operator to /setup.
		s.writeJSONError(w, http.StatusLocked, "cluster_not_bootstrapped",
			"This cluster has not completed first-run setup. The operator must finish /setup before sign-in is available.")
		return

	default:
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("magic_link_handler_failed",
				slog.String("error_id", eid),
				slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "internal_error", "magic link request failed; reference "+eid)
		return
	}
}

// writeJSON serializes v as JSON and writes it with the given status.
// Errors during encoding are logged at warn level — the response is
// already partially written so we cannot recover.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError standardizes the error envelope across handlers.
// Adds an errorId so support / debugging can correlate user reports
// with server logs.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   code,
		"message": message,
		"errorId": generateErrorId(),
	})
}
