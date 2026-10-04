package pipelinerun

import (
	"errors"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// preview_test.go -- what connecting a source's pipeline would read, before
// anything is written (epic memql#5479, the connect rail's Repository stop).

// previewManifest is connectManifest plus what the rail must show before
// anybody confirms: a sharded packages step gated on a bucket, a step that
// needs a fleet machine, a push-only stage and a notify stage.
const previewManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  select:
    go: import-graph
    buckets:
      os: ["clients/**"]
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: unit
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 4
          secrets: [SHOP_TOKEN]
        - name: os-checks
          run: make os-test
          when: { bucket: os }
          needs: { docker: true }
    - name: deploy
      on: [push]
      steps:
        - name: verify-rollout
          run: ./verify.sh
          secrets: [VERIFY_TOKEN, SHOP_TOKEN]
    - name: notify
      on: [push]
      channel: znas-instance
`

func TestAPreviewReadsTheManifestAndWritesNothing(t *testing.T) {
	h := connectHarness(t, previewManifest)
	h.integ.Configure(func(d *Deps) { d.WebhookReachable = func() bool { return true } })

	res, err := h.integ.Preview(personCtx(ownerID), packageID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Refusal != nil {
		t.Fatalf("a manifest that validates is no refusal: %+v", res.Refusal)
	}
	if res.Repository != repoName || res.DefaultBranch != "main" || res.SHA != shaA || res.Name != "shop" || res.CheckName != "MemQL / shop" {
		t.Errorf("preview = %+v", res)
	}
	var names []string
	for _, st := range res.Stages {
		names = append(names, st.Name)
	}
	if !slices.Equal(names, []string{"checks", "tests", "deploy", "notify"}) {
		t.Fatalf("every stage, in the order written: %v", names)
	}
	tests := res.Stages[1]
	if len(tests.Steps) != 2 || tests.Steps[0].Name != "unit" || tests.Steps[0].Shards != 4 || tests.Steps[0].Packages != pipelines.PackagesAffected {
		t.Errorf("tests' steps carry their shape: %+v", tests.Steps)
	}
	if os := tests.Steps[1]; !slices.Equal(os.Needs, []string{"docker"}) || os.Bucket != "os" {
		t.Errorf("os-checks says what it needs and what gates it: %+v", os)
	}
	if deploy := res.Stages[2]; !slices.Equal(deploy.On, []string{"push"}) {
		t.Errorf("a push-only stage says so: %+v", deploy)
	}
	if notify := res.Stages[3]; notify.Channel != "znas-instance" || len(notify.Steps) != 0 {
		t.Errorf("a notify stage names its channel: %+v", notify)
	}
	if !slices.Equal(res.Needs, []string{"docker"}) || !slices.Equal(res.Secrets, []string{"SHOP_TOKEN", "VERIFY_TOKEN"}) {
		t.Errorf("needs %v, secrets %v: each once, sorted", res.Needs, res.Secrets)
	}
	if res.SuggestedDelivery != DeliveryWebhook {
		t.Errorf("a cluster GitHub can reach suggests the webhook: %q", res.SuggestedDelivery)
	}
	if res.Existing != nil {
		t.Errorf("an unconnected source has no pipeline: %+v", res.Existing)
	}
	if n := len(h.store.pipelineCreates); n != 0 {
		t.Errorf("a preview writes nothing: %d pipeline writes", n)
	}
}

func TestAPreviewSaysWhatRefusesAsAnAnswer(t *testing.T) {
	// No pipeline block: the stop that asked renders the refusal, so it is
	// the answer's, not an error.
	h := connectHarness(t, "formatVersion: 1\nname: shop\n")
	res, err := h.integ.Preview(personCtx(ownerID), packageID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Refusal == nil || res.Refusal.Code != pipelines.CodeNotDeclared || res.Refusal.Message == "" {
		t.Fatalf("no block is pipeline_not_declared: %+v", res.Refusal)
	}
	if res.Repository != repoName || res.DefaultBranch != "main" {
		t.Errorf("what was read before the refusal is still answered: %+v", res)
	}
	if res.SuggestedDelivery != DeliveryPoll {
		t.Errorf("a cluster GitHub cannot reach suggests the poll: %q", res.SuggestedDelivery)
	}

	// A repository another source already runs.
	h = connectHarness(t, previewManifest)
	other := testPipeline(DeliveryWebhook)
	other.ID, other.PackageID = "pl-other", "pkg-other"
	h.store.addPipeline(other)
	res, err = h.integ.Preview(personCtx(ownerID), packageID)
	if err != nil || res.Refusal == nil || res.Refusal.Code != pipelines.CodeAlreadyConnected {
		t.Fatalf("a repository with another source's pipeline: %+v %v", res.Refusal, err)
	}

	// Somebody else's source is not previewed for them: a source they cannot
	// read is the same zero rows as one that is not there, and the answer
	// says only that.
	h = connectHarness(t, previewManifest)
	res, err = h.integ.Preview(personCtx(otherID), packageID)
	if err != nil || res.Refusal == nil || res.Refusal.Code != "source_unreadable" || res.Repository != "" || len(res.Stages) != 0 {
		t.Errorf("a stranger's preview reads nothing of the source: %+v %v", res, err)
	}
	if _, err := h.integ.Preview(clusterOwnerCtx(otherID), packageID); !errors.Is(err, ErrNotOwner) {
		t.Errorf("a cluster owner previewing a colleague's source: %v, want ErrNotOwner", err)
	}
}

func TestAPreviewOfAConnectedSourceAnswersItsPipeline(t *testing.T) {
	h := connectHarness(t, previewManifest)
	if _, err := connect(h, personCtx(ownerID), ConnectRequest{
		Delivery: DeliveryPoll, Compute: pipelines.ComputeClusterAndFleet, SecretNames: []string{"SHOP_TOKEN", "VERIFY_TOKEN"},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := h.integ.Preview(personCtx(ownerID), packageID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Existing == nil || res.Existing.Delivery != DeliveryPoll || res.Existing.Compute != pipelines.ComputeClusterAndFleet ||
		!slices.Equal(res.Existing.SecretNames, []string{"SHOP_TOKEN", "VERIFY_TOKEN"}) || res.Existing.Status != PipelineActive {
		t.Errorf("a reconnect is prefilled from the pipeline it restates: %+v", res.Existing)
	}
	if res.Refusal != nil {
		t.Errorf("the source's own pipeline is a reconnect, not a refusal: %+v", res.Refusal)
	}
}
