package app

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/releasecatalog"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/sdk/go/client"
)

type installationVerifierFunc func(context.Context, string) (*pat.Claims, error)

func (f installationVerifierFunc) VerifyToken(ctx context.Context, token string) (*pat.Claims, error) {
	return f(ctx, token)
}

func TestInstallationPATReplacesAmbientSystemAuthorityAndRechecksRevocation(t *testing.T) {
	token, _, err := pat.Mint()
	require.NoError(t, err)
	var revoked bool
	verify := installationReadAuthenticator(installationVerifierFunc(func(ctx context.Context, raw string) (*pat.Claims, error) {
		require.True(t, memql.FreshReadFromContext(ctx))
		require.Equal(t, token, raw)
		if revoked {
			return nil, pat.ErrPATRevoked
		}
		return &pat.Claims{Source: pat.SourcePAT, UserId: "v1:identity:user:person", PATID: "v1:identity:identity:key", Role: "developer"}, nil
	}))
	ctx, err := verify(identity.ContextWithSystemActor(t.Context()), token)
	require.NoError(t, err)
	actor, ok := auth.AccessFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "v1:identity:user:person", actor.UserId)
	require.Equal(t, "v1:identity:identity:key", actor.IdentityId)
	require.Equal(t, auth.RoleDeveloper, actor.Role)
	require.False(t, actor.Synthetic || actor.RoleStandIn || actor.Unranked)
	revoked = true
	_, err = verify(ctx, token)
	require.Error(t, err)
	for _, claims := range []*pat.Claims{nil, {Source: pat.SourceJWT, UserId: "person", PATID: "key"}, {Source: pat.SourcePAT, UserId: "person"}} {
		_, err = installationReadAuthenticator(installationVerifierFunc(func(context.Context, string) (*pat.Claims, error) { return claims, nil }))(ctx, token)
		require.Error(t, err)
	}
}

func TestInstallationContinuityUsesIdentityTLSAndOriginalPAT(t *testing.T) {
	var mismatch, revoked atomic.Bool
	var calls atomic.Int64
	token, _, err := pat.Mint()
	require.NoError(t, err)
	verify := installationReadAuthenticator(installationVerifierFunc(func(ctx context.Context, raw string) (*pat.Claims, error) {
		if raw != token || revoked.Load() {
			return nil, errors.New("private refusal")
		}
		return &pat.Claims{Source: pat.SourcePAT, UserId: "person", PATID: "key", Role: "owner"}, nil
	}))
	handler := releasecatalog.NewIdentityReadHandler(verify, func(_ context.Context, query string) ([]client.Row, error) {
		calls.Add(1)
		if query == client.CurrentUserBuild(client.CurrentUserArgs{}) {
			id := "v1:identity:user:person"
			if mismatch.Load() {
				id = "v1:identity:user:other"
			}
			return []client.Row{{"id": id}}, nil
		}
		require.Equal(t, client.ReleaseInstallationHealthBuild(client.ReleaseInstallationHealthArgs{Nonce: "fresh"}), query)
		return []client.Row{{"nonce": "fresh", "userId": "person", "identityId": "key", "engineCommit": strings.Repeat("a", 40), "migrationsDigest": "sha256:" + strings.Repeat("b", 64)}}, nil
	})
	s := httptest.NewTLSServer(handler)
	defer s.Close()
	roots := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	endpoint := s.URL + releasewire.ReadPath
	got, err := installationServingProbe(t.Context(), endpoint, token, roots, "fresh")
	require.NoError(t, err)
	require.Equal(t, "key", got.IdentityID)
	require.EqualValues(t, 2, calls.Load())
	mismatch.Store(true)
	_, err = installationServingProbe(t.Context(), endpoint, token, roots, "fresh")
	require.ErrorContains(t, err, "authenticated user differs")
	revoked.Store(true)
	before := calls.Load()
	_, err = installationServingProbe(t.Context(), endpoint, token, roots, "fresh")
	require.Error(t, err)
	require.Equal(t, before, calls.Load())
	_, err = installationServingProbe(t.Context(), s.URL, token, roots, "fresh")
	require.ErrorContains(t, err, "Identity installation-read")
}
