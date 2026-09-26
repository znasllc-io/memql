package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// NativeMediaType selects the OS representation of an identity interaction.
// The same routes retain their cookie paths and authoritative validation.
const NativeMediaType = "application/vnd.memql.identity+json"

func nativeRequest(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), NativeMediaType)
}

func writeNative(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", NativeMediaType)
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

// Native UI trust comes from the installed service configuration, never a
// proposed setup domain, request Host, or third-party OAuth registration.
func (s *Server) nativeOrigin(r *http.Request) string {
	if r != nil {
		if doors := identity.Doors(); doors != nil {
			if name, ok := doors.ReservedNameFor(r.Context(), r.Host); ok {
				return "https://" + frontdoor.AccountRoleHost(frontdoor.AccountRoleApp, name)
			}
		}
	}
	return strings.TrimSuffix(identity.ShellHomeURL("", s.Cfg.BaseURL), "/")
}

type nativeResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *nativeResponse) Header() http.Header { return w.header }
func (w *nativeResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *nativeResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (s *Server) nativeUI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := s.nativeOrigin(r)
		native := nativeRequest(r)
		if native || r.Method == http.MethodOptions {
			w.Header().Add("Vary", "Origin")
			if origin == "" || r.Header.Get("Origin") != origin {
				http.Error(w, "Identity UI origin refused", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
				w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, X-CSRF-Token, Authorization")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		out := &nativeResponse{header: make(http.Header)}
		next(out, r)
		for key, values := range out.header {
			w.Header()[key] = values
		}
		if native {
			if location := out.header.Get("Location"); location != "" {
				w.Header().Del("Location")
				writeNative(w, map[string]string{"redirect": location})
				return
			}
			if !strings.Contains(out.header.Get("Content-Type"), "json") {
				status := out.status
				if status < 400 {
					status = http.StatusServiceUnavailable
				}
				w.Header().Set("Content-Type", NativeMediaType)
				w.WriteHeader(status)
				writeNative(w, map[string]string{"error": http.StatusText(status)})
				return
			}
		} else if r.Method == http.MethodGet && strings.Contains(out.header.Get("Content-Type"), "text/html") && r.URL.Path != githubconnect.AppSetupStartPath {
			if origin == "" {
				http.Error(w, "MemQL OS origin is not configured", http.StatusServiceUnavailable)
				return
			}
			// Tokens travel in the fragment, never in OS request/access logs.
			dest := url.URL{Path: "/identity" + r.URL.Path, Fragment: r.URL.RawQuery}
			w.Header().Set("Referrer-Policy", "no-referrer")
			http.Redirect(w, r, origin+dest.String(), http.StatusSeeOther)
			return
		}
		if out.status != 0 {
			w.WriteHeader(out.status)
		}
		_, _ = w.Write(out.body.Bytes())
	}
}

// Unavailable is distinct from claimed and unclaimed. Both independent
// signals must be readable before OS may offer first-run ownership.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	if s.CountUsers == nil || s.ClusterClaimed == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeNative(w, map[string]string{"error": "Ownership state is unavailable"})
		return
	}
	n, bootstrapErr := s.CountUsers(r.Context())
	claimed, claimErr := s.ClusterClaimed(r.Context())
	if bootstrapErr != nil || claimErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeNative(w, map[string]string{"error": "Ownership state is unavailable"})
		return
	}
	state := "claimed"
	if n == 0 && !claimed {
		state = "unclaimed"
	}
	writeNative(w, map[string]string{"state": state, "csrf": CSRFTokenFromRequest(r)})
}
