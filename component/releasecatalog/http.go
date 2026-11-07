package releasecatalog

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/sdk/go/client"
)

const maxReadReply = 4 << 20

var readNonce = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// readQuery is a closed protocol, not a query proxy. Callers cannot supply a
// query, principal, endpoint or additional arguments. Builders quote every
// value; the native catalog/health handlers still enforce their own role gate.
func readQuery(r releasewire.Request) (string, error) {
	switch r.Operation {
	case "catalog":
		if digestPattern.MatchString(r.CandidateID) && r.Cursor == "" && r.Limit == 0 && r.Nonce == "" {
			return client.ReleaseGetPublishedCandidateBuild(client.ReleaseGetPublishedCandidateArgs{CandidateId: r.CandidateID}), nil
		}
	case "list":
		if r.CandidateID == "" && len(r.Cursor) <= 1024 && r.Limit >= 1 && r.Limit <= maxPage && r.Nonce == "" {
			return client.ReleaseListPublishedCandidatesBuild(client.ReleaseListPublishedCandidatesArgs{Cursor: r.Cursor, Limit: r.Limit}), nil
		}
	case "health":
		if r.CandidateID == "" && r.Cursor == "" && r.Limit == 0 && readNonce.MatchString(r.Nonce) {
			return client.ReleaseInstallationHealthBuild(client.ReleaseInstallationHealthArgs{Nonce: r.Nonce}), nil
		}
	case "current-user":
		if r.CandidateID == "" && r.Cursor == "" && r.Limit == 0 && r.Nonce == "" {
			return client.CurrentUserBuild(client.CurrentUserArgs{}), nil
		}
	}
	return "", errors.New("invalid installation read")
}

// NewIdentityReadHandler is mounted only by Identity. authenticate verifies the
// PAT and CURRENT owning user on every request, then replaces any ambient actor.
// Missing/revoked/suspended credentials and synthetic principals fail closed.
// No credential is exchanged, issued, retained or forwarded to the BFF.
func NewIdentityReadHandler(authenticate func(context.Context, string) (context.Context, error), execute func(context.Context, string) ([]client.Row, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodPost || r.URL.Path != releasewire.ReadPath || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "invalid installation read", http.StatusBadRequest)
			return
		}
		if r.TLS == nil || authenticate == nil || execute == nil {
			http.Error(w, "installation read unavailable", http.StatusServiceUnavailable)
			return
		}
		header := r.Header.Values("Authorization")
		if len(header) != 1 || !strings.HasPrefix(header[0], "Bearer mql_pat_") || len(header[0]) > 16<<10 || strings.ContainsAny(strings.TrimPrefix(header[0], "Bearer "), "\r\n\t ") {
			http.Error(w, "installation read requires a personal access token", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		var request releasewire.Request
		if err != nil || decode(body, &request) != nil {
			http.Error(w, "invalid installation read", http.StatusBadRequest)
			return
		}
		query, err := readQuery(request)
		if err != nil {
			http.Error(w, "invalid installation read", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		ctx, err = authenticate(memql.ContextWithFreshRead(ctx), strings.TrimPrefix(header[0], "Bearer "))
		if err != nil || ctx == nil {
			http.Error(w, "installation credential was refused", http.StatusUnauthorized)
			return
		}
		actor, _ := auth.AccessFromContext(ctx)
		if admit(ctx) != nil || actor == nil || memql.BareShortId(actor.IdentityId) == "" {
			http.Error(w, "installation read requires an identified developer, admin or owner", http.StatusForbidden)
			return
		}
		rows, err := execute(memql.ContextWithFreshRead(ctx), query)
		if err != nil || len(rows) != 1 {
			http.Error(w, "installation evidence is unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err = json.Marshal(rows[0])
		if err != nil || len(body) > maxReadReply {
			http.Error(w, "installation evidence is unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// ReadIdentityHTTP uses one explicitly configured HTTPS origin and exact path.
// Redirects never receive the bearer, and response/error bodies are bounded.
// Each call reauthenticates the protected original PAT; there is no token cache
// or fallback to another principal or transport after a refusal.
func ReadIdentityHTTP(ctx context.Context, endpoint, token string, roots *x509.CertPool, request releasewire.Request) (client.Row, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != releasewire.ReadPath || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || len(endpoint) > 2048 || strings.ContainsAny(endpoint, "\r\n\t ") {
		return nil, errors.New("installation read requires the exact Identity HTTPS endpoint")
	}
	if !strings.HasPrefix(token, "mql_pat_") || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\t ") {
		return nil, errors.New("Identity installation read requires a personal access token")
	}
	if _, err := readQuery(request); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid installation read endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := hc.Do(req)
	if err != nil {
		return nil, transportError(ctx)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, transportError(ctx)
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, maxReadReply+1))
	var row client.Row
	if err != nil || len(body) > maxReadReply || decode(body, &row) != nil || row == nil {
		return nil, errors.New("installation read returned invalid evidence")
	}
	return row, nil
}
