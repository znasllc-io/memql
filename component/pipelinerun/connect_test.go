package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// connect_test.go -- a source's one pipeline (decisions 8, 12, 14).

const connectManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: unit
          run: go test ./...
          secrets: [SHOP_TOKEN]
`

// connectHarness is a harness with ownerID's GitHub repository source and a
// repository whose default branch's head carries manifest.
func connectHarness(t *testing.T, manifest string) *harness {
	t.Helper()
	h := newHarness(t)
	h.store.addPackage(PackageSource{
		ID: packageID, OwnerUserID: "v1:identity:user:" + ownerID, AccountID: "acct-1", Name: "shop",
		SourceKind: "repo", RepoURL: "https://github.com/Acme/Shop", CredentialID: credID, Status: "active",
	})
	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: "Acme/Shop", DefaultBranch: "main"}
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	tree := fstest.MapFS{
		"README.md":   {Data: []byte("# shop")},
		"main.go":     {Data: []byte("package main")},
		"go.mod":      {Data: []byte("module acme.test/shop")},
		"sub/main.go": {Data: []byte("package sub")},
	}
	if manifest != "" {
		tree[pipelines.ManifestPath] = &fstest.MapFile{Data: []byte(manifest)}
	}
	h.github.trees[repoName+"@"+shaA] = tree
	return h
}

// connect connects with the defaults a test does not care about: the shop's
// package, webhook delivery, and connectManifest's one secret allowed. A test
// about the allowlist passes its own (an empty, non-nil list allows nothing).
func connect(h *harness, ctx context.Context, req ConnectRequest) (ConnectResult, error) {
	if req.PackageID == "" {
		req.PackageID = packageID
	}
	if req.Delivery == "" {
		req.Delivery = DeliveryWebhook
	}
	if req.SecretNames == nil {
		req.SecretNames = []string{"SHOP_TOKEN"}
	}
	return h.integ.Connect(ctx, req)
}

func TestConnectCreatesTheSourcesOnePipeline(t *testing.T) {
	h := connectHarness(t, connectManifest)
	res, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll, SecretNames: []string{"SHOP_TOKEN", "B_KEY", "SHOP_TOKEN"}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	wantID := PipelineIDFor(packageID)
	if res.PipelineID != wantID || res.Repository != repoName || res.Delivery != DeliveryPoll || res.Compute != pipelines.ComputeCluster {
		t.Errorf("answer = %+v", res)
	}
	if len(res.Stages) != 2 || res.Stages[0] != "checks" || res.Stages[1] != "tests" {
		t.Errorf("stages = %v", res.Stages)
	}
	if res.Reconnected {
		t.Errorf("a first connect is not a reconnect")
	}
	p, ok := h.store.pipeline(wantID)
	if !ok {
		t.Fatalf("no pipeline row at %q", wantID)
	}
	if p.OwnerUserID != "v1:identity:user:"+ownerID || p.PackageID != packageID || p.AccountID != "acct-1" {
		t.Errorf("the pipeline is the source's owner's, tied to its account: %+v", p)
	}
	if p.InstallationID != 7 || p.CredentialID != credID || p.DefaultBranch != "main" || p.Name != "shop" {
		t.Errorf("pipeline = %+v", p)
	}
	if len(p.SecretNames) != 2 || p.SecretNames[0] != "B_KEY" || p.SecretNames[1] != "SHOP_TOKEN" {
		t.Errorf("the allowlist is deduplicated and sorted: %v", p.SecretNames)
	}
	if len(h.github.treeCalls) != 1 || h.github.treeCalls[0] != repoName+"@"+shaA {
		t.Errorf("the manifest is read at the default branch's head: %v", h.github.treeCalls)
	}
	mints := h.github.tokenMints
	if len(mints) != 1 || mints[0].CredentialID != credID || mints[0].Owner != "v1:identity:user:"+ownerID {
		t.Errorf("the grant is proved by a mint through the owner's credential: %+v", mints)
	}
}

func TestConnectRefusesAPersonWhoDoesNotOwnTheSource(t *testing.T) {
	h := connectHarness(t, connectManifest)

	// A cluster owner READS every source -- and still does not connect one
	// that is somebody else's.
	if _, err := connect(h, clusterOwnerCtx(otherID), ConnectRequest{}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("a cluster owner connecting a colleague's source: %v, want ErrNotOwner", err)
	}
	// Anyone else reads nothing at all, and is told so without the sentence
	// claiming to know whether it exists.
	_, err := connect(h, personCtx(otherID), ConnectRequest{})
	if code := refusalCode(err); code != packages.CodeSourceUnreadable {
		t.Errorf("a stranger: %v (code %q), want %s", err, code, packages.CodeSourceUnreadable)
	}
	// Nobody at all.
	if _, err := connect(h, context.Background(), ConnectRequest{}); !errors.Is(err, ErrNoCaller) {
		t.Errorf("no caller: %v", err)
	}
	if n := len(h.store.pipelineCreates); n != 0 {
		t.Errorf("nothing is written for a refused connect: %d", n)
	}
	if n := len(h.github.tokenMints); n != 0 {
		t.Errorf("no token is minted before ownership is proved: %d mints", n)
	}
}

func TestConnectRefusesASourceThatIsNotAGitHubRepository(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*PackageSource)
		code   string
	}{
		"a zip from the Library": {func(p *PackageSource) { p.SourceKind = "artifact"; p.RepoURL = "" }, packages.CodeSourceHostUnsupported},
		"another host":           {func(p *PackageSource) { p.RepoURL = "https://gitlab.com/acme/shop" }, packages.CodeSourceHostUnsupported},
		"no GitHub connection":   {func(p *PackageSource) { p.CredentialID = "" }, packages.CodeCredentialNotFound},
		"no repository named":    {func(p *PackageSource) { p.RepoURL = "https://github.com/acme" }, packages.CodeSourceUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			h := connectHarness(t, connectManifest)
			pkg := h.store.packages[packageID]
			c.mutate(&pkg)
			h.store.addPackage(pkg)
			_, err := connect(h, personCtx(ownerID), ConnectRequest{})
			if got := refusalCode(err); got != c.code {
				t.Errorf("err = %v (code %q), want %s", err, got, c.code)
			}
		})
	}
}

func TestConnectRefusesASourceWithNoPipelineBlock(t *testing.T) {
	h := connectHarness(t, "formatVersion: 1\nname: shop\n")
	_, err := connect(h, personCtx(ownerID), ConnectRequest{})
	var r *pipelines.Refusal
	if !errors.As(err, &r) || r.Code != pipelines.CodeNotDeclared {
		t.Fatalf("err = %v, want %s", err, pipelines.CodeNotDeclared)
	}
	if n := len(h.store.pipelineCreates); n != 0 {
		t.Errorf("a pipeline that could never compile is never connected: %d writes", n)
	}
}

func TestConnectPassesAValidateRefusalThrough(t *testing.T) {
	h := connectHarness(t, `formatVersion: 1
name: shop
pipeline:
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
          needs: { quantum: true }
`)
	_, err := connect(h, personCtx(ownerID), ConnectRequest{})
	var r *pipelines.Refusal
	if !errors.As(err, &r) || r.Code != pipelines.CodeNeedUnknown || r.Scope != "checks/vet" {
		t.Fatalf("err = %v, want %s scoped checks/vet", err, pipelines.CodeNeedUnknown)
	}
}

func TestConnectRefusesAMissingOrBrokenManifest(t *testing.T) {
	missing := connectHarness(t, "")
	if _, err := connect(missing, personCtx(ownerID), ConnectRequest{}); refusalCode(err) != packages.CodeManifestMissing {
		t.Errorf("no manifest: %v", err)
	}
	broken := connectHarness(t, "formatVersion: 1\nname: shop\npipeline:\n  stagez: []\n")
	if _, err := connect(broken, personCtx(ownerID), ConnectRequest{}); refusalCode(err) != packages.CodeManifestInvalid {
		t.Errorf("an unknown key in the block is the manifest's refusal: %v", err)
	}
}

func TestConnectRefusesAnUnusableAllowlistOrOption(t *testing.T) {
	h := connectHarness(t, connectManifest)
	for _, name := range []string{"MEMQL_SHA", "lower_case", "1LEADING"} {
		_, err := connect(h, personCtx(ownerID), ConnectRequest{SecretNames: []string{name}})
		var r *pipelines.Refusal
		if !errors.As(err, &r) || r.Code != pipelines.CodeSecretInvalid {
			t.Errorf("secret %q: %v, want %s", name, err, pipelines.CodeSecretInvalid)
		}
	}
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: "carrier-pigeon"}); err == nil {
		t.Errorf("an unknown delivery is refused")
	}
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{Compute: "laptop"}); err == nil {
		t.Errorf("an unknown compute is refused")
	}
}

func TestConnectPassesTheGrantsRefusalThrough(t *testing.T) {
	h := connectHarness(t, connectManifest)
	h.github.tokenErr = &packages.Refusal{Code: packages.CodeReconnectRequired, Detail: "GitHub refused the grant"}
	_, err := connect(h, personCtx(ownerID), ConnectRequest{})
	if refusalCode(err) != packages.CodeReconnectRequired {
		t.Errorf("err = %v", err)
	}
}

func TestReconnectReactivatesAndKeepsWhatThePollSaw(t *testing.T) {
	h := connectHarness(t, connectManifest)
	existing := testPipeline(DeliveryWebhook)
	existing.Status = PipelineDisconnected
	existing.Heads = map[string]string{"branch:main": shaA}
	existing.Timings = map[string]float64{"acme.test/shop": 1.5}
	h.store.addPipeline(existing)

	res, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll, Compute: pipelines.ComputeClusterAndFleet})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !res.Reconnected {
		t.Errorf("a second connect is a reconnect")
	}
	p, _ := h.store.pipeline(existing.ID)
	if p.Status != PipelineActive || p.Delivery != DeliveryPoll || p.Compute != pipelines.ComputeClusterAndFleet {
		t.Errorf("reconnected = %+v", p)
	}
	if p.Heads["branch:main"] != shaA || p.Timings["acme.test/shop"] != 1.5 {
		t.Errorf("the poll's heads and the timing table survive a reconnect: %v %v", p.Heads, p.Timings)
	}
}

func TestReconnectToAnotherRepositoryForgetsTheOldHeads(t *testing.T) {
	h := connectHarness(t, connectManifest)
	existing := testPipeline(DeliveryPoll)
	existing.Repository = "acme/old-shop"
	existing.Heads = map[string]string{"branch:main": shaC}
	h.store.addPipeline(existing)

	if _, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	p, _ := h.store.pipeline(existing.ID)
	if p.Repository != repoName || len(p.Heads) != 0 {
		t.Errorf("another repository's heads are no baseline here: %s %v", p.Repository, p.Heads)
	}
}

// A poll of the OLD repository that read the row before the move writes its
// heads under the old repository's key, which the connect's own section does
// not hold -- landing here right after the row names the new repository. The
// fence connect takes on the old key afterwards clears them, so the new
// repository never starts from the old one's heads.
func TestAReconnectElsewhereClearsHeadsALatePollWrote(t *testing.T) {
	h := connectHarness(t, connectManifest)
	existing := testPipeline(DeliveryPoll)
	existing.Repository = "acme/old-shop"
	existing.Heads = map[string]string{"branch:main": shaC}
	h.store.addPipeline(existing)
	late := true
	h.store.afterCreatePipeline = func() {
		if late {
			late = false
			if err := h.store.UpdatePipeline(context.Background(), existing.OwnerUserID, existing.ID,
				PipelinePatch{Heads: ptr(map[string]string{"branch:main": shaB, "pr:4": shaC})}); err != nil {
				t.Errorf("the late poll's write: %v", err)
			}
		}
	}

	if _, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	p, _ := h.store.pipeline(existing.ID)
	if p.Repository != repoName || len(p.Heads) != 0 {
		t.Errorf("the old repository's heads are no baseline for the new one: %s %v", p.Repository, p.Heads)
	}
	keys := h.gate.seen()
	if len(keys) != 2 || keys[0] != RepositoryGateKey(repoName) || keys[1] != RepositoryGateKey("acme/old-shop") {
		t.Errorf("gate keys = %v; want the new repository's, then -- after it, never inside it -- the old one's", keys)
	}
}

func TestDisconnectIsTheOwnersAndKeepsTheRow(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)

	if _, err := h.integ.Disconnect(personCtx(otherID), p.ID); err == nil {
		t.Errorf("another person disconnected somebody else's pipeline")
	}
	got, err := h.integ.Disconnect(personCtx(ownerID), p.ID)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if got.Status != PipelineDisconnected {
		t.Errorf("status = %q", got.Status)
	}
	stored, ok := h.store.pipeline(p.ID)
	if !ok || stored.Status != PipelineDisconnected {
		t.Errorf("the row stays, disconnected: %+v", stored)
	}
	if last := h.store.pipelineUpdates[len(h.store.pipelineUpdates)-1]; last.Owner != p.OwnerUserID {
		t.Errorf("written under the owner's authority, got %q", last.Owner)
	}
}

// An operator must be able to free a repository a departed owner's pipeline
// holds -- connect refuses every other source while it is active, and only a
// disconnect frees it. So a cluster owner may disconnect ANY pipeline; the
// write is still made under the row owner's borrowed authority, and it writes
// the status and nothing else. Every other person is held to their own.
func TestAClusterOwnerMayDisconnectAnyPipeline(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)

	got, err := h.integ.Disconnect(clusterOwnerCtx(otherID), p.ID)
	if err != nil {
		t.Fatalf("a cluster owner disconnecting a colleague's pipeline: %v", err)
	}
	if got.Status != PipelineDisconnected {
		t.Errorf("status = %q", got.Status)
	}
	stored, _ := h.store.pipeline(p.ID)
	if stored.Status != PipelineDisconnected || stored.OwnerUserID != p.OwnerUserID {
		t.Errorf("the row is disconnected and still its owner's: %+v", stored)
	}
	if len(h.store.pipelineUpdates) != 1 {
		t.Fatalf("pipeline writes = %d, want the one status write", len(h.store.pipelineUpdates))
	}
	if u := h.store.pipelineUpdates[0]; u.Owner != p.OwnerUserID || u.Patch.Status == nil || u.Patch.Heads != nil || u.Patch.SecretNames != nil {
		t.Errorf("the status alone, under the row owner's authority: %+v", u)
	}

	// The repository is free: another source connects it.
	h2 := connectHarness(t, connectManifest)
	theirs := testPipeline(DeliveryWebhook)
	theirs.ID, theirs.PackageID, theirs.OwnerUserID = PipelineIDFor("pkg-departed"), "pkg-departed", "v1:identity:user:user-gone"
	h2.store.addPipeline(theirs)
	if _, err := connect(h2, personCtx(ownerID), ConnectRequest{}); refusalCode(err) != pipelines.CodeAlreadyConnected {
		t.Fatalf("while the departed owner's pipeline is active: %v", err)
	}
	if _, err := h2.integ.Disconnect(clusterOwnerCtx("user-operator"), theirs.ID); err != nil {
		t.Fatalf("the operator frees it: %v", err)
	}
	if _, err := connect(h2, personCtx(ownerID), ConnectRequest{}); err != nil {
		t.Errorf("then another source connects it: %v", err)
	}

	// Not a cluster owner: held to their own, and told nothing about whose.
	h3 := newHarness(t)
	h3.store.addPipeline(p)
	for name, ctx := range map[string]context.Context{
		"a developer": personCtx(otherID),
		"an admin": auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: otherID, Role: auth.RoleAdmin,
		}),
		"a synthetic owner": auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "operator-key", Role: auth.RoleOwner, Synthetic: true,
		}),
	} {
		if _, err := h3.integ.Disconnect(ctx, p.ID); err == nil {
			t.Errorf("%s disconnected somebody else's pipeline", name)
		}
	}
	if stored, _ := h3.store.pipeline(p.ID); stored.Status != PipelineActive {
		t.Errorf("a refused disconnect wrote: %+v", stored)
	}
}

func TestTheConnectCapabilityAnswersThePipeline(t *testing.T) {
	h := connectHarness(t, connectManifest)
	nodes, err := h.integ.handleConnect(personCtx(ownerID), map[string]any{
		"packageId": packageID, "delivery": "webhook", "secretNames": []any{"SHOP_TOKEN"},
	}, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	payload := decodeNode(t, nodes)
	if payload["pipelineId"] != PipelineIDFor(packageID) || payload["compute"] != "cluster" || payload["repository"] != repoName {
		t.Errorf("answer = %v", payload)
	}
}

// Consent is asked of EVERY step, not only the stages one event would plan:
// a step in a push-only stage that names a need, on a cluster-only pipeline,
// is refused at connect rather than on the first push that reaches it.
func TestConnectAsksConsentOfEveryStep(t *testing.T) {
	const fleetManifest = `formatVersion: 1
name: shop
pipeline:
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: release
      on: [push]
      steps:
        - name: sign
          run: make sign
          execution: native
          platform: darwin/arm64
          needs: { macos_tooling: true }
`
	h := connectHarness(t, fleetManifest)
	_, err := connect(h, personCtx(ownerID), ConnectRequest{})
	var r *pipelines.Refusal
	if !errors.As(err, &r) || r.Code != pipelines.CodeFleetNotConsented || r.Scope != "release/sign" {
		t.Fatalf("err = %v, want %s scoped release/sign", err, pipelines.CodeFleetNotConsented)
	}
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{Compute: pipelines.ComputeClusterAndFleet}); err != nil {
		t.Errorf("with the fleet consented the same pipeline connects: %v", err)
	}

	// The allowlist too: connectManifest's unit step names SHOP_TOKEN.
	h = connectHarness(t, connectManifest)
	_, err = connect(h, personCtx(ownerID), ConnectRequest{SecretNames: []string{}})
	if !errors.As(err, &r) || r.Code != pipelines.CodeSecretNotAllowed || r.Scope != "tests/unit" {
		t.Fatalf("err = %v, want %s scoped tests/unit", err, pipelines.CodeSecretNotAllowed)
	}
	if n := len(h.store.pipelineCreates); n != 0 {
		t.Errorf("a pipeline some event could never run is never connected: %d writes", n)
	}
}

// One pipeline per repository: another source's ACTIVE pipeline of the same
// repository refuses the connect, before any request leaves the cluster; a
// disconnected one is history and refuses nothing; the source's own is a
// reconnect.
func TestConnectRefusesARepositoryAnotherSourceRuns(t *testing.T) {
	h := connectHarness(t, connectManifest)
	theirs := testPipeline(DeliveryWebhook)
	theirs.ID, theirs.PackageID, theirs.OwnerUserID = PipelineIDFor("pkg-theirs"), "pkg-theirs", "v1:identity:user:"+otherID
	h.store.addPipeline(theirs)

	_, err := connect(h, personCtx(ownerID), ConnectRequest{})
	var r *pipelines.Refusal
	if !errors.As(err, &r) || r.Code != pipelines.CodeAlreadyConnected {
		t.Fatalf("err = %v, want %s", err, pipelines.CodeAlreadyConnected)
	}
	if strings.Contains(r.Detail, otherID) {
		t.Errorf("the refusal names whose pipeline it is: %q", r.Detail)
	}
	if n := len(h.github.tokenMints); n != 0 {
		t.Errorf("the repository was known taken before any request left the cluster: %d mints", n)
	}
	if n := len(h.store.pipelineCreates); n != 0 {
		t.Errorf("nothing is written: %d", n)
	}

	theirs.Status = PipelineDisconnected
	h.store.addPipeline(theirs)
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{}); err != nil {
		t.Fatalf("a disconnected predecessor blocks nothing: %v", err)
	}
	// And the source's own pipeline, now active, is a reconnect.
	res, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll})
	if err != nil || !res.Reconnected {
		t.Fatalf("reconnecting the source's own pipeline: %+v %v", res, err)
	}
}

// Two sources connecting one repository at once: the repository's gate
// serializes them, and exactly one pipeline is connected.
func TestConcurrentConnectsOfOneRepositoryConnectOne(t *testing.T) {
	h := connectHarness(t, connectManifest)
	second := h.store.packages[packageID]
	second.ID, second.OwnerUserID = "pkg-second", "v1:identity:user:"+otherID
	h.store.addPackage(second)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct {
		ctx context.Context
		pkg string
	}{
		{personCtx(ownerID), packageID},
		{personCtx(otherID), "pkg-second"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = connect(h, c.ctx, ConnectRequest{PackageID: c.pkg})
		}()
	}
	wg.Wait()
	refused := 0
	for _, err := range errs {
		if refusalCode(err) == pipelines.CodeAlreadyConnected {
			refused++
		} else if err != nil {
			t.Errorf("unexpected: %v", err)
		}
	}
	if refused != 1 || len(h.store.pipelineCreates) != 1 {
		t.Errorf("refused %d, created %d; want exactly one of each", refused, len(h.store.pipelineCreates))
	}
}

// refusalCode is the catalogued code an error carries, from either
// catalogue, or "".
func refusalCode(err error) string {
	var pr *packages.Refusal
	if errors.As(err, &pr) {
		return pr.Code
	}
	var r *pipelines.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}
