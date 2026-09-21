package identity

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// githubapp_test.go -- the three capabilities a cluster's GitHub App is
// registered through (design record 2026-09-20-github-app-setup, D3).
//
// The properties that matter are all about WHO and WHAT NOT: who may begin and
// remove, which tier the product may write over, and that no answer carries a
// credential. Each refusal is asserted with the engine untouched, because a
// refusal that still wrote a state row is a refusal in the reply only.

func appCtx(role componentAuth.Role, userId string) context.Context {
	return componentAuth.ContextWithAccess(context.Background(), &componentAuth.AccessContext{
		UserId:       userId,
		PrimaryEmail: userId + "@example.test",
		Role:         role,
	})
}

func appReply(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("capability: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d result nodes, want exactly 1", len(nodes))
	}
	var out map[string]any
	if jerr := json.Unmarshal(nodes[0].Payload, &out); jerr != nil {
		t.Fatalf("decode reply: %v", jerr)
	}
	return out
}

func appStatus(t *testing.T, i *IdentityIntegration, ctx context.Context) map[string]any {
	t.Helper()
	nodes, err := i.handleGithubAppStatus(ctx, nil, 0)
	return appReply(t, nodes, err)
}

func appBegin(t *testing.T, i *IdentityIntegration, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	nodes, err := i.handleGithubAppSetupBegin(ctx, args, 0)
	return appReply(t, nodes, err)
}

func appRemove(t *testing.T, i *IdentityIntegration, ctx context.Context) map[string]any {
	t.Helper()
	nodes, err := i.handleGithubAppRemove(ctx, nil, 0)
	return appReply(t, nodes, err)
}

func newAppIntegration(rows *githubconnect.RowReader) (*IdentityIntegration, *beginFakeEngine) {
	eng := &beginFakeEngine{}
	i := NewIdentityIntegrationWithEngine(eng, nil, nil)
	if rows != nil {
		i.SetGitHubAppRows(*rows)
	}
	return i, eng
}

func (f *beginFakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.statements)
}

// ---------------------------------------------------------------------------
// githubAppStatus
// ---------------------------------------------------------------------------

func TestStatusTellsAnOwnerTheyCanSetUpAnAppThatIsNotThere(t *testing.T) {
	unconfigureApp(t)
	i, eng := newAppIntegration(nil)
	reply := appStatus(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"))
	if reply["configured"] != false || reply["source"] != "" || reply["canSetup"] != true {
		t.Errorf("reply = %v", reply)
	}
	if reply["slug"] != "" || reply["installUrl"] != "" {
		t.Errorf("no app, and a slug or an install link anyway: %v", reply)
	}
	if eng.count() != 0 {
		t.Errorf("a status read wrote %d statement(s)", eng.count())
	}
}

// Anybody signed in may ASK -- that is the point, so the Source stop knows
// before it offers Connect -- and only an owner is told they could act.
func TestStatusTellsEverybodyElseTheyCannot(t *testing.T) {
	unconfigureApp(t)
	for _, role := range []componentAuth.Role{componentAuth.RoleDeveloper, componentAuth.RoleAdmin, componentAuth.RoleWriter, componentAuth.RoleReader} {
		i, _ := newAppIntegration(nil)
		reply := appStatus(t, i, appCtx(role, "v1:identity:user:somebody"))
		if reply["canSetup"] != false {
			t.Errorf("%s was told they could register the cluster's app", role)
		}
	}
}

func TestStatusNamesAnAppRegisteredFromTheProduct(t *testing.T) {
	unconfigureApp(t)
	rows := storedAppRows()
	i, _ := newAppIntegration(&rows)
	reply := appStatus(t, i, appCtx(componentAuth.RoleWriter, "v1:identity:user:somebody"))
	if reply["configured"] != true || reply["source"] != "cluster" {
		t.Fatalf("reply = %v", reply)
	}
	if reply["slug"] != "registered-from-the-product" || reply["installUrl"] != "https://github.com/apps/registered-from-the-product/installations/new" {
		t.Errorf("reply = %v", reply)
	}
}

// TestStatusSeesAnAppAnotherNodeRegisteredAMomentAgo. The registration is
// written by the IDENTITY node's callback, a different process from the one a
// surface asks, so this node's resolver is never told. A status answered from
// its ten-second memory tells the owner who has just come back from GitHub
// that there is no app -- and a surface asks once and believes the answer. So
// the rows change UNDER a memory that is still fresh, and the status has to see
// it; and what it read has to be what githubConnectBegin then finds, because
// Connect is the very next thing that owner presses.
func TestStatusSeesAnAppAnotherNodeRegisteredAMomentAgo(t *testing.T) {
	unconfigureApp(t)
	stored := storedAppRows()
	registered := false
	gate := func(read func(context.Context, string) (string, error)) func(context.Context, string) (string, error) {
		return func(ctx context.Context, name string) (string, error) {
			if !registered {
				return "", errors.New("not found")
			}
			return read(ctx, name)
		}
	}
	rows := githubconnect.RowReader{Variable: gate(stored.Variable), Secret: gate(stored.Secret)}
	i, _ := newAppIntegration(&rows)
	owner := appCtx(componentAuth.RoleOwner, "v1:identity:user:owner")

	if reply := appStatus(t, i, owner); reply["configured"] != false {
		t.Fatalf("control: before the registration the reply = %v", reply)
	}
	// THE CONTROL THAT MAKES THIS A TEST OF THE MEMORY: asked the ordinary way,
	// the resolver still remembers "no app" after the rows change.
	registered = true
	if cfg, _ := i.githubAppConfig(owner); cfg.Configured() {
		t.Fatal("control: the resolver read again inside its TTL, so this test cannot tell a fresh status from a remembered one")
	}

	reply := appStatus(t, i, owner)
	if reply["configured"] != true || reply["source"] != "cluster" {
		t.Fatalf("the status answered from memory: %v", reply)
	}
	if cfg, source := i.githubAppConfig(owner); !cfg.Configured() || source != githubconnect.SourceCluster {
		t.Errorf("the status did not leave what it read for the begin that follows: configured=%v source=%q", cfg.Configured(), source)
	}
}

// TestStatusAnswersNoCredential, with its control: the credentials really were
// resolvable on this node, so "absent from the reply" is a property of the
// reply and not of an integration that read nothing.
func TestStatusAnswersNoCredential(t *testing.T) {
	unconfigureApp(t)
	rows := storedAppRows()
	i, _ := newAppIntegration(&rows)
	nodes, err := i.handleGithubAppStatus(appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(nodes[0].Payload)
	cfg, _ := i.githubAppConfig(context.Background())
	if cfg.ClientSecret == "" || cfg.PrivateKeyB64 == "" || cfg.WebhookSecret == "" {
		t.Fatal("the credentials were not resolvable, so the scan below would prove nothing")
	}
	for name, value := range map[string]string{
		"client secret": cfg.ClientSecret, "private key": cfg.PrivateKeyB64, "webhook secret": cfg.WebhookSecret,
		"app id": cfg.AppID, "client id": cfg.ClientID,
	} {
		if strings.Contains(raw, value) {
			t.Errorf("the status reply carries the %s:\n  %s", name, raw)
		}
	}
}

// The environment's app is the OPERATOR's: shown, never offered for change.
func TestStatusOffersNothingOverAnAppTheEnvironmentSet(t *testing.T) {
	configureApp(t)
	i, _ := newAppIntegration(nil)
	reply := appStatus(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"))
	if reply["configured"] != true || reply["source"] != "environment" || reply["canSetup"] != false {
		t.Errorf("reply = %v", reply)
	}
}

// One environment value is a statement about this cluster's app. There is no
// app -- and nothing to set up from here either, because rows would be ignored.
func TestStatusOverAPartialEnvironmentIsNoAppAndNoOffer(t *testing.T) {
	unconfigureApp(t)
	t.Setenv(githubconnect.EnvAppID, "123456")
	rows := storedAppRows()
	i, _ := newAppIntegration(&rows)
	reply := appStatus(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"))
	if reply["configured"] != false || reply["source"] != "" || reply["canSetup"] != false {
		t.Errorf("reply = %v", reply)
	}
}

func TestStatusRefusesAnEmptyActor(t *testing.T) {
	unconfigureApp(t)
	i, _ := newAppIntegration(nil)
	if _, err := i.handleGithubAppStatus(context.Background(), nil, 0); err == nil {
		t.Error("a call with no actor was answered")
	}
}

// ---------------------------------------------------------------------------
// githubAppSetupBegin
// ---------------------------------------------------------------------------

func TestBeginMintsASetupStateForAnOwner(t *testing.T) {
	unconfigureApp(t)
	i, eng := newAppIntegration(nil)
	reply := appBegin(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"),
		map[string]any{"returnPath": "/?connect=deployables", "organization": "znasllc-io"})
	if reply["reason"] != "ok" {
		t.Fatalf("reason = %v", reply["reason"])
	}
	startURL, _ := reply["startUrl"].(string)
	if !strings.HasPrefix(startURL, "https://identity.example.test/auth/github/app/new?state=") {
		t.Fatalf("startUrl = %q", startURL)
	}

	write := eng.statement(t, "mutation createGithubConnectState(")
	if _, err := langparser.ParseExpression(write); err != nil {
		t.Fatalf("the engine could not parse the state write: %v\n  %s", err, write)
	}
	for _, want := range []string{
		`userId: "v1:identity:user:owner"`,
		`purpose: "app_setup"`,
		`organization: "znasllc-io"`,
		`returnPath: "/?connect=deployables"`,
	} {
		if !strings.Contains(write, want) {
			t.Errorf("the state row is missing %s:\n  %s", want, write)
		}
	}
	// ONLY THE DIGEST IS STORED, exactly as for a connect state.
	plaintext := regexp.MustCompile(`[?&]state=([^&]+)`).FindStringSubmatch(startURL)[1]
	if strings.Contains(write, plaintext) {
		t.Errorf("the PLAINTEXT state was written to the row:\n  %s", write)
	}
	if !strings.Contains(write, `stateHash: "`+componentIdentity.HashConnectState(plaintext)+`"`) {
		t.Errorf("the row does not carry the digest of the state in the URL:\n  %s", write)
	}
	// THE MANIFEST IS NOT HERE. Nothing the browser receives describes the app.
	if strings.Contains(startURL, "manifest") || strings.Contains(startURL, "permissions") {
		t.Errorf("the start URL carries part of the manifest: %q", startURL)
	}

	audit := eng.statement(t, "mutation createAuditEvent(")
	if !strings.Contains(audit, `action:"github_app_setup_begun"`) || !strings.Contains(audit, `targetType:"config"`) || !strings.Contains(audit, `category:"configuration"`) {
		t.Errorf("audit = %s", audit)
	}
}

// TestOnlyAClusterOwnerMayBeginOrRemove. The app is the deployment's: every
// person's grant is made against it. A refusal that still wrote something is a
// refusal in the reply only, so the engine is asserted untouched.
func TestOnlyAClusterOwnerMayBeginOrRemove(t *testing.T) {
	unconfigureApp(t)
	for _, role := range []componentAuth.Role{componentAuth.RoleDeveloper, componentAuth.RoleAdmin, componentAuth.RoleWriter, componentAuth.RoleReader} {
		rows := storedAppRows()
		i, eng := newAppIntegration(&rows)
		ctx := appCtx(role, "v1:identity:user:somebody")

		begun := appBegin(t, i, ctx, map[string]any{})
		if begun["reason"] != "github_app_setup_forbidden" || begun["startUrl"] != "" {
			t.Errorf("%s began a registration: %v", role, begun)
		}
		removed := appRemove(t, i, ctx)
		if removed["reason"] != "github_app_setup_forbidden" || removed["removed"] != false {
			t.Errorf("%s removed the cluster's app: %v", role, removed)
		}
		if eng.count() != 0 {
			t.Errorf("%s was refused and the engine was written to anyway: %d statement(s)", role, eng.count())
		}
	}
}

func TestTheProductDoesNotWriteOverTheEnvironmentsApp(t *testing.T) {
	configureApp(t)
	i, eng := newAppIntegration(nil)
	ctx := appCtx(componentAuth.RoleOwner, "v1:identity:user:owner")
	if reply := appBegin(t, i, ctx, map[string]any{}); reply["reason"] != "github_app_managed_by_environment" {
		t.Errorf("begin: %v", reply)
	}
	if reply := appRemove(t, i, ctx); reply["reason"] != "github_app_managed_by_environment" {
		t.Errorf("remove: %v", reply)
	}
	if eng.count() != 0 {
		t.Errorf("%d statement(s) were written", eng.count())
	}
}

func TestAnOrganizationThatIsNotALoginIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	unconfigureApp(t)
	for _, org := range []string{"../settings", "acme/evil", "acme?x=1", "-acme"} {
		i, eng := newAppIntegration(nil)
		reply := appBegin(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"), map[string]any{"organization": org})
		if reply["reason"] != "github_app_setup_invalid" || eng.count() != 0 {
			t.Errorf("organization %q: reply=%v statements=%d", org, reply, eng.count())
		}
	}
}

func TestBeginRefusesAnEmptyActor(t *testing.T) {
	unconfigureApp(t)
	i, eng := newAppIntegration(nil)
	if _, err := i.handleGithubAppSetupBegin(context.Background(), map[string]any{}, 0); err == nil {
		t.Error("a registration was begun by nobody")
	}
	if eng.count() != 0 {
		t.Errorf("%d statement(s) were written", eng.count())
	}
}

// ---------------------------------------------------------------------------
// githubAppRemove
// ---------------------------------------------------------------------------

func TestRemoveClearsTheSixAndSaysSo(t *testing.T) {
	unconfigureApp(t)
	rows := storedAppRows()
	i, eng := newAppIntegration(&rows)
	reply := appRemove(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"))
	if reply["removed"] != true || reply["reason"] != "ok" {
		t.Fatalf("reply = %v", reply)
	}
	eng.mu.Lock()
	var clears int
	for _, q := range eng.statements {
		if (strings.HasPrefix(q, "mutation setGlobalVariable(") || strings.HasPrefix(q, "mutation setGlobalSecret(")) && strings.Contains(q, "active: false") {
			clears++
		}
	}
	eng.mu.Unlock()
	if clears != 6 {
		t.Errorf("%d rows cleared, want six", clears)
	}
	if audit := eng.statement(t, "mutation createAuditEvent("); !strings.Contains(audit, `action:"github_app_removed"`) {
		t.Errorf("audit = %s", audit)
	}
	// The `githubApp` readiness module just changed its answer.
	eng.statement(t, "builtin readinessRecompute()")
}

func TestRemoveWithNothingRegisteredSaysSoAndWritesNothing(t *testing.T) {
	unconfigureApp(t)
	i, eng := newAppIntegration(nil)
	reply := appRemove(t, i, appCtx(componentAuth.RoleOwner, "v1:identity:user:owner"))
	if reply["removed"] != false || reply["reason"] != "github_app_not_configured" {
		t.Errorf("reply = %v", reply)
	}
	if eng.count() != 0 {
		t.Errorf("%d statement(s) were written", eng.count())
	}
}

// TestTheThreeCapabilitiesAreRegistered. A builtin whose executor names a
// capability the provider does not register is a BOOT ERROR
// (integration_executor_audit.go), so this fails here rather than on a node.
func TestTheThreeCapabilitiesAreRegistered(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range NewIdentityIntegration().Capabilities() {
		if c.Handler != nil {
			declared[c.Name] = true
		}
	}
	for _, name := range []string{"githubAppStatus", "githubAppSetupBegin", "githubAppRemove"} {
		if !declared[name] {
			t.Errorf("%s is declared in dsl/identity/builtins.memql and not registered here", name)
		}
	}
}
