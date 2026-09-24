package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
)

func (s *Server) passkeyUIOrigins() []string {
	origin := strings.TrimSuffix(identity.ShellHomeURL("", s.Cfg.BaseURL), "/")
	if origin == "" {
		return nil
	}
	return []string{origin}
}

// WebAuthn Related Origin Requests preserve existing identity-host RP IDs
// while the ceremony's UI runs in OS. No caller-controlled origins are added.
func (s *Server) handleWebAuthnOrigins(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	origins := s.passkeyUIOrigins()
	if origins == nil {
		origins = []string{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"origins": origins})
}
