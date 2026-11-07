package releasecatalog

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/sdk/go/client"
)

const readPAT = "mql_pat_installation_fixture"

func readActor(ctx context.Context, role auth.Role) context.Context {
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "person", IdentityId: "pat", Role: role})
}

func TestIdentityReaderVerifiesSignedCatalogAcrossReplicasAndRevocation(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	release := fixtureRelease()
	envelope, err := pl.SignPublishedRelease(release, "key", key)
	require.NoError(t, err)
	verified, err := pl.VerifyPublishedRelease(envelope, "fixture", map[string]ed25519.PublicKey{"key": pub})
	require.NoError(t, err)
	var revoked, corrupt atomic.Bool
	var calls atomic.Int64
	handler := NewIdentityReadHandler(func(ctx context.Context, token string) (context.Context, error) {
		require.True(t, memql.FreshReadFromContext(ctx))
		if token != readPAT || revoked.Load() {
			return nil, errors.New("private token diagnostic")
		}
		return readActor(ctx, auth.RoleOwner), nil
	}, func(ctx context.Context, query string) ([]client.Row, error) {
		require.True(t, memql.FreshReadFromContext(ctx))
		calls.Add(1)
		if strings.HasPrefix(query, "builtin releaseListPublishedCandidates(") {
			return []client.Row{{"releases": []any{map[string]any{"candidateId": release.CandidateID, "catalogDigest": verified.Digest(), "components": []any{map[string]any{"name": "spoof"}}}}}}, nil
		}
		require.Equal(t, client.ReleaseGetPublishedCandidateBuild(client.ReleaseGetPublishedCandidateArgs{CandidateId: release.CandidateID}), query)
		body := json.RawMessage(envelope)
		if corrupt.Load() {
			body = json.RawMessage(`{"payloadType":"tampered"}`)
		}
		return []client.Row{{"candidateId": release.CandidateID, "catalogDigest": verified.Digest(), "envelope": body}}, nil
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	s := source{ID: "publisher", Publisher: "fixture", Endpoint: server.URL + releasewire.ReadPath, CredentialSecret: "TOKEN", PublicKeys: map[string]string{"key": base64.StdEncoding.EncodeToString(pub)}, RootCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
	newReader := func() *Reader {
		body, err := json.Marshal(configuration{FormatVersion: 1, Sources: []source{s}})
		require.NoError(t, err)
		reader, err := NewSnapshot(readerContext(), body, readPAT)
		require.NoError(t, err)
		return reader
	}
	page, err := newReader().List(readerContext(), s.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, "engine", page.Releases[0].Components[0].Name)
	second := newReader()
	got, err := second.Get(readerContext(), s.ID, release.CandidateID, verified.Digest())
	require.NoError(t, err)
	require.Equal(t, verified.Digest(), got.Digest())
	_, err = second.Get(readerContext(), s.ID, release.CandidateID, "sha256:"+strings.Repeat("f", 64))
	require.ErrorContains(t, err, "different candidate or catalog")
	corrupt.Store(true)
	_, err = second.Get(readerContext(), s.ID, release.CandidateID, verified.Digest())
	require.ErrorContains(t, err, "configured publisher keys")
	before := calls.Load()
	revoked.Store(true)
	_, err = second.Get(readerContext(), s.ID, release.CandidateID, verified.Digest())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private")
	require.Equal(t, before, calls.Load(), "revocation is rechecked before another read")
}

func TestIdentityReaderRefusesAuthorityAndProtocolConfusionBeforeQueries(t *testing.T) {
	for _, test := range []struct {
		name, body, authorization string
		role                      auth.Role
		synthetic                 bool
	}{
		{"missing credential", `{"operation":"current-user"}`, "", auth.RoleOwner, false},
		{"JWT", `{"operation":"current-user"}`, "Bearer eyJ.fixture.jwt", auth.RoleOwner, false},
		{"operator", `{"operation":"current-user"}`, "Operator secret", auth.RoleOwner, false},
		{"reader", `{"operation":"current-user"}`, "Bearer " + readPAT, auth.RoleReader, false},
		{"synthetic", `{"operation":"current-user"}`, "Bearer " + readPAT, auth.RoleOwner, true},
		{"write", `{"operation":"publish"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"arbitrary query", `{"operation":"current-user","query":"mutation deleteAll()"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"principal", `{"operation":"current-user","userId":"someone-else"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"duplicate", `{"operation":"health","operation":"current-user"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"case alias", `{"operation":"health","Operation":"current-user"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"extra input", `{"operation":"current-user","nonce":"extra"}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"trailing object", `{"operation":"current-user"}{}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"page bound", `{"operation":"list","limit":11}`, "Bearer " + readPAT, auth.RoleOwner, false},
		{"injection", `{"operation":"health","nonce":"x\") mutation deleteAll()"}`, "Bearer " + readPAT, auth.RoleOwner, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var queries atomic.Int64
			h := NewIdentityReadHandler(func(ctx context.Context, _ string) (context.Context, error) {
				return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "person", IdentityId: "pat", Role: test.role, Synthetic: test.synthetic}), nil
			}, func(context.Context, string) ([]client.Row, error) { queries.Add(1); return nil, nil })
			r := httptest.NewRequest(http.MethodPost, "https://identity.example"+releasewire.ReadPath, strings.NewReader(test.body))
			r.Header.Set("Authorization", test.authorization)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.GreaterOrEqual(t, w.Code, 400)
			require.Zero(t, queries.Load())
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		})
	}
}

func TestIdentityReaderTransportNeverForwardsBearerOrTrustsMalformedReplies(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, fault := range []string{"redirect", "untrusted", "oversized", "duplicate", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch fault {
				case "redirect":
					http.Redirect(w, r, target.URL+releasewire.ReadPath, http.StatusTemporaryRedirect)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat(" ", maxReadReply+1))
				case "duplicate":
					_, _ = io.WriteString(w, `{"id":"a","id":"b"}`)
				default:
					_, _ = io.WriteString(w, `{"id":"person"}`)
				}
			}))
			defer s.Close()
			roots := x509.NewCertPool()
			roots.AddCert(s.Certificate())
			if fault == "untrusted" {
				roots = x509.NewCertPool()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			_, err := ReadIdentityHTTP(ctx, s.URL+releasewire.ReadPath, readPAT, roots, releasewire.Request{Operation: "current-user"})
			require.Error(t, err)
			require.NotContains(t, err.Error(), readPAT)
		})
	}
	require.Zero(t, redirected.Load())
	for _, endpoint := range []string{"http://identity.example" + releasewire.ReadPath, "https://identity.example/", "https://identity.example" + releasewire.ReadPath + "?token=bad", "https://user:pass@identity.example" + releasewire.ReadPath} {
		_, err := ReadIdentityHTTP(t.Context(), endpoint, readPAT, nil, releasewire.Request{Operation: "current-user"})
		require.Error(t, err)
	}
}
