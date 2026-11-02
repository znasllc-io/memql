package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// trainReleaseFixture is a saved-release fixture that can really publish and seal, with
// a train's request for it: internal origin and the owner's standing authority.
func trainReleaseFixture(t *testing.T) (*buildDBFixture, pl.TrainReleaseRequest, context.Context) {
	t.Helper()
	f := newBuildDBFixture(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	f.config.Profiles[0].Registries[0].Origin = server.URL
	f.config.Profiles[0].Registries[0].Repository = "candidate/" + f.runID
	f.config.Profiles[0].Registries[0].AllowLoopbackHTTP = true
	f.publication = `@template
automation fixtureBuildPublication {
 args { candidateId string! approvalId string! }
 candidate := builtin releaseGetCandidate(candidateId: args.candidateId)
 targets := builtin releaseCandidateTargets(candidateId: args.candidateId)
 if candidate.candidateId != args.candidateId || candidate.approvalId != args.approvalId || targets.candidateId != args.candidateId { return }
 builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId, component: "engine", artifact: "image", targetId: "registry")
 builtin releaseSealPublishedCandidate(candidateId: args.candidateId, approvalId: args.approvalId)
}`
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := catalogConfiguration{FormatVersion: 1, Publisher: "fixture", KeyID: "fixture-key", SigningKeySecret: "CATALOG_KEY", PublicKeys: map[string]string{"fixture-key": base64.StdEncoding.EncodeToString(public)}}
	body, _ := json.Marshal(cfg)
	variables, secrets := f.i.resolver.systemVariable, f.i.resolver.systemSecret
	f.i.resolver.systemVariable = func(ctx context.Context, name string) (string, error) {
		if name == ReleaseCatalogConfigurationVariable {
			return string(body), nil
		}
		return variables(ctx, name)
	}
	f.i.resolver.systemSecret = func(ctx context.Context, name string) (string, error) {
		if name == "CATALOG_KEY" {
			return base64.StdEncoding.EncodeToString(key.Seed()), nil
		}
		return secrets(ctx, name)
	}
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	owner, err := candidateOwner(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	r := pl.TrainReleaseRequest{
		Profile: "engine", PipelineID: f.pipeline.source.PipelineID, Repository: f.pipeline.source.Repository, SHA: f.pipeline.source.SHA,
		Version: "0.25.0", OwnerUserID: owner, Authority: map[string]any{"credentialClass": auth.ForwardedClassUser, "roleCeiling": string(auth.RoleOwner)},
	}
	return f, r, auth.ContextWithInternalOrigin(context.Background())
}

// A train walks the saved journey by itself, a step at a time on either replica, and the
// release it reaches is the one a person's press would have: tagged at the commit it
// named, approved, published and sealed.
func TestATrainPublishesOnItsOwnersWordAcrossReplicas(t *testing.T) {
	f, r, tick := trainReleaseFixture(t)
	peer := f.peer(t)
	step := func(i *Integration) pl.TrainReleaseStatus {
		t.Helper()
		status, err := i.AdvanceTrainRelease(tick, r)
		if err != nil {
			t.Fatalf("step at %+v: %v", r, err)
		}
		r.IntentID = status.IntentID
		return status
	}
	status := step(f.i)
	if status.Phase != "prepared" || status.IntentID == "" || status.Stop {
		t.Fatalf("after preparing: %+v", status)
	}
	f.key = status.IntentID
	if status = step(peer); status.Phase != "building" || status.RunID == "" {
		t.Fatalf("after starting: %+v", status)
	}
	if status = step(f.i); status.Phase != "building" || f.pipeline.starts != 1 {
		t.Fatalf("while it runs: %+v, started %d times", status, f.pipeline.starts)
	}
	f.pipeline.run.Status, f.pipeline.run.Conclusion, f.pipeline.run.WorkRunID = "completed", "success", f.runs["build"]
	if status = step(peer); status.Phase != "built" {
		t.Fatalf("once it passed: %+v", status)
	}
	status = step(f.i)
	if status.Phase != "ready" || status.CandidateID == "" || status.ApprovalID != "" {
		t.Fatalf("after assembling, and before any approval: %+v", status)
	}
	cleanupCandidate(t, f.db, status.CandidateID)
	status = step(peer)
	if status.ApprovalID == "" || status.ApprovedBy != StandingApprover(r.OwnerUserID) {
		t.Fatalf("the approval: %+v", status)
	}
	// Publishing goes on past the call. Each later call reads where it stands, and one
	// that finds it running or stopped takes the next step.
	var published pl.TrainReleaseStatus
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		published = step(f.i)
		if published.Published {
			break
		}
		if published.Stop {
			t.Fatalf("publishing stopped: %+v", published)
		}
	}
	if !published.Published || published.CatalogDigest == "" {
		t.Fatalf("it never published: %+v", published)
	}

	// What a deploy reads next.
	release, err := f.i.NewestRelease(tick, r.OwnerUserID, r.Authority, "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "0.25.0" || release.Tag != "v0.25.0" || release.SHA != r.SHA || release.CandidateID != published.CandidateID || release.CatalogDigest != published.CatalogDigest {
		t.Fatalf("the newest release: %+v, published %+v", release, published)
	}
	if _, err := f.i.NewestRelease(tick, r.OwnerUserID, r.Authority, "engine", "9.9.9"); err == nil || !strings.Contains(err.Error(), "not in the release catalog") {
		t.Fatalf("a version never published: %v", err)
	}
}

func TestATrainStopsWhenItsOwnerLosesTheRole(t *testing.T) {
	f, r, tick := trainReleaseFixture(t)
	status, err := f.i.AdvanceTrainRelease(tick, r)
	if err != nil {
		t.Fatal(err)
	}
	r.IntentID = status.IntentID
	f.key = status.IntentID
	f.ownerRole.Store(string(auth.RoleAdmin))
	if _, err := f.i.AdvanceTrainRelease(tick, r); err == nil {
		t.Fatal("a demoted owner's release went on")
	}
	f.ownerRole.Store(string(auth.RoleOwner))
	if status, err = f.i.AdvanceTrainRelease(tick, r); err != nil || status.Phase != "building" {
		t.Fatalf("restored: %+v, %v", status, err)
	}
}

func TestATrainsReleaseIsRefusedWithoutItsStandingAndItsOrigin(t *testing.T) {
	f, r, tick := trainReleaseFixture(t)
	if _, err := f.i.AdvanceTrainRelease(ownerCtx(), r); err == nil || !strings.Contains(err.Error(), "internal origin") {
		t.Fatalf("a signed-in owner reached it: %v", err)
	}
	if _, err := f.i.AdvanceTrainRelease(context.Background(), r); err == nil {
		t.Fatal("nobody reached it")
	}
	missing := r
	missing.Authority = nil
	if _, err := f.i.AdvanceTrainRelease(tick, missing); err == nil || !strings.Contains(err.Error(), "standing authority") {
		t.Fatalf("no authority: %v", err)
	}
	writer := r
	writer.Authority = map[string]any{"credentialClass": auth.ForwardedClassUser, "roleCeiling": string(auth.RoleWriter)}
	if _, err := f.i.AdvanceTrainRelease(tick, writer); err == nil {
		t.Fatal("a writer's ceiling reached it")
	}
	other := r
	other.OwnerUserID = "someone-else"
	status, err := f.i.AdvanceTrainRelease(tick, other)
	if err == nil {
		t.Fatalf("another owner's pipeline reached it: %+v", status)
	}
}

func TestATrainStopsAtAVersionItDidNotCarry(t *testing.T) {
	f, r, tick := trainReleaseFixture(t)
	r.Version = "9.9.9"
	status, err := f.i.AdvanceTrainRelease(tick, r)
	if err != nil {
		t.Fatal(err)
	}
	f.key = status.IntentID
	if !status.Stop || status.ReasonCode != "version_differs" {
		t.Fatalf("status = %+v", status)
	}
}

func TestATrainAReadingHeldDoesNothing(t *testing.T) {
	f, r, tick := trainReleaseFixture(t)
	r.Observe = true
	if status, err := f.i.AdvanceTrainRelease(tick, r); err != nil || status.IntentID != "" {
		t.Fatalf("with nothing saved: %+v, %v", status, err)
	}
	r.Observe = false
	status, err := f.i.AdvanceTrainRelease(tick, r)
	if err != nil {
		t.Fatal(err)
	}
	r.IntentID, r.Observe = status.IntentID, true
	f.key = status.IntentID
	if status, err = f.i.AdvanceTrainRelease(tick, r); err != nil || status.Phase != "prepared" || f.pipeline.starts != 0 {
		t.Fatalf("a held train started something: %+v, %v, %d starts", status, err, f.pipeline.starts)
	}
}

// Each phase of the journey, and what comes next.
func TestTheNextStepOfASavedRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		view    releaseBuildView
		want    string
		stop    bool
		failed  bool
		reached bool
	}{
		{name: "prepared", view: releaseBuildView{Phase: "prepared", Actions: []string{"start"}}, want: "start"},
		{name: "tagging, unconfirmed", view: releaseBuildView{Phase: "tagging", Actions: []string{"observe", "resume"}}, want: "resume"},
		{name: "uncertain", view: releaseBuildView{Phase: "uncertain", Actions: []string{"observe", "resume"}}, want: "resume"},
		{name: "building", view: releaseBuildView{Phase: "building", Actions: []string{"observe"}}, want: "observe"},
		{name: "built", view: releaseBuildView{Phase: "built", Actions: []string{"resume"}}, want: "resume"},
		{name: "ready", view: releaseBuildView{Phase: "ready", Actions: []string{"view_candidate", "approve"}}, want: "approve"},
		{name: "approved", view: releaseBuildView{Phase: "ready", Actions: []string{"view_candidate", "publish"}}, want: "publish"},
		{name: "publishing", view: releaseBuildView{Phase: "publishing", Actions: []string{"view_candidate"}}, want: ""},
		{name: "a publication that stopped", view: releaseBuildView{Phase: "uncertain", ReasonCode: "publication_interrupted", Actions: []string{"view_candidate", "publish"}}, want: "publish"},
		{name: "published", view: releaseBuildView{Phase: "published", Actions: []string{"view_candidate"}}, want: "", reached: true},
		{name: "a failed build", view: releaseBuildView{Phase: "blocked", ReasonCode: "build_not_successful", Actions: []string{"observe"}}, want: "observe", failed: true},
		{name: "another block", view: releaseBuildView{Phase: "blocked", ReasonCode: "version_prepare_required"}, want: "", stop: true},
		{name: "a conflicting file", view: releaseBuildView{Phase: "uncertain", ReasonCode: "release_asset_conflict", Actions: []string{"publish"}}, want: "publish", stop: true},
		{name: "a promotion nobody can confirm", view: releaseBuildView{Phase: "uncertain", ReasonCode: "promotion_unconfirmed", Actions: []string{"publish"}}, want: "publish", stop: true},
		{name: "a changed profile", view: releaseBuildView{Phase: "ready", ReasonCode: "authority_changed"}, want: "", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trainReleaseAction(tc.view); got != tc.want {
				t.Errorf("action = %q, want %q", got, tc.want)
			}
			status := trainStatusOf(tc.view)
			if status.Stop != tc.stop || status.BuildFailed != tc.failed || status.Published != tc.reached {
				t.Errorf("status = %+v", status)
			}
		})
	}
}
