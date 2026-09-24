package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

func TestGitHubRefreshAcrossReplicasSpendsRotatingTokenOnce(t *testing.T) {
	first, db := dbEngine(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	second, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err = second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := "v1:identity:user:refresh-owner-" + suffix
	credential := "v1:platform:sourceCredential:refresh-" + suffix
	ctx := deployerCtx(owner)
	sealed, fingerprint, err := secret.Encrypt("old-user-token")
	if err != nil {
		t.Fatal(err)
	}
	refresh, _, err := secret.Encrypt("single-use-refresh")
	if err != nil {
		t.Fatal(err)
	}
	mustExecute(t, first, auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation createGithubAppGrant(credentialId: %s, host: "github.com", label: "Replica test", encryptedValue: %s, fingerprint: %s, refreshToken: %s, expiresAt: %s, login: "replica-user", externalId: %s, installationIds: [])`, langparser.QuoteString(credential), langparser.QuoteString(sealed), langparser.QuoteString(fingerprint), langparser.QuoteString(refresh), langparser.QuoteString(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)), langparser.QuoteString(suffix)))
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", credential).Exec(context.Background())
	})
	hub := newGrantHub().body("/login/oauth/access_token", http.StatusOK, `{"access_token":"renewed-user-token","expires_in":28800,"refresh_token":"rotated-refresh-token"}`)
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}), githubapp.WithOAuthBase("https://github.com"))
	stores := []*store{{engine: first, github: client, directDB: func() *sql.DB { return db.DB }}, {engine: second, github: client, directDB: func() *sql.DB { return db.DB }}}
	start := make(chan struct{})
	errs := make(chan error, 6)
	var wg sync.WaitGroup
	for n := 0; n < 6; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			grant, err := stores[n%2].peekCredential(ctx, credential, owner)
			if err != nil {
				errs <- err
				return
			}
			if grant.Bearer != "renewed-user-token" {
				errs <- fmt.Errorf("replica received stale bearer")
			}
		}(n)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := hub.hits("/login/oauth/access_token"); got != 1 {
		t.Fatalf("single-use refresh token spent %d times", got)
	}
	row, err := stores[1].sourceCredentialSealedById(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := secret.Decrypt(rowString(row, "refreshToken"))
	if err != nil || rotated != "rotated-refresh-token" {
		t.Fatal("receiving replica cannot read persisted rotated refresh token")
	}
	// A targeted reconnect captured before disconnect must not reactivate the
	// row, even when it completes on another replica after token rotation.
	callback := &identity.Store{Engine: second, DirectDB: func() *sql.DB { return db.DB }}
	target, err := callback.GithubReconnectTarget(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores[0].revokeSourceCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	_, _, err = callback.UpsertGithubAppGrant(ctx, identity.GithubAppGrant{
		OwnerUserId: owner, ExternalId: suffix, TargetCredentialId: credential, TargetRevokedAt: target.RevokedAt,
		EncryptedValue: sealed, Fingerprint: fingerprint, Login: "replica-user",
	})
	if !errors.Is(err, identity.ErrGithubReconnectCancelled) {
		t.Fatalf("late reconnect was not cancelled: %v", err)
	}
	row, err = stores[1].sourceCredentialSealedById(ctx, credential)
	if err != nil || rowString(row, "status") != credentialStatusRevoked {
		t.Fatal("late callback reactivated disconnected grant")
	}
}

// The begin-side store and receiving callback-side store share only persisted
// graph rows and Postgres locks; no browser flow state lives on either engine.
func TestGitHubConnectStateSurvivesReplicaHopAndConsumesOnce(t *testing.T) {
	first, db := dbEngine(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	second, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err = second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	verifier, _, err := secret.Encrypt("replica-verifier")
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	replicas := []*identity.Store{{Engine: first, DirectDB: func() *sql.DB { return db.DB }}, {Engine: second, DirectDB: func() *sql.DB { return db.DB }}}
	hash := identity.HashConnectState("hop-" + suffix)
	id, err := replicas[0].CreateGithubConnectState(context.Background(), identity.GithubConnectStateSeed{
		UserId: "hop-user-" + suffix, StateHash: hash, ExpiresAt: time.Now().Add(time.Minute), SessionId: "hop-session", PKCEVerifier: verifier, CredentialId: "selected-grant", ExpectedExternalId: "5150", FlowId: "browser-flow",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
	})
	row, err := replicas[1].LookupGithubConnectState(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.SessionId != "hop-session" || row.CredentialId != "selected-grant" || row.ExpectedExternalId != "5150" || row.FlowId != "browser-flow" {
		t.Fatal("receiving replica lost persistent flow binding")
	}
	plain, err := secret.Decrypt(row.PKCEVerifier)
	if err != nil || plain != "replica-verifier" {
		t.Fatal("receiving replica cannot recover sealed PKCE")
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for _, store := range replicas {
		wg.Add(1)
		go func(store *identity.Store) {
			defer wg.Done()
			<-start
			if _, err := store.ConsumeGithubConnectState(context.Background(), hash, ""); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, identity.ErrGithubConnectStateAlreadyConsumed) {
				t.Error(err)
			}
		}(store)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("callback state consumed %d times", wins.Load())
	}
}
