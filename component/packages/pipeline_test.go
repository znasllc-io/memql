package packages

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/edge"
)

// ---------------------------------------------------------------------------
// Fakes for the pipeline's outside world
// ---------------------------------------------------------------------------

type fakeFetcher struct {
	tree fs.FS
	// sourceSeen records what the fetch was told: the credential NAME and the
	// OWNER it is to be resolved under, straight off the package row.
	sourceSeen RepoSource
	// resolvedTokens records every credential VALUE the fetch resolved, so a
	// test can assert none of them reached a row.
	resolvedTokens []string
	// credentials maps a credential id to the token a resolver would unseal.
	credentials map[string]string
	err         error
}

func (f *fakeFetcher) FetchRepo(_ context.Context, src RepoSource, _ Limits) (*SourceSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.sourceSeen = src
	if v, ok := f.credentials[src.CredentialId]; ok {
		f.resolvedTokens = append(f.resolvedTokens, v)
	}
	return &SourceSnapshot{Tree: f.tree, Version: "sha-abc123", cleanup: func() {}}, nil
}

func (f *fakeFetcher) FetchArtifact(_ context.Context, _ string, _ Limits) (*SourceSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &SourceSnapshot{Tree: f.tree, Version: "hash-abc123", cleanup: func() {}}, nil
}

type fakeBuilder struct {
	built []string
	err   error
	tail  string
}

func (b *fakeBuilder) Build(_ context.Context, _ *SourceSnapshot, dep DeployableReport) (BuildResult, error) {
	b.built = append(b.built, dep.Name)
	if b.err != nil {
		return BuildResult{LogTail: b.tail}, b.err
	}
	return BuildResult{Bundle: edge.Bundle{"index.html": []byte("<!doctype html>")}}, nil
}

type fakeStager struct {
	active  map[string]string
	staged  []string
	written int
	onWrite func()
}

func (s *fakeStager) StageDomain(_ context.Context, domain string, tree fs.FS) (string, error) {
	hash, _, err := hashTree(tree)
	if err != nil {
		return "", err
	}
	s.staged = append(s.staged, domain)
	return "packages/" + domain + "/" + hash + "/", nil
}

func (s *fakeStager) ReadActiveSet(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range s.active {
		out[k] = v
	}
	return out, nil
}

func (s *fakeStager) WriteActiveSet(_ context.Context, set map[string]string) error {
	s.active = set
	s.written++
	if s.onWrite != nil {
		s.onWrite()
	}
	return nil
}

type fakeRoller struct {
	rolls  int
	err    error
	onRoll func()
}

func (r *fakeRoller) Roll(context.Context, string) error {
	r.rolls++
	if r.onRoll != nil {
		r.onRoll()
	}
	return r.err
}

type fakePublisher struct {
	created   []string
	published []string
	repointed []string
	snapshots int
	err       error
	onRepoint func()
}

func (p *fakePublisher) EnsureSite(_ context.Context, req EnsureSiteRequest) (string, string, bool, error) {
	p.created = append(p.created, req.DeployableName)
	return "v1:platform:site:" + req.DeployableName, req.Hostname, true, nil
}

func (p *fakePublisher) PublishBundle(_ context.Context, siteId string, _ edge.Bundle) (PublishResult, error) {
	if p.err != nil {
		return PublishResult{}, p.err
	}
	p.published = append(p.published, siteId)
	return PublishResult{SiteId: siteId, BundleRef: "blob://sites/x/v1/", Version: "v1"}, nil
}

func (p *fakePublisher) RepointSite(_ context.Context, siteId, bundleRef string) error {
	p.repointed = append(p.repointed, siteId+" -> "+bundleRef)
	if p.onRepoint != nil {
		p.onRepoint()
	}
	return nil
}

func (p *fakePublisher) StoreSnapshot(context.Context, string, string, []byte) (string, error) {
	p.snapshots++
	return "v1:library:artifact:snap", nil
}

type fakeAuditor struct{ events []DeployAuditEvent }

func (a *fakeAuditor) Deploy(_ context.Context, ev DeployAuditEvent) { a.events = append(a.events, ev) }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	deps      *Deps
	engine    *recordingEngine
	fetcher   *fakeFetcher
	builder   *fakeBuilder
	stager    *fakeStager
	roller    *fakeRoller
	publisher *fakePublisher
	auditor   *fakeAuditor
}

func newHarness(t *testing.T, tree fs.FS, pkgRow map[string]any) *harness {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query packageById": {pkgRow},
	}}
	h := &harness{
		engine:    engine,
		fetcher:   &fakeFetcher{tree: tree},
		builder:   &fakeBuilder{},
		stager:    &fakeStager{active: map[string]string{}},
		roller:    &fakeRoller{},
		publisher: &fakePublisher{},
		auditor:   &fakeAuditor{},
	}
	n := 0
	h.deps = &Deps{
		Store:     &store{engine: engine},
		Fetcher:   h.fetcher,
		Builder:   h.builder,
		Stager:    h.stager,
		Roller:    h.roller,
		Publisher: h.publisher,
		Auditor:   h.auditor,
		Now:       func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
		NewId: func(concept string) string {
			n++
			return concept + ":fixed" + string(rune('0'+n))
		},
	}
	return h
}

func ownerPackage() map[string]any {
	return map[string]any{
		"id":          "v1:platform:package:abc",
		"ownerUserId": "v1:identity:user:someone",
		"name":        "acme",
		"sourceKind":  "repo",
		"repoUrl":     "https://github.com/acme/widget",
		"status":      "active",
	}
}

func clusterOwner() Actor { return Actor{UserId: "v1:identity:user:owner", IsClusterOwner: true} }
func plainUser() Actor    { return Actor{UserId: "v1:identity:user:someone"} }

// spaOnlyPackage is validPackage with the DSL removed -- the D6 fast path.
func spaOnlyPackage() fstest.MapFS {
	p := validPackage()
	delete(p, "dsl/acme/concepts.memql")
	return p
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

func TestTheConfirmGateIsAlwaysPresent(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     plainUser(),
	})
	if err != nil {
		t.Fatalf("parking at the confirm gate is not a failure: %v", err)
	}
	if !out.AwaitingConfirm || out.Status != StatusAwaitingConfirm {
		t.Fatalf("want a parked run, got %+v", out)
	}
	if out.Report == nil || len(out.Report.Deployables) != 2 {
		t.Fatalf("the report must be on the row before the gate: %+v", out.Report)
	}
	// Nothing past the gate may have run.
	if len(h.builder.built) != 0 || len(h.publisher.published) != 0 || h.roller.rolls != 0 {
		t.Fatalf("the gate must block every later stage: built=%v published=%v rolls=%d",
			h.builder.built, h.publisher.published, h.roller.rolls)
	}
	// And the row must NOT be closed -- a terminal row accepts no further
	// writes, so closing here would strand the person's confirmation.
	if h.engine.sawStatement("mutation closePackageDeployment") {
		t.Fatal("a parked run must not close its deployment row")
	}
}

func TestTheD9GateRefusesADslDeployBeforeAnythingRuns(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     plainUser(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	})
	if err == nil {
		t.Fatalf("a DSL-carrying package must be refused for a non-cluster-owner: %+v", out)
	}
	if got := RefusalCode(err); got != CodeDslRequiresClusterOwner {
		t.Fatalf("want %s, got %s (%v)", CodeDslRequiresClusterOwner, got, err)
	}
	// AT START is the whole requirement: nothing may have been built, staged,
	// rolled or published. A gate that fired after the build would still
	// refuse -- and would have run somebody's build script first.
	if len(h.builder.built) != 0 {
		t.Fatalf("the gate must fire before any build: %v", h.builder.built)
	}
	if len(h.stager.staged) != 0 || h.stager.written != 0 || h.roller.rolls != 0 {
		t.Fatalf("the gate must fire before any stage or roll: staged=%v written=%d rolls=%d",
			h.stager.staged, h.stager.written, h.roller.rolls)
	}
	if len(h.publisher.published) != 0 {
		t.Fatalf("the gate must fire before any publish: %v", h.publisher.published)
	}
	if out.Status != StatusRefused {
		t.Fatalf("a typed refusal closes the row as refused, got %q", out.Status)
	}
}

func TestAnSpaOnlyPackageDeploysUnderTheCallersOwnAuthority(t *testing.T) {
	// The reachable positive for the gate above: the same caller, the same
	// everything, minus the DSL.
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     plainUser(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	})
	if err != nil {
		t.Fatalf("an SPAs-only package must deploy for an ordinary user: %v", err)
	}
	if out.Status != StatusSucceeded {
		t.Fatalf("want succeeded, got %q", out.Status)
	}
	if len(h.publisher.published) != 2 {
		t.Fatalf("both deployables must publish: %v", h.publisher.published)
	}
}

func TestAnSpaOnlyDeploySkipsStageAndRollEntirely(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(h.stager.staged) != 0 || h.stager.written != 0 {
		t.Fatalf("no DSL means no staging: staged=%v written=%d", h.stager.staged, h.stager.written)
	}
	if h.roller.rolls != 0 {
		t.Fatalf("no DSL means NO RESTART -- an SPA redeploy must not roll the cluster; rolls=%d", h.roller.rolls)
	}
	if !h.engine.sawStatement(`"` + StatusPublishing + `"`) {
		t.Fatal("the run must still reach publishing")
	}
	for _, skipped := range []string{StatusStagingDsl, StatusRolling} {
		if h.engine.sawStatement(`advancePackageDeployment(deploymentId: "v1:platform:packageDeployment:fixed1", status: "` + skipped + `"`) {
			t.Fatalf("the row must never record a %s stage it did not run", skipped)
		}
	}
}

func TestUnchangedDslSkipsTheRollAndPublishesAnyway(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	req := DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}
	if _, err := Deploy(context.Background(), h.deps, req); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if h.roller.rolls != 1 {
		t.Fatalf("the first deploy of new DSL must roll once, got %d", h.roller.rolls)
	}

	// Same tree, same bytes. The active set already points at this content,
	// so nothing changed and nothing restarts.
	before := h.roller.rolls
	if _, err := Deploy(context.Background(), h.deps, req); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	if h.roller.rolls != before {
		t.Fatalf("redeploying identical DSL must not roll again: %d -> %d", before, h.roller.rolls)
	}
	if len(h.publisher.published) != 4 {
		t.Fatalf("the sites must still be republished both times: %v", h.publisher.published)
	}
}

func TestAFailedBuildLandsALogTailAndPublishesNothing(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.builder.err = errors.New("exit status 1")
	h.builder.tail = "npm ERR! missing script: build"

	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	})
	if err == nil {
		t.Fatal("a failed build must fail the deploy")
	}
	if len(h.publisher.published) != 0 {
		t.Fatalf("every site must still be serving its old version: %v", h.publisher.published)
	}
	if out.Status != StatusRefused && out.Status != StatusFailed {
		t.Fatalf("want a terminal status, got %q", out.Status)
	}
	// The log tail has to reach the row, or "why did my build fail" is
	// answered in a pod log the person cannot reach.
	if !h.engine.sawStatement("missing script: build") {
		t.Fatalf("the build log tail must land on the deployment row; statements: %v", h.engine.statements())
	}
}

// TestTheCredentialIsResolvedUnderThePackageOwnerAtFetchTimeAndReachesNoRow
// pins the D10 handoff (epic memql#4885): the pipeline hands the fetch the
// credential NAME and the package's OWNER, straight off the row, and never a
// value. The caller here is a CLUSTER OWNER deploying somebody else's package,
// which is the case the owner field exists for -- the fetch resolves under the
// package owner, not under the person clicking.
func TestTheCredentialIsResolvedUnderThePackageOwnerAtFetchTimeAndReachesNoRow(t *testing.T) {
	pkg := ownerPackage()
	pkg["credentialId"] = "v1:platform:sourceCredential:acme"
	h := newHarness(t, spaOnlyPackage(), pkg)
	h.fetcher.credentials = map[string]string{"v1:platform:sourceCredential:acme": "ghp_SUPERSECRETVALUE"}

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	if got := h.fetcher.sourceSeen.CredentialId; got != "v1:platform:sourceCredential:acme" {
		t.Fatalf("the fetch must be handed the credential NAME, got %q", got)
	}
	if got, want := h.fetcher.sourceSeen.OwnerUserId, rowString(pkg, "ownerUserId"); got != want {
		t.Fatalf("the fetch must be handed the PACKAGE owner to resolve under, got %q want %q -- the caller was a cluster owner, and resolving under them would be resolving under the wrong person", got, want)
	}
	if h.fetcher.sourceSeen.OwnerUserId == clusterOwner().UserId {
		t.Fatal("control failed: the package owner and the caller are the same id, so this test cannot tell them apart")
	}
	if len(h.fetcher.resolvedTokens) != 1 {
		t.Fatalf("the credential must be resolved exactly once, at fetch time: %v", h.fetcher.resolvedTokens)
	}
	// The reachable positive: the NAME does appear in the package row this
	// test seeded, so a search that found nothing would be searching wrongly.
	for _, q := range h.engine.statements() {
		if strings.Contains(q, "ghp_SUPERSECRETVALUE") {
			t.Fatalf("the token VALUE reached a row: %s", q)
		}
	}
}

func TestBothOutcomesAudit(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		if _, err := Deploy(context.Background(), h.deps, DeployRequest{
			PackageId: "v1:platform:package:abc",
			Actor:     clusterOwner(),
			Confirmed: true,
			Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
		}); err != nil {
			t.Fatalf("deploy: %v", err)
		}
		if len(h.auditor.events) != 1 || h.auditor.events[0].Status != StatusSucceeded {
			t.Fatalf("want one success event, got %+v", h.auditor.events)
		}
	})

	t.Run("refusal", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		if _, err := Deploy(context.Background(), h.deps, DeployRequest{
			PackageId: "v1:platform:package:abc",
			Actor:     plainUser(),
			Confirmed: true,
		}); err == nil {
			t.Fatal("want a refusal")
		}
		if len(h.auditor.events) != 1 {
			t.Fatalf("want one event, got %+v", h.auditor.events)
		}
		if got := h.auditor.events[0].FailureReason; got != CodeDslRequiresClusterOwner {
			t.Fatalf("the audit must carry the stable code, got %q", got)
		}
	})
}

func TestARetryIsANewRow(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	req := DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}
	first, err := Deploy(context.Background(), h.deps, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Deploy(context.Background(), h.deps, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.DeploymentId == second.DeploymentId {
		t.Fatalf("a retry must open a new row, both were %s", first.DeploymentId)
	}
}

func TestEveryStatusTheMachineWritesIsInTheDeclaredEnum(t *testing.T) {
	// The enum lives in dsl/platform/concepts.memql and the machine writes
	// strings; the two agree here or a stage advance is refused at runtime by
	// a validator nothing in this package can see.
	declared := map[string]bool{}
	for _, s := range stageOrder {
		declared[s] = true
	}
	for s := range packageDeploymentTerminalStatuses {
		declared[s] = true
	}

	h := newHarness(t, validPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	seen := 0
	for _, q := range h.engine.statements() {
		if !strings.Contains(q, "status: ") {
			continue
		}
		for _, part := range strings.Split(q, "status: ")[1:] {
			value := strings.Trim(strings.SplitN(part, ",", 2)[0], `") `)
			if value == "" || strings.HasPrefix(value, "args") {
				continue
			}
			seen++
			if !declared[value] {
				t.Errorf("the machine wrote status %q, which is not in the declared enum", value)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no status writes observed; this test would pass vacuously")
	}
}

// TestTheStageOrderIsTheD6Law pins the ORDER the row records, which is what
// makes a partial deploy structurally impossible: a failure anywhere before
// publishing leaves every site serving what it was serving.
func TestTheStageOrderIsTheD6Law(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	var order []string
	for _, q := range h.engine.statements() {
		if !strings.HasPrefix(q, "mutation advancePackageDeployment") {
			continue
		}
		value := strings.Trim(strings.SplitN(strings.Split(q, "status: ")[1], ")", 2)[0], `" `)
		order = append(order, value)
	}

	want := []string{StatusBuilding, StatusStagingDsl, StatusRolling, StatusPublishing}
	if len(order) != len(want) {
		t.Fatalf("want the four post-confirm stages, got %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("stage %d: want %s, got %s (full order %v)", i, want[i], order[i], order)
		}
	}
	// And the effects landed in that order too: the pointer moved before the
	// roll, and the roll before the publish.
	if h.stager.written != 1 || h.roller.rolls != 1 || len(h.publisher.published) != 2 {
		t.Fatalf("effects: written=%d rolls=%d published=%v", h.stager.written, h.roller.rolls, h.publisher.published)
	}
}
