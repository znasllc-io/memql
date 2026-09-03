package workbench

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// build_test.go drives the build entry against a real directory, a real shell
// and a real tarball. Nothing here is faked but the mesh.
//
// The entry runs somebody else's command, so the assertions that matter are
// about what it REFUSES and what it does not hand over: the class gate, the
// constructed environment, the timeout, and the directory that is gone
// afterwards whatever happened.

func buildLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// buildIntegration is an Integration whose workspace root is a temp dir.
func buildIntegration(t *testing.T) (*Integration, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(rootEnvVar, root)
	t.Setenv("MEMQL_NODE_ID", "workbench-test-1")
	// The uid drop is not exercised here: the test process is not root, so
	// applyBuildUser reports "cannot change user" and the command runs as the
	// test. Its own coverage is the reason string, below.
	i := NewIntegration(buildLogger())
	return i, root
}

// sourceTarGz packs a fixture tree the way component/packages does: one
// synthesized top-level directory, sorted, no timestamps.
func sourceTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     "src/" + name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Format:   tar.FormatPAX,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

func buildRequest(t *testing.T, command string, files map[string]string) BuildRequest {
	t.Helper()
	return BuildRequest{
		DeploymentId:   "v1:platform:packageDeployment:abc123",
		DeployableName: "storefront",
		OwnerUserId:    "v1:identity:user:alice",
		Path:           "clients/web",
		Command:        command,
		Output:         "dist",
		Source:         BuildBytes{Inline: sourceTarGz(t, files)},
	}
}

// webFixture is the smallest tree that is a deployable: a package directory
// whose build writes an index.html into dist/.
func webFixture() map[string]string {
	return map[string]string{
		"memql-package.yaml":       "formatVersion: 1\nname: acme\n",
		"clients/web/package.json": `{"name":"web"}`,
	}
}

// unpack reads a built output tarball back into a path -> content map.
func unpack(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("output is not gzip: %v", err)
	}
	defer gz.Close()
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			t.Fatalf("output tar: %v", nerr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, rerr := io.ReadAll(tr)
		if rerr != nil {
			t.Fatalf("output read: %v", rerr)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

func TestABuildProducesItsOutputTree(t *testing.T) {
	i, root := buildIntegration(t)
	req := buildRequest(t, "mkdir -p dist && printf '<!doctype html>' > dist/index.html && mkdir -p dist/assets && printf 'x' > dist/assets/app.js", webFixture())

	res := i.RunBuild(context.Background(), req, "")
	if !res.OK {
		t.Fatalf("the build must succeed: %s: %s\n%s", res.ErrorCode, res.ErrorMessage, res.LogTail)
	}
	files := unpack(t, res.Output.Inline)
	if got := files["dist/index.html"]; got != "<!doctype html>" {
		t.Fatalf("the built index.html must come back verbatim, got %q (files: %v)", got, files)
	}
	if _, ok := files["dist/assets/app.js"]; !ok {
		t.Fatalf("every file under the output directory must come back: %v", files)
	}
	if res.FileCount != 2 {
		t.Fatalf("the result must count what it packed, got %d", res.FileCount)
	}
	if res.NodeId != "workbench-test-1" {
		t.Fatalf("the result must name the node that built it, got %q", res.NodeId)
	}

	// THE DIRECTORY IS GONE. It exists for one call, and a build that
	// succeeded leaves exactly as little behind as one that failed.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the build directory must be torn down; root still holds %v", entries)
	}
}

// TestBuildEnvironmentCarriesNoClusterCredential is task memql#4901's own
// acceptance criterion, run as the criterion states it: a fixture whose build
// dumps its environment, and an assertion that every secret this process holds
// is absent from it.
func TestBuildEnvironmentCarriesNoClusterCredential(t *testing.T) {
	// The shapes a node's environment really carries. Set on THIS process, so
	// the assertion is about inheritance rather than about a list: if
	// buildEnv ever went back to appending to os.Environ(), every one of
	// these would appear in the dump.
	secrets := map[string]string{
		"MEMQL_MASTER_KEY":                      "mk_thisisthemasterkey",
		// NOT the shared test DSN literal: this fixture is about a string
		// NEVER reaching the child, so it is deliberately a value nothing
		// else in the tree uses (scripts/cidb's TestNoHardcodedSharedDSNInTests
		// would otherwise count it as a fifteenth copy of the real one).
		"MEMQL_DATABASE_DSN":                    "postgres://fixture:fixture-only@127.0.0.1:1/fixture",
		"MEMQL_AZURE_STORAGE_CONNECTION_STRING": "DefaultEndpointsProtocol=https;AccountKey=deadbeef",
		"MEMQL_OPENAI_API_KEY":                  "sk-test-openai",
		"MEMQL_NODE_BOOTSTRAP_TOKEN":            "bootstrap-secret-value",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}

	i, _ := buildIntegration(t)
	req := buildRequest(t, "mkdir -p dist && env > dist/index.html", webFixture())
	res := i.RunBuild(context.Background(), req, "")
	if !res.OK {
		t.Fatalf("the fixture build must succeed: %s: %s", res.ErrorCode, res.ErrorMessage)
	}
	dump := unpack(t, res.Output.Inline)["dist/index.html"]

	// The reachable positive FIRST: the dump has to be a real environment, or
	// an empty file would pass every assertion below.
	if !strings.Contains(dump, "MEMQL_BUILD_DEPLOYMENT_ID=") {
		t.Fatalf("the dump is not an environment this build was given: %q", dump)
	}
	for name, value := range secrets {
		if strings.Contains(dump, name+"=") {
			t.Errorf("the build's environment names %s", name)
		}
		if strings.Contains(dump, value) {
			t.Errorf("the build's environment carries the VALUE of %s", name)
		}
	}
}

func TestAFailedBuildKeepsItsOutputAndRefusesTyped(t *testing.T) {
	i, _ := buildIntegration(t)
	req := buildRequest(t, "echo 'npm ERR! missing script: build' >&2 && exit 7", webFixture())

	res := i.RunBuild(context.Background(), req, "")
	if res.OK {
		t.Fatal("a command that exits non-zero must not report a successful build")
	}
	if res.ErrorCode != BuildCodeFailed {
		t.Fatalf("want %s, got %s", BuildCodeFailed, res.ErrorCode)
	}
	if res.ExitCode != 7 {
		t.Fatalf("the exit status must survive, got %d", res.ExitCode)
	}
	// The tail is what answers "why did my build fail" inside the OS.
	if !strings.Contains(res.LogTail, "missing script: build") {
		t.Fatalf("stderr must reach the tail, got %q", res.LogTail)
	}
}

func TestABuildPastItsTimeoutIsStoppedAndSaysSo(t *testing.T) {
	i, _ := buildIntegration(t)
	req := buildRequest(t, "sleep 30", webFixture())
	req.TimeoutSec = 1

	started := time.Now()
	res := i.RunBuild(context.Background(), req, "")
	if res.OK {
		t.Fatal("a build past its timeout must not succeed")
	}
	if res.ErrorCode != BuildCodeTimeout {
		t.Fatalf("want %s, got %s: %s", BuildCodeTimeout, res.ErrorCode, res.ErrorMessage)
	}
	// Killed rather than waited out: the assertion is that it did not take
	// the sleep's own thirty seconds.
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("the command must be killed at the timeout, not waited out; took %s", elapsed)
	}
}

func TestABuildThatWritesNoOutputSaysThatRatherThanSucceeding(t *testing.T) {
	i, _ := buildIntegration(t)
	// The command SUCCEEDS and produces nothing. Reported as its own code
	// because "your build script does not write dist/" is a different repair
	// from "your build script failed".
	req := buildRequest(t, "true", webFixture())

	res := i.RunBuild(context.Background(), req, "")
	if res.OK {
		t.Fatal("a build with no output directory must not report success")
	}
	if res.ErrorCode != BuildCodeOutputMissing {
		t.Fatalf("want %s, got %s", BuildCodeOutputMissing, res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMessage, "dist") {
		t.Fatalf("the sentence must name the directory it looked for, got %q", res.ErrorMessage)
	}
}

func TestABuildIsRefusedBeforeItRunsWhenTheRequestIsIncomplete(t *testing.T) {
	i, root := buildIntegration(t)
	cases := map[string]func(*BuildRequest){
		"no command":     func(r *BuildRequest) { r.Command = "" },
		"no source":      func(r *BuildRequest) { r.Source = BuildBytes{} },
		"no deployment":  func(r *BuildRequest) { r.DeploymentId = "" },
		"no owner":       func(r *BuildRequest) { r.OwnerUserId = "" },
		"escaping path":  func(r *BuildRequest) { r.Path = "../../etc" },
		"escaping outpt": func(r *BuildRequest) { r.Output = "../../etc" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := buildRequest(t, "true", webFixture())
			mutate(&req)
			res := i.RunBuild(context.Background(), req, "")
			if res.OK || res.ErrorCode != BuildCodeInvalid {
				t.Fatalf("want %s, got ok=%v %s", BuildCodeInvalid, res.OK, res.ErrorCode)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatalf("nothing may be provisioned for a refused request; root holds %v", entries)
			}
		})
	}
}

// TestTheBuildActionIsNotAWorkbenchHostAction pins the half of the entry's
// reachability that lives on the agent side: a tool call naming `build` is
// refused by name, before the remote branch can forward it anywhere.
func TestTheBuildActionIsNotAWorkbenchHostAction(t *testing.T) {
	i, _ := buildIntegration(t)
	nodes, err := i.handleDispatchHost(context.Background(), map[string]any{
		"action": BuildAction,
		"planId": "v1:planner:plan:p1",
		"args":   map[string]any{"cmd": "echo hi"},
	}, 0)
	if err != nil {
		t.Fatalf("the refusal must be structured, not an error: %v", err)
	}
	var res dispatchResult
	if uerr := json.Unmarshal(nodes[0].Payload, &res); uerr != nil {
		t.Fatalf("payload: %v", uerr)
	}
	if res.OK || res.ErrorCode != "unknown_action" {
		t.Fatalf("want unknown_action, got ok=%v %s", res.OK, res.ErrorCode)
	}
	// The reachable positive: a real action on the same path is NOT refused
	// this way, so the assertion above is about `build` rather than about
	// every call this fixture makes.
	nodes, err = i.handleDispatchHost(context.Background(), map[string]any{
		"action": "fs_list",
		"planId": "v1:planner:plan:p1",
		"args":   map[string]any{"path": "."},
	}, 0)
	if err != nil {
		t.Fatalf("fs_list: %v", err)
	}
	// A FRESH value, not the one above. dispatchResult omits errorCode when
	// empty, so unmarshalling a success over a refusal leaves the refusal's
	// code in place -- and the assertion would pass against a handler that
	// refused everything.
	var listed dispatchResult
	if uerr := json.Unmarshal(nodes[0].Payload, &listed); uerr != nil {
		t.Fatalf("payload: %v", uerr)
	}
	if listed.ErrorCode == "unknown_action" {
		t.Fatal("fs_list must not be refused as an unknown action")
	}
}

// ---------------------------------------------------------------------------
// The hop: an originating node to a workbench node and back
// ---------------------------------------------------------------------------

// buildMesh wires a real ForwardHandler behind a buildForwarder, with no
// PeerManager and no network -- the shape forward_authority_test.go
// established, extended to carry the response back to the caller.
type buildMesh struct {
	t       *testing.T
	handler *ForwardHandler
	// pinned records the affinity the originating side asked for, which is
	// how a test asserts the pin is passed through rather than dropped.
	pinned    []string
	reachable bool
}

func (m *buildMesh) SelfNodeId() string   { return "bff-1" }
func (m *buildMesh) SelfNodeType() string { return "bff" }

func (m *buildMesh) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error) {
	m.pinned = append(m.pinned, pinnedNodeId)
	if !m.reachable {
		return nil, "", ErrNoWorkbenchPeer
	}
	if req.GetRequestId() == "" {
		req.RequestId = id.NewShortId()
	}
	var got *nodev1.WorkbenchForwardResponse
	m.handler.HandleForwardedRequest(ctx, req, func(msg *nodev1.NodeServerMessage) error {
		got = msg.GetWorkbenchForwardResponse()
		return nil
	})
	if got == nil {
		m.t.Fatal("the workbench side sent no response")
	}
	return got, "workbench-test-1", nil
}

func newBuildMesh(t *testing.T, workbenchSide *Integration) *buildMesh {
	t.Helper()
	return &buildMesh{t: t, handler: NewForwardHandler(workbenchSide, buildLogger()), reachable: true}
}

func TestABuildCrossesToTheWorkbenchNodeAndComesBack(t *testing.T) {
	// TWO integrations, as in the cluster: the originating node holds no
	// workspace root worth anything, and the workbench node is where the
	// directory and the shell are.
	workbenchSide, root := buildIntegration(t)
	mesh := newBuildMesh(t, workbenchSide)

	origin := NewIntegration(buildLogger())
	origin.remote = true
	origin.SetBuildForwarder(mesh)

	req := buildRequest(t, "mkdir -p dist && printf 'hello' > dist/index.html", webFixture())
	res := origin.RunBuild(context.Background(), req, "workbench-test-9")
	if !res.OK {
		t.Fatalf("the forwarded build must succeed: %s: %s\n%s", res.ErrorCode, res.ErrorMessage, res.LogTail)
	}
	if got := unpack(t, res.Output.Inline)["dist/index.html"]; got != "hello" {
		t.Fatalf("the built bytes must survive the hop, got %q", got)
	}
	if res.NodeId != "workbench-test-1" {
		t.Fatalf("the result must name the node that built it, got %q", res.NodeId)
	}
	// THE PIN IS PASSED THROUGH. It is a preference the picker applies, and a
	// caller that dropped it would send the run's second app to a different
	// replica with nothing saying so.
	if len(mesh.pinned) != 1 || mesh.pinned[0] != "workbench-test-9" {
		t.Fatalf("the affinity pin must reach the picker, got %v", mesh.pinned)
	}
	// The workbench node's directory is gone; the ORIGIN never made one.
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("the build directory must be torn down, root holds %v", entries)
	}
}

func TestAnUnreachableWorkbenchRefusesRatherThanBuildingHere(t *testing.T) {
	workbenchSide, _ := buildIntegration(t)
	mesh := newBuildMesh(t, workbenchSide)
	mesh.reachable = false

	localRoot := t.TempDir()
	t.Setenv(rootEnvVar, localRoot)
	origin := NewIntegration(buildLogger())
	origin.remote = true
	origin.SetBuildForwarder(mesh)

	res := origin.RunBuild(context.Background(), buildRequest(t, "mkdir -p dist && touch dist/index.html", webFixture()), "")
	if res.OK {
		t.Fatal("an unreachable workbench must refuse, not build here")
	}
	if res.ErrorCode != BuildCodeNoPeer {
		t.Fatalf("want %s, got %s", BuildCodeNoPeer, res.ErrorCode)
	}
	// The message names what an operator needs, and nothing ran.
	for _, want := range []string{"MEMQL_WORKER_PEERS", "MEMQL_WORKBENCH_REMOTE"} {
		if !strings.Contains(res.ErrorMessage, want) {
			t.Errorf("the refusal must name %s: %q", want, res.ErrorMessage)
		}
	}
	if entries, _ := os.ReadDir(localRoot); len(entries) != 0 {
		t.Fatalf("a refused build must provision nothing locally; root holds %v", entries)
	}
}

func TestTheLocalFallbackIsReachableOnlyByAskingForIt(t *testing.T) {
	// The reachable positive for the refusal above: the SAME unreachable
	// mesh, with the operator's explicit opt-in, builds here.
	workbenchSide, _ := buildIntegration(t)
	mesh := newBuildMesh(t, workbenchSide)
	mesh.reachable = false

	origin, _ := buildIntegration(t)
	origin.remote = true
	origin.localFallback = true
	origin.SetBuildForwarder(mesh)

	res := origin.RunBuild(context.Background(), buildRequest(t, "mkdir -p dist && printf 'local' > dist/index.html", webFixture()), "")
	if !res.OK {
		t.Fatalf("with the opt-in set the build must run here: %s: %s", res.ErrorCode, res.ErrorMessage)
	}
	if got := unpack(t, res.Output.Inline)["dist/index.html"]; got != "local" {
		t.Fatalf("got %q", got)
	}
}

// TestTheBuildEntryAnswersOnlyToTheEngine is the class gate: a `build` forward
// carrying any assertion but the engine's own runs nothing.
func TestTheBuildEntryAnswersOnlyToTheEngine(t *testing.T) {
	workbenchSide, root := buildIntegration(t)
	handler := NewForwardHandler(workbenchSide, buildLogger())

	payload, err := json.Marshal(buildRequest(t, "mkdir -p dist && touch dist/index.html", webFixture()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A USER assertion -- exactly what every agent tool-loop forward carries,
	// which is what makes this the case worth pinning.
	userAuthority, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: "v1:identity:user:mallory", Role: auth.RoleWriter},
		auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("authority: %v", err)
	}
	var got *nodev1.WorkbenchForwardResponse
	handler.HandleForwardedRequest(context.Background(), &nodev1.WorkbenchForwardRequest{
		RequestId: "r1",
		PlanId:    "deployment:abc-storefront",
		Action:    BuildAction,
		ArgsJson:  payload,
		Authority: node.ForwardedAuthorityToProto(userAuthority, "agent-1", "agent"),
	}, func(msg *nodev1.NodeServerMessage) error {
		got = msg.GetWorkbenchForwardResponse()
		return nil
	})
	if got == nil {
		t.Fatal("the refusal must be sent, not dropped")
	}
	if got.GetErrorCode() != BuildCodeEntryRefused {
		t.Fatalf("want %s, got %s: %s", BuildCodeEntryRefused, got.GetErrorCode(), got.GetErrorMessage())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("nothing may run for a refused entry; root holds %v", entries)
	}

	// THE REACHABLE POSITIVE. The same request under the engine's own
	// assertion builds -- so the assertion above is about the class rather
	// than about a handler that refuses everything.
	systemAuthority, err := auth.ForwardedAuthorityForSystem("workbench-build:bff-1", time.Now())
	if err != nil {
		t.Fatalf("system authority: %v", err)
	}
	got = nil
	handler.HandleForwardedRequest(context.Background(), &nodev1.WorkbenchForwardRequest{
		RequestId: "r2",
		PlanId:    "deployment:abc-storefront",
		Action:    BuildAction,
		ArgsJson:  payload,
		Authority: node.ForwardedAuthorityToProto(systemAuthority, "bff-1", "bff"),
	}, func(msg *nodev1.NodeServerMessage) error {
		got = msg.GetWorkbenchForwardResponse()
		return nil
	})
	if got == nil || got.GetErrorCode() != "" {
		t.Fatalf("the engine's own assertion must be admitted, got %+v", got)
	}
}
