package packages

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

func TestSourceConnectionsPersistOwnedBindingsAndPreserveExistingPackages(t *testing.T) {
	eng, db := dbEngine(t)
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	a, b := "source-owner-"+suffix, "source-stranger-"+suffix
	seedTiePrincipal(t, eng, a, auth.RoleOwner)
	seedTiePrincipal(t, eng, b, auth.RoleOwner)
	ctx, other := tieActorCtx(a, auth.RoleOwner), tieActorCtx(b, auth.RoleOwner)
	account, credential := "source-account-"+suffix, "source-grant-"+suffix
	mustExecute(t, eng, ctx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Source test account")`, langparser.QuoteString(account)))
	sealed, fingerprint, err := secret.Encrypt(grantUserToken)
	if err != nil {
		t.Fatal(err)
	}
	mustExecute(t, eng, auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation createGithubAppGrant(credentialId: %s, host: "github.com", label: "Personal GitHub", encryptedValue: %s, fingerprint: %s, login: "alice", externalId: "100", installationIds: ["42","43"])`, langparser.QuoteString(credential), langparser.QuoteString(sealed), langparser.QuoteString(fingerprint)))
	hub := installedRepo(newGrantHub(), "acme", "repo-"+suffix).body("/user/installations", http.StatusOK, twoInstallations)
	hub.body("/repos/alice/other/installation", 200, `{"id":43}`)
	hub.body("/repos/alice/other", 200, `{"full_name":"alice/other"}`)
	hub.body("/user/installations/42/repositories", 200, `{"total_count":0,"repositories":[]}`)
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	s := &store{engine: eng, logger: discardLogger(), github: client}
	deps := &Deps{Store: s, GitHubApp: client, Credentials: s.resolveCredential, PeekCredentials: s.peekCredential, HTTP: &http.Client{Transport: hub}, Logger: discardLogger()}
	integration := NewIntegration(eng, discardLogger())
	integration.depsOnce.Do(func() { integration.deps = deps })
	if err := eng.RegisterIntegration(integration); err != nil {
		t.Fatal(err)
	}
	create := func(actor context.Context, installation string) (string, error) {
		result, err := eng.Execute(actor, fmt.Sprintf(`builtin sourceConnectionCreate(credentialId: %s, installationId: %s)`, langparser.QuoteString(credential), langparser.QuoteString(installation)))
		if err != nil {
			return "", err
		}
		nodes, ok := result.OutputPayload().(map[string]memorynodes.MemoryNode)
		if !ok || len(nodes) != 1 {
			return "", fmt.Errorf("unexpected connection reply: %T", result.OutputPayload())
		}
		for _, node := range nodes {
			return rowString(replyPayload(t, []memorynodes.MemoryNode{node}), "connectionId"), nil
		}
		return "", fmt.Errorf("empty connection reply")
	}
	first, err := create(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	again, err := create(ctx, "42")
	if err != nil || first != again {
		t.Fatalf("idempotent connection: %q %q %v", first, again, err)
	}
	// A second engine has none of the creating replica's local lookup state.
	receiver, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Logger = discardLogger()
	if err := receiver.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	receiverStore := &store{engine: receiver}
	if row, err := receiverStore.sourceConnectionByID(ctx, first); err != nil || rowString(row, "status") != "active" {
		t.Fatalf("second replica could not read persisted connection: %v %v", row, err)
	}
	second, err := create(ctx, "43")
	if err != nil || second == first {
		t.Fatalf("second installation: %q %v", second, err)
	}
	row, err := s.sourceConnectionByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !sameShortId(rowString(row, "ownerUserId"), a) || !sameShortId(rowString(row, "credentialId"), credential) || rowString(row, "providerAccountId") != "100" || rowString(row, "accountLogin") != "acme" {
		t.Fatalf("persisted binding: %v", row)
	}
	if got := mustExecute(t, eng, other, `query sourceConnectionsMine()`); len(got) != 0 {
		t.Fatalf("other owner read personal connections: %v", got)
	}
	if _, err := create(other, "42"); err == nil {
		t.Fatal("another owner registered the first owner's credential")
	}
	// The same installation may be selected independently by another person
	// using their own grant. Their stable connection IDs must remain distinct.
	otherCredential := credential + "-other"
	mustExecute(t, eng, auth.ContextWithInternalOrigin(other), fmt.Sprintf(`mutation createGithubAppGrant(credentialId: %s, host: "github.com", label: "Other GitHub", encryptedValue: %s, fingerprint: %s, login: "bob", externalId: "101", installationIds: ["42"])`, langparser.QuoteString(otherCredential), langparser.QuoteString(sealed), langparser.QuoteString(fingerprint)))
	otherNodes, err := integration.handleSourceConnectionCreate(other, map[string]any{"credentialId": otherCredential, "installationId": "42"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if otherID := rowString(replyPayload(t, otherNodes), "connectionId"); otherID == first || otherID == "" {
		t.Fatalf("owners shared a binding ID: %q", otherID)
	}
	if _, err := eng.Execute(other, fmt.Sprintf(`builtin sourceConnectionRemove(connectionId: %s)`, langparser.QuoteString(first))); err == nil {
		t.Fatal("another owner removed a connection")
	}
	if _, err := eng.Execute(ctx, fmt.Sprintf(`insert("v1:platform:sourceConnection", id=%s, payload={"installationId":"999"})`, langparser.QuoteString(first))); err == nil {
		t.Fatal("raw write forged a verified source")
	}
	packageID := "source-package-" + suffix
	repoURL := "https://github.com/acme/repo-" + suffix
	packageCall := func(id, repo, binding, cred string) string {
		return fmt.Sprintf(`mutation createPackage(packageId: %s, name: "Bound package", sourceKind: "repo", repoUrl: %s, credentialId: %s, sourceConnectionId: %s, accountId: %s)`, langparser.QuoteString(id), langparser.QuoteString(repo), langparser.QuoteString(cred), langparser.QuoteString(binding), langparser.QuoteString(account))
	}
	mustExecute(t, eng, ctx, packageCall(packageID, repoURL, first, credential))
	pkg, err := s.packageById(ctx, packageID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameShortId(rowString(pkg, "sourceConnectionId"), first) || !sameShortId(rowString(pkg, "ownerUserId"), a) || !sameShortId(rowString(pkg, "accountId"), account) {
		t.Fatalf("package attribution: %v", pkg)
	}
	for _, tc := range []struct{ name, repo, binding, cred string }{
		{"foreign installation", "https://github.com/alice/other", first, credential},
		{"wrong grant", repoURL, first, "another-grant"},
		{"missing source", repoURL, "missing-source", credential},
	} {
		if _, err := eng.Execute(ctx, packageCall(packageID+"-"+strings.ReplaceAll(tc.name, " ", "-"), tc.repo, tc.binding, tc.cred)); err == nil {
			t.Fatalf("created package with %s", tc.name)
		}
	}
	mustExecute(t, eng, ctx, fmt.Sprintf(`builtin sourceConnectionRemove(connectionId: %s)`, langparser.QuoteString(first)))
	if row, err := receiverStore.sourceConnectionByID(ctx, first); err != nil || rowString(row, "status") != "removed" {
		t.Fatalf("second replica retained stale active binding: %v %v", row, err)
	}
	if _, err := eng.Execute(ctx, fmt.Sprintf(`builtin sourceRepositories(connectionId: %s)`, langparser.QuoteString(first))); err == nil {
		t.Fatal("removed connection listed repositories")
	}
	if _, err := eng.Execute(ctx, packageCall(packageID+"-removed", repoURL, first, credential)); err == nil {
		t.Fatal("removed connection accepted new package")
	}
	// Removal leaves the grant, sibling source, and persisted package intact.
	if row, err := s.sourceConnectionByID(ctx, second); err != nil || rowString(row, "status") != "active" {
		t.Fatalf("sibling changed: %v %v", row, err)
	}
	if _, err := s.peekCredential(ctx, credential, a); err != nil {
		t.Fatalf("removal revoked shared grant: %v", err)
	}
	mustExecute(t, eng, ctx, fmt.Sprintf(`mutation updatePackageSource(packageId: %s, repoRef: "main")`, langparser.QuoteString(packageID)))
	pkg, err = s.packageById(ctx, packageID)
	if err != nil || rowString(pkg, "repoRef") != "main" {
		t.Fatalf("existing package changed: %v %v", pkg, err)
	}
	rc, err := s.peekCredential(ctx, credential, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installationBearer(ctx, client, rc, "acme", "repo-"+suffix); err != nil {
		t.Fatalf("existing package lost fetch after source removal: %v", err)
	}
	if err := deps.validatePackageSourceConnection(ctx, pkg); err != nil {
		t.Fatalf("existing package lost removed binding: %v", err)
	}
	// A repository transfer must not silently change the selected installation.
	hub.body("/repos/acme/repo-"+suffix+"/installation", 200, `{"id":43}`)
	if err := deps.validatePackageSourceConnection(ctx, pkg); RefusalCode(err) != "source_repository_mismatch" {
		t.Fatalf("transferred repository escaped source: %v", err)
	}
	hub.body("/repos/acme/repo-"+suffix+"/installation", 200, `{"id":42}`)
	restored, err := create(ctx, "42")
	if err != nil || !sameShortId(restored, first) {
		t.Fatalf("reactivation lost stable identity: %s %v", restored, err)
	}
	if memql.BareShortId(rowString(pkg, "credentialId")) != credential {
		t.Fatal("package credential changed")
	}
}
