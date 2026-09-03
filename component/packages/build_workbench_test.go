package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// build_workbench_test.go drives the BINDING -- the translation between the
// pipeline and the build surface -- against a runner that records what it was
// asked and answers what a test tells it to.
//
// The workbench's own half (the shell, the directory, the constructed
// environment, the class gate) is tested where it lives. What is testable only
// here is whether the pipeline asks for the right thing and reads the answer
// back correctly, which is exactly where a build surface that "works" ends up
// deploying an empty bundle.

// recordingRunner is a workbench that never runs anything.
type recordingRunner struct {
	requests []workbench.BuildRequest
	pins     []string
	// answer, when set, is what RunBuild returns; the zero value is a
	// successful build of one index.html.
	answer *workbench.BuildResult
}

func (r *recordingRunner) RunBuild(_ context.Context, req workbench.BuildRequest, pinned string) workbench.BuildResult {
	r.requests = append(r.requests, req)
	r.pins = append(r.pins, pinned)
	if r.answer != nil {
		return *r.answer
	}
	return workbench.BuildResult{
		OK:     true,
		NodeId: "workbench-7",
		Output: workbench.BuildBytes{Inline: builtTarGz(map[string]string{
			"dist/index.html":    "<!doctype html>",
			"dist/assets/app.js": "console.log(1)",
		})},
		FileCount: 2,
	}
}

// builtTarGz packs an output tree the way the workbench does.
func builtTarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestTheBuildSurfaceIsAskedForWhatTheManifestSaid(t *testing.T) {
	runner := &recordingRunner{}
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	if len(runner.requests) == 0 {
		t.Fatal("the build surface must be asked to build something")
	}
	req := runner.requests[0]
	if req.Command != DefaultBuildCommand || req.Output != DefaultBuildOutput {
		t.Fatalf("the manifest's build plan must reach the surface, got %q into %q", req.Command, req.Output)
	}
	if req.Path != "clients/web" {
		t.Fatalf("the deployable's own path must reach the surface, got %q", req.Path)
	}
	// THE OWNER IS THE PACKAGE'S, not the caller's. A cluster owner deploying
	// somebody's package builds it for that person.
	if req.OwnerUserId != "v1:identity:user:someone" {
		t.Fatalf("the build must run for the package's owner, got %q", req.OwnerUserId)
	}
	if req.DeploymentId == "" {
		t.Fatal("the build must name the deployment it belongs to; it keys the directory and the log subject")
	}
	if len(req.Source.Inline) == 0 {
		t.Fatal("the snapshot must reach the surface")
	}
	// The bounds are the PIPELINE's, passed in rather than re-read.
	if req.Limits.MaxFileBytes != h.deps.Limits.normalized().MaxFileBytes {
		t.Fatalf("the surface must be handed the pipeline's own limits, got %d", req.Limits.MaxFileBytes)
	}
}

func TestTheSecondAppOfARunPrefersTheNodeTheFirstBuiltOn(t *testing.T) {
	runner := &recordingRunner{}
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(runner.pins) != 2 {
		t.Fatalf("this fixture has two apps to build, got %d", len(runner.pins))
	}
	if runner.pins[0] != "" {
		t.Fatalf("the first app of a run has no node to prefer, got %q", runner.pins[0])
	}
	if runner.pins[1] != "workbench-7" {
		t.Fatalf("the second app must prefer the node the first built on, got %q", runner.pins[1])
	}
}

func TestTheBuiltBundleIsWhatThePublisherReceives(t *testing.T) {
	runner := &recordingRunner{}
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())
	// The publisher records the bundle it was handed, so the assertion is
	// about what would actually be SERVED rather than about a return value.
	var served []string
	h.publisher.onPublish = func(bundle edge.Bundle) {
		for name := range bundle {
			served = append(served, name)
		}
	}

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// The `dist/` the workbench packs under is STRIPPED: a site serves
	// index.html at its root, not dist/index.html.
	joined := strings.Join(served, " ")
	if !strings.Contains(joined, "index.html") || strings.Contains(joined, "dist/") {
		t.Fatalf("the output's top-level directory must be stripped before publishing, got %v", served)
	}
	if !strings.Contains(joined, "assets/app.js") {
		t.Fatalf("every built file must reach the publisher, got %v", served)
	}
}

func TestATypedRefusalFromTheBuildSurfaceKeepsItsCode(t *testing.T) {
	cases := []struct {
		name string
		from string
		want string
	}{
		// An operator fact about the cluster, all the way to the Build stop.
		{"no peer", workbench.BuildCodeNoPeer, CodeNoWorkbenchPeer},
		// A fact about this build, with its own repair.
		{"timeout", workbench.BuildCodeTimeout, CodeDeployableBuildTimeout},
		// Everything else is the author's build script.
		{"failed", workbench.BuildCodeFailed, CodeDeployableBuildFailed},
		{"output missing", workbench.BuildCodeOutputMissing, CodeDeployableBuildFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{answer: &workbench.BuildResult{
				ErrorCode:    tc.from,
				ErrorMessage: "the surface's own sentence",
				LogTail:      "npm ERR! something",
				NodeId:       "workbench-7",
			}}
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())

			out, err := Deploy(context.Background(), h.deps, DeployRequest{
				PackageId: "v1:platform:package:abc",
				Actor:     clusterOwner(),
				Confirmed: true,
				Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
			})
			if err == nil {
				t.Fatal("a refused build must fail the deploy")
			}
			if got := RefusalCode(err); got != tc.want {
				t.Fatalf("want %s, got %s (%v)", tc.want, got, err)
			}
			// Nothing was published: the D6 order's whole promise.
			if len(h.publisher.published) != 0 {
				t.Fatalf("every site must still be serving what it was serving: %v", h.publisher.published)
			}
			// The surface's own sentence reaches the row, and so does the
			// tail -- the OS renders both.
			if !h.engine.sawStatement("the surface's own sentence") {
				t.Error("the surface's sentence must reach the deployment row")
			}
			if !h.engine.sawStatement("npm ERR! something") {
				t.Error("the build tail must reach the deployment row")
			}
			// And WHERE it failed: on a two-replica workbench this is the
			// difference between a bad build script and one sick replica.
			if !h.engine.sawStatement("workbench-7") {
				t.Error("the node the build failed on must reach the row")
			}
			_ = out
		})
	}
}

func TestAPrebuiltAppRecordsThatNothingRan(t *testing.T) {
	runner := &recordingRunner{}
	h := newHarness(t, prebuiltNoDslPackage(), ownerPackage())
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// The fast path is unchanged: the surface is never asked.
	if len(runner.requests) != 0 {
		t.Fatalf("a prebuilt app must not reach the build surface: %v", runner.requests)
	}
	// And the row says so, rather than saying nothing -- which is what makes
	// a four-second deploy legible as a fast one.
	if !h.engine.sawStatement(`"surface":"` + SurfacePrebuilt + `"`) {
		t.Fatalf("the row must record that the output was already built; statements: %v", h.engine.statements())
	}
}

func TestTheOutputIsReadBackUnderThePipelinesOwnCaps(t *testing.T) {
	// A built output over the file cap is refused by THIS side, not merely by
	// the surface: the two enforce the same numbers, so a build surface that
	// forgot its own bound cannot be the way past the publisher's.
	runner := &recordingRunner{answer: &workbench.BuildResult{
		OK:     true,
		NodeId: "workbench-7",
		Output: workbench.BuildBytes{Inline: builtTarGz(map[string]string{
			"dist/index.html": strings.Repeat("x", 4096),
		})},
	}}
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.deps.Limits = Limits{MaxSourceBytes: 1 << 20, MaxFileBytes: 1024, MaxFileCount: 10}
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())

	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     clusterOwner(),
		Confirmed: true,
		Hostnames: map[string]string{"storefront": "shop.example.com", "docs": "docs.example.com"},
	})
	if err == nil {
		t.Fatal("an output over the per-file cap must be refused")
	}
	if len(h.publisher.published) != 0 {
		t.Fatalf("nothing may be published: %v", h.publisher.published)
	}
}

// TestNoOfferedKindReachesTheFleetRoute is task memql#4904's own acceptance
// criterion: the seam ships with a hop test and no consumer.
func TestNoOfferedKindReachesTheFleetRoute(t *testing.T) {
	if !OfferedKindsBuildOnTheWorkbench() {
		t.Fatal("every kind this cluster offers must build in-cluster; a registered kind reaching the fleet route is a design change")
	}
	for _, kind := range []string{KindSPA, KindStatic, KindStorefront} {
		if got := BuildSurfaceFor(kind); got != SurfaceWorkbench {
			t.Errorf("kind %q builds on %q, want %q", kind, got, SurfaceWorkbench)
		}
	}
	// The reachable positive: the table CAN answer fleet, so the assertion
	// above is about the offered kinds rather than about a function that
	// only ever says one thing.
	if got := BuildSurfaceFor("macos"); got != SurfaceFleet {
		t.Fatalf("a kind that needs a Mac must route to the fleet, got %q", got)
	}
	// And a kind no target claims has no surface at all -- never a default.
	if got := BuildSurfaceFor("nonsense"); got != "" {
		t.Fatalf("an unclaimed kind must have no surface, got %q", got)
	}
}

var _ = io.Discard
