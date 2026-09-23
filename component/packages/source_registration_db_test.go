package packages

import (
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

func TestRepositorySourceRegistrationRestoresAcrossReplicasWithoutChangingDeployables(t *testing.T) {
	_, db := dbEngine(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	first, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	first.Logger = discardLogger()
	if err = first.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	second, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err = second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, stranger := "registration-owner-"+suffix, "registration-other-"+suffix
	seedTiePrincipal(t, first, owner, auth.RoleOwner)
	seedTiePrincipal(t, first, stranger, auth.RoleOwner)
	ctx, other := tieActorCtx(owner, auth.RoleOwner), tieActorCtx(stranger, auth.RoleOwner)
	account := "registration-account-" + suffix
	mustExecute(t, first, ctx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Registration tests")`, langparser.QuoteString(account)))
	sealed, fp, err := secret.Encrypt(grantUserToken)
	if err != nil {
		t.Fatal(err)
	}
	repo := "registration-" + suffix
	hub := installedRepo(newGrantHub(), "acme", repo).body("/user/installations", 200, twoInstallations)
	hub.body("/repos/acme/"+repo, 200, `{"full_name":"acme/`+repo+`","default_branch":"main"}`)
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	engines := []*memql.MemQLEngine{first, second}
	stores := make([]*store, 2)
	integrations := make([]*Integration, 2)
	for n, eng := range engines {
		stores[n] = &store{engine: eng, github: client, directDB: func() *sql.DB { return db.DB }, logger: discardLogger()}
		d := &Deps{Store: stores[n], GitHubApp: client, Credentials: stores[n].resolveCredential, PeekCredentials: stores[n].peekCredential, HTTP: &http.Client{Transport: hub}, Logger: discardLogger()}
		i := NewIntegration(eng, discardLogger())
		i.depsOnce.Do(func() { i.deps = d })
		if err := eng.RegisterIntegration(i); err != nil {
			t.Fatal(err)
		}
		integrations[n] = i
	}
	credentials := []string{"registration-grant-" + suffix, "registration-grant-second-" + suffix}
	bindings := make([]string, 2)
	for n, credential := range credentials {
		mustExecute(t, first, auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation createGithubAppGrant(credentialId: %s, host: "github.com", label: "GitHub", encryptedValue: %s, fingerprint: %s, login: "person", externalId: %s, installationIds: ["42"])`, langparser.QuoteString(credential), langparser.QuoteString(sealed), langparser.QuoteString(fp), langparser.QuoteString(fmt.Sprintf("%s%d", suffix, n))))
		nodes, err := integrations[0].handleSourceConnectionCreate(ctx, map[string]any{"credentialId": credential, "installationId": "42"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		bindings[n] = rowString(replyPayload(t, nodes), "connectionId")
	}
	call := func(credential, binding, ref string) string {
		return fmt.Sprintf(`builtin packageSourceRegister(name: "Source", repoUrl: %s, repoRef: %s, credentialId: %s, sourceConnectionId: %s, accountId: %s)`, langparser.QuoteString("https://github.com/acme/"+repo), langparser.QuoteString(ref), langparser.QuoteString(credential), langparser.QuoteString(binding), langparser.QuoteString(account))
	}
	register := func(eng *memql.MemQLEngine, q string) (map[string]any, error) {
		r, err := eng.Execute(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, n := range r.OutputPayload().(map[string]concept.MemoryNode) {
			return replyPayload(t, []concept.MemoryNode{n}), nil
		}
		return nil, fmt.Errorf("missing registration reply")
	}
	// Separate engine instances start from no shared in-memory state. Exactly one
	// registration wins and all callers receive its stable row ID.
	start := make(chan struct{})
	answers := make(chan map[string]any, 6)
	errs := make(chan error, 6)
	var wg sync.WaitGroup
	for n := 0; n < 6; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			r, err := register(engines[n%2], call(credentials[0], bindings[0], "main"))
			if err != nil {
				errs <- err
			} else {
				answers <- r
			}
		}(n)
	}
	close(start)
	wg.Wait()
	close(answers)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	packageID := ""
	created := 0
	for answer := range answers {
		if rowBool(answer, "created") {
			created++
		}
		id := rowString(answer, "packageId")
		if packageID != "" && packageID != id {
			t.Fatal("replicas duplicated registration")
		}
		packageID = id
	}
	if created != 1 || packageID == "" {
		t.Fatalf("created=%d id=%s", created, packageID)
	}
	// Another identity can independently track the very same repository/ref.
	sibling, err := register(second, call(credentials[1], bindings[1], "main"))
	if err != nil {
		t.Fatal(err)
	}
	siblingID := rowString(sibling, "packageId")
	if siblingID == packageID {
		t.Fatal("different GitHub identities shared a package")
	}
	siteID := "registration-site-" + suffix
	mustExecute(t, first, ctx, fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, kind: "spa", title: "Existing app", bundleRef: "", accountId: %s)`, langparser.QuoteString(siteID), langparser.QuoteString("registration-"+suffix+".example.test"), langparser.QuoteString(account)))
	mustExecute(t, first, ctx, fmt.Sprintf(`insert("v1:platform:site", id=%s, payload={"packageId":%s,"packageDeployableName":"web"})`, langparser.QuoteString(siteID), langparser.QuoteString(packageID)))
	runID := "registration-run-" + suffix
	if err := stores[0].openDeployment(ctx, deploymentSeed{DeploymentId: runID, PackageId: packageID, OwnerUserId: owner, AccountId: account, RequestedBy: owner, SourceVersion: "previous-version", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	beforeSite, err := stores[0].siteById(ctx, siteID)
	if err != nil {
		t.Fatal(err)
	}
	remove := fmt.Sprintf(`mutation setPackageSourceRemoved(packageId: %s, removed: true)`, langparser.QuoteString(packageID))
	for _, q := range []string{remove, fmt.Sprintf(`insert("v1:platform:package", id=%s, payload={"sourceRemoved":true})`, langparser.QuoteString(packageID))} {
		if _, err := first.Execute(other, q); err == nil {
			t.Fatal("foreign owner hid the source")
		}
	}
	mustExecute(t, first, ctx, remove)
	removed, err := stores[1].packageById(ctx, packageID)
	if err != nil || !rowBool(removed, "sourceRemoved") || rowString(removed, "status") != "active" {
		t.Fatalf("removal changed lifecycle: %v %v", removed, err)
	}
	if err := integrations[1].deps.validatePackageSourceConnection(ctx, removed); err != nil {
		t.Fatalf("removed source lost fetch: %v", err)
	}
	afterSite, err := stores[1].siteById(ctx, siteID)
	if err != nil || rowString(afterSite, "status") != rowString(beforeSite, "status") || rowBool(afterSite, "deleted") {
		t.Fatal("removal deactivated app", err)
	}
	if row, err := stores[1].packageById(ctx, siblingID); err != nil || rowBool(row, "sourceRemoved") {
		t.Fatal("removal changed sibling", err)
	}
	for _, credential := range credentials {
		if _, err := stores[1].peekCredential(ctx, credential, owner); err != nil {
			t.Fatal("removal revoked grant", err)
		}
	}
	for _, binding := range bindings {
		if row, err := stores[1].sourceConnectionByID(ctx, binding); err != nil || rowString(row, "status") != "active" {
			t.Fatal("removal changed installation binding", err)
		}
	}
	if history, err := stores[1].deploymentById(ctx, runID); err != nil || rowString(history, "sourceVersion") != "previous-version" {
		t.Fatal("removal changed run history", err)
	}
	// Empty ref follows the provider's default and restores the existing explicit
	// default-branch record without rewriting its name/configuration/history.
	restored, err := register(second, call(credentials[0], bindings[0], ""))
	if err != nil {
		t.Fatal(err)
	}
	if rowString(restored, "packageId") != packageID || !rowBool(restored, "restored") || rowBool(restored, "created") {
		t.Fatalf("re-add replaced source: %v", restored)
	}
	row, err := stores[0].packageById(ctx, packageID)
	if err != nil || rowBool(row, "sourceRemoved") || rowString(row, "repoRef") != "main" {
		t.Fatal("restore changed existing source configuration", err)
	}
}
