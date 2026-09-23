package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity"
	identityhttp "github.com/znasllc-io/memql/component/identity/http"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
)

// Exercise the actual NetHTTP outer middleware with both identity mounts.
// Route-only tests miss an outer CORS handler that terminates OPTIONS before
// the CSRF and credentialed WebAuthn policies can answer the browser.
func TestIdentityCORSBehindNetHTTP(t *testing.T) {
	const origin = "https://os.test"
	const foreign = "https://foreign.test"
	for _, outerOrigins := range [][]string{{"*"}, {foreign}} {
		t.Run(strings.Join(outerOrigins, ","), func(t *testing.T) {
			cfg := identity.Config{BaseURL: "https://identity.test", DeployProvider: "docker-local", CORSAllowedOrigins: []string{origin}}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			web, err := identityweb.NewServer(cfg, logger, nil)
			require.NoError(t, err)
			web.CountUsers = func(context.Context) (int, error) { return 0, nil }
			web.ClusterClaimed = func(context.Context) (bool, error) { return false, nil }
			mux := http.NewServeMux()
			web.Mount(mux)
			(&identityhttp.Server{Cfg: cfg, Logger: logger}).Mount(mux)
			srv, err := NewNetHTTP(ComponentName, WithStrictServer(&Server{}), WithBaseRouter(mux), WithBaseURL(""), WithAllowedOrigins(outerOrigins...), WithLoggerWriter(io.Discard))
			require.NoError(t, err)
			_, cancel, err := srv.prepareForRun(context.Background())
			require.NoError(t, err)
			defer cancel()
			for _, path := range []string{"/setup", "/auth/setup/passkey", "/auth/setup/resume", "/auth/webauthn/register/begin", "/auth/webauthn/register/finish", "/auth/webauthn/login/begin", "/auth/webauthn/login/finish"} {
				t.Run(path, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodOptions, "https://identity.test"+path, nil)
					req.Header.Set("Origin", origin)
					req.Header.Set("Access-Control-Request-Method", http.MethodPost)
					headers := "authorization,content-type"
					if !strings.Contains(path, "/webauthn/") {
						headers = "x-csrf-token"
					}
					req.Header.Set("Access-Control-Request-Headers", headers)
					res := httptest.NewRecorder()
					srv.server.Handler.ServeHTTP(res, req)
					require.Equal(t, http.StatusNoContent, res.Code)
					require.Equal(t, origin, res.Header().Get("Access-Control-Allow-Origin"))
					require.Equal(t, "true", res.Header().Get("Access-Control-Allow-Credentials"))
					require.Contains(t, res.Header().Get("Access-Control-Allow-Methods"), http.MethodPost)
					for _, h := range strings.Split(headers, ",") {
						require.Contains(t, strings.ToLower(res.Header().Get("Access-Control-Allow-Headers")), h)
					}
					req.Header.Set("Origin", foreign)
					denied := httptest.NewRecorder()
					srv.server.Handler.ServeHTTP(denied, req)
					require.Empty(t, denied.Header().Get("Access-Control-Allow-Origin"))
					require.Empty(t, denied.Header().Get("Access-Control-Allow-Credentials"))
				})
			}
			// The generic outer allowlist must not add credentialed access to the
			// actual response after the route denied that same origin's preflight.
			req := httptest.NewRequest(http.MethodGet, "https://identity.test/setup", nil)
			req.Header.Set("Accept", identityweb.NativeMediaType)
			req.Header.Set("Origin", foreign)
			res := httptest.NewRecorder()
			srv.server.Handler.ServeHTTP(res, req)
			require.Equal(t, http.StatusForbidden, res.Code)
			require.Empty(t, res.Header().Get("Access-Control-Allow-Origin"))
			require.Empty(t, res.Header().Get("Access-Control-Allow-Credentials"))
		})
	}
}
