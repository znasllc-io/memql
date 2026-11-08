package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/releasecatalog"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/integrations/release"
)

// Exercise the mounted route, generated queries, native release capabilities,
// real signed ledger and result adapter on two independent engines. A stub
// returning []Row cannot reproduce the typed node set returned by a builtin.
// Authentication's fresh/revoked PAT contract is covered separately; only the
// verifier and public trust configuration are fixtures here, never the reads.
func TestIdentityInstallationReadsThroughNativeEngines(t *testing.T) {
	owner := "installation-reader-" + id.NewShortId()
	userID := "v1:identity:user:" + owner
	credentialID := "v1:identity:identity:" + id.NewShortId()
	token, _, err := pat.Mint()
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(owner))
	candidateID := "sha256:" + hex.EncodeToString(sum[:])
	publisher := "reader-" + id.NewShortId()
	record := pl.PublishedRelease{
		FormatVersion: 1, Publisher: publisher, CandidateID: candidateID, ApprovalID: "fixture-approval",
		WorkflowDigest: candidateID, ProvenanceDigest: candidateID, Compatibility: []pl.ReleaseCompatibility{},
		Components: []pl.PublishedComponent{{Name: "engine", Version: "0.31.23", Repository: "fixture/engine", Commit: strings.Repeat("b", 40),
			Artifacts: []pl.PublishedArtifact{{Name: "image", Kind: "oci", Platform: "linux/arm64", Digest: candidateID, Size: 100, ImageDigest: candidateID,
				Locations: []pl.PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "fixture/engine"}}}}}},
	}
	envelope, err := pl.SignPublishedRelease(record, "fixture", private)
	require.NoError(t, err)
	verified, err := pl.VerifyPublishedRelease(envelope, publisher, map[string]ed25519.PublicKey{"fixture": public})
	require.NoError(t, err)
	configuration, err := json.Marshal(map[string]any{"formatVersion": 1, "publisher": publisher, "publicKeys": map[string]string{"fixture": base64.StdEncoding.EncodeToString(public)}})
	require.NoError(t, err)
	var revoked bool
	for replica := 0; replica < 2; replica++ {
		engine, pool := workTemplateDBEngineAndDB(t)
		db, err := memorynodes.NewMemoryNodesDatabase(database.WithDSN(dbtest.DSN()))
		require.NoError(t, err)
		db.DB, db.Bun, db.Logger = pool.DB, pool, engine.Logger
		node := &App{Logger: engine.Logger, engine: engine, db: db, mux: http.NewServeMux()}
		if replica == 0 {
			_, err = pool.ExecContext(t.Context(), `INSERT INTO release_candidates(candidate_id,owner_user_id,manifest,state) VALUES(?,?,'{}','approved')`, candidateID, owner)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, e := pool.Exec(`DELETE FROM release_catalog WHERE candidate_id=?`, candidateID)
				require.NoError(t, e)
				_, e = pool.Exec(`DELETE FROM release_candidates WHERE candidate_id=?`, candidateID)
				require.NoError(t, e)
				_, e = pool.Exec(`DELETE FROM "MemoryNodes" WHERE id=?`, userID)
				require.NoError(t, e)
			})
			_, err = pool.ExecContext(t.Context(), `INSERT INTO release_catalog(candidate_id,publisher,catalog_digest,envelope) VALUES(?,?,?,?)`, candidateID, publisher, verified.Digest(), envelope)
			require.NoError(t, err)
			_, err = pool.ExecContext(t.Context(), `INSERT INTO "MemoryNodes" (id,concept,type,schema,provenance,payload,"createdAt","createdBy") VALUES(?, 'v1:identity:user', 'object', '{}', '{}', '{"active":true,"role":"owner","primaryEmail":"fixture@example.com"}', clock_timestamp(), ?)`, userID, userID)
			require.NoError(t, err)
		}
		pctx := node.pluginContext()
		pctx.ResolveSystemVariable = func(ctx context.Context, name string) (string, error) {
			require.True(t, memql.FreshReadFromContext(ctx))
			require.Equal(t, release.ReleaseCatalogConfigurationVariable, name)
			return string(configuration), nil
		}
		for _, registration := range memql.RegisteredPlugins() {
			if registration.Name == "release" {
				provider, err := registration.Factory(pctx)
				require.NoError(t, err)
				require.NoError(t, engine.RegisterIntegration(provider))
			}
		}
		node.mountIdentityInstallationReads(installationVerifierFunc(func(ctx context.Context, raw string) (*pat.Claims, error) {
			require.True(t, memql.FreshReadFromContext(ctx))
			if revoked || raw != token {
				return nil, pat.ErrPATRevoked
			}
			return &pat.Claims{Source: pat.SourcePAT, UserId: userID, PATID: credentialID, Role: "owner"}, nil
		}))
		server := httptest.NewTLSServer(node.mux)
		t.Cleanup(server.Close)
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		read := func(request releasewire.Request) (map[string]any, error) {
			return releasecatalog.ReadIdentityHTTP(t.Context(), server.URL+releasewire.ReadPath, token, roots, request)
		}
		user, err := read(releasewire.Request{Operation: "current-user"})
		require.NoError(t, err)
		require.Equal(t, owner, memql.BareShortId(user["id"].(string)))
		catalog, err := read(releasewire.Request{Operation: "catalog", CandidateID: candidateID})
		require.NoError(t, err, "replica %d must materialize the native builtin's typed node set", replica)
		require.Equal(t, candidateID, catalog["candidateId"])
		require.Equal(t, verified.Digest(), catalog["catalogDigest"])
		body, err := json.Marshal(catalog["envelope"])
		require.NoError(t, err)
		observed, err := pl.VerifyPublishedRelease(body, publisher, map[string]ed25519.PublicKey{"fixture": public})
		require.NoError(t, err)
		require.Equal(t, verified.Digest(), observed.Digest())
		page, err := read(releasewire.Request{Operation: "list", Limit: 1})
		require.NoError(t, err)
		releases := page["releases"].([]any)
		require.Len(t, releases, 1)
		require.Equal(t, candidateID, releases[0].(map[string]any)["candidateId"])
		health, err := read(releasewire.Request{Operation: "health", Nonce: "fresh-" + id.NewShortId()})
		if len(buildinfo.Commit()) == 40 {
			require.NoError(t, err)
			require.Equal(t, buildinfo.Commit(), health["engineCommit"])
			require.Equal(t, owner, health["userId"])
			require.Equal(t, memql.BareShortId(credentialID), health["identityId"])
			migrations, err := node.installationMigrationHealth(t.Context())
			require.NoError(t, err)
			require.Equal(t, migrations, health["migrationsDigest"])
		} else {
			require.Error(t, err, "an unstamped test binary must not assert release health")
		}
		revoked = true
		_, err = read(releasewire.Request{Operation: "catalog", CandidateID: candidateID})
		require.Error(t, err)
		revoked = false
	}
}
