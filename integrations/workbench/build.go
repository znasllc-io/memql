package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// build.go is the workbench's SECOND entry, beside handleDispatchHost: one
// deployable's build for the packages pipeline (epic memql#4900, task #4901).
//
// ===========================================================================
// WHY A SECOND ENTRY RATHER THAN SIX MORE TOOL CALLS
// ===========================================================================
// The agent tool loop reaches this integration through workbenchHost, keyed on
// a Plan, with a per-Plan workspace and the exec allowlist between it and the
// shell. A package build is a different caller with a different contract: the
// command is the manifest's (somebody else's code, run where that is assumed
// rather than hoped), the key is the DEPLOYMENT, the owner is the package's,
// and nothing a model says can reach it. So the entry has no DSL construct at
// all -- it is a Go seam, RunBuild, and the only wire form it takes is a
// WorkbenchForwardRequest whose action is `build` under a SYSTEM-class
// authority (auth.ForwardedAuthorityForSystem: a node acting for itself, which
// no tool-loop forward can mint because those re-assert the caller's own
// class). The workbench-side handler refuses `build` under any other class,
// and handleDispatchHost refuses the action by name before it forwards, so a
// model naming it never leaves the agent.
//
// ===========================================================================
// ONE CALL, ONE DIRECTORY, NOTHING SHARED
// ===========================================================================
// A build directory lives for exactly one call: provisioned, filled from the
// snapshot, built, packed, torn down -- whether the build succeeded, failed or
// timed out. No v1:workbench:workspace row is written for it (that row's
// planId! and its parent relationship to a plan would point at nothing), no
// teardown waits on the deployment's terminal status, and a node lost mid-run
// leaks nothing but a directory on a pod that is already gone. Affinity is a
// preference the CALLER carries between the deployables of one run (the node
// that built the first is preferred for the second), never a correctness
// input.
//
// ===========================================================================
// THE ENVIRONMENT IS CONSTRUCTED, NEVER INHERITED
// ===========================================================================
// handleExec appends to os.Environ(), which on a node is the node's whole
// credential set. A build gets buildEnv: PATH, a HOME inside its own directory,
// a TMPDIR, the locale, CI=true, npm's cache pointed inside the directory, and
// two MEMQL_BUILD_* facts about the run. TestBuildEnvironmentCarriesNoClusterCredential
// dumps `env` from inside a build and asserts every secret the test process
// holds is absent.

// BuildAction is the WorkbenchForwardRequest.action this entry answers to.
const BuildAction = "build"

// The build entry's own error codes. Stable: the packages pipeline maps them
// onto its refusal catalogue, and the OS renders copy keyed on what the
// pipeline emits.
const (
	// BuildCodeInvalid: the request named no command, no source, or a path
	// that escapes the tree. A caller error; nothing ran.
	BuildCodeInvalid = "build_request_invalid"
	// BuildCodeSourceUnreadable: the snapshot bytes could not be read or
	// expanded, or the deployable's path is not a directory in them.
	BuildCodeSourceUnreadable = "build_source_unreadable"
	// BuildCodeFailed: the command exited non-zero. The tail says why.
	BuildCodeFailed = "build_failed"
	// BuildCodeTimeout: the command outlived its timeout and was killed,
	// process group and all.
	BuildCodeTimeout = "build_timeout"
	// BuildCodeOutputMissing: the command succeeded and left no output
	// directory where the manifest said it would.
	BuildCodeOutputMissing = "build_output_missing"
	// BuildCodeOutputTooLarge: the output tree exceeds the publisher-grade
	// limits the request carried.
	BuildCodeOutputTooLarge = "build_output_too_large"
	// BuildCodeNoPeer: MEMQL_WORKBENCH_REMOTE is set and no workbench peer is
	// reachable. The same refusal handleDispatchHost gives, for the same
	// reason (memql#3506): running here is not the isolation that flag asks
	// for.
	BuildCodeNoPeer = "no_workbench_peer"
	// BuildCodeForwardFailed: the hop to the workbench node failed for a
	// reason other than "no peer" -- a stream that closed mid-build, an
	// unreadable reply.
	BuildCodeForwardFailed = "build_forward_failed"
	// BuildCodeEntryRefused: a `build` forward arrived under an authority
	// that is not the engine's own. Nothing ran.
	BuildCodeEntryRefused = "build_entry_refused"
)

const (
	// DefaultBuildTimeout bounds one deployable's command when the request
	// carries none. Fifteen minutes is generous for `npm ci && npm run build`
	// and short enough that a wedged build does not hold a run for an hour.
	DefaultBuildTimeout = 15 * time.Minute
	// MaxBuildTimeout is the ceiling a request may ask for.
	MaxBuildTimeout = 2 * time.Hour
	// DefaultBuildLogTailBytes bounds the tail returned on the result. The
	// row keeps this; the store keeps every line.
	DefaultBuildLogTailBytes = 64 << 10
	// DefaultBuildInlineBytes is the largest tarball that rides inline in a
	// forward request or response. The mesh's message cap is 32 MiB and JSON
	// base64 costs a third, so 8 MiB leaves room for the rest of the envelope.
	DefaultBuildInlineBytes = 8 << 20
	// buildLogLineCap bounds how many lines of build output one build logs
	// through slog. Past it the count of dropped lines is logged once.
	buildLogLineCap = 2000
	// buildSubjectConcept is the concept a build's log lines are ABOUT
	// (the Logs epic's `subject` seam, memql#4893): every line carries the
	// deployment as its subject, so the Build stop's "open in Logs" is one
	// filter rather than a search.
	buildSubjectConcept = "v1:platform:packageDeployment"
	// buildOutputTop is the top-level directory name the packed output tree
	// carries, so the pipeline's extractor strips it exactly as it strips
	// GitHub's synthesized root.
	buildOutputTop = "dist"
)

// BuildRequest is one deployable's build: what to run, on which bytes, under
// which limits.
type BuildRequest struct {
	// DeploymentId is the v1:platform:packageDeployment this build belongs
	// to. It keys the build directory and names the subject on every log line.
	DeploymentId string `json:"deploymentId"`
	// DeployableName is the manifest's name for the app being built.
	DeployableName string `json:"deployableName"`
	// OwnerUserId is the package owner. The build writes no row of its own;
	// the field rides along so the workbench node's log lines and any future
	// bookkeeping name the person the directory belongs to.
	OwnerUserId string `json:"ownerUserId"`
	// Path is the deployable's directory inside the source tree, and Command
	// runs there. Output is the built tree, relative to Path.
	Path    string `json:"path"`
	Command string `json:"command"`
	Output  string `json:"output"`
	// Source is the snapshot: a tar.gz of the whole package tree, with or
	// without a single synthesized top-level directory (GitHub's form and the
	// pipeline's own agree once extracted).
	Source BuildBytes `json:"source"`
	// TimeoutSec bounds the command. Zero takes DefaultBuildTimeout; the
	// workbench clamps to MaxBuildTimeout.
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Limits bound the tree in both directions.
	Limits BuildLimits `json:"limits"`
}

// BuildBytes names where a tarball is: inline on the wire, or at a blob
// reference (`blob://<object>` under the cluster's own container) when it is
// too large to ride a mesh message.
type BuildBytes struct {
	Ref    string `json:"ref,omitempty"`
	Inline []byte `json:"inline,omitempty"`
}

// Empty reports whether nothing was named.
func (b BuildBytes) Empty() bool { return strings.TrimSpace(b.Ref) == "" && len(b.Inline) == 0 }

// BuildLimits bound the source and the output. Zero fields take the defaults
// below, which are the packages publisher's numbers.
type BuildLimits struct {
	MaxSourceBytes  int64 `json:"maxSourceBytes,omitempty"`
	MaxOutputBytes  int64 `json:"maxOutputBytes,omitempty"`
	MaxFileBytes    int64 `json:"maxFileBytes,omitempty"`
	MaxFileCount    int   `json:"maxFileCount,omitempty"`
	MaxLogTailBytes int   `json:"maxLogTailBytes,omitempty"`
	MaxInlineBytes  int64 `json:"maxInlineBytes,omitempty"`
}

const (
	defaultBuildMaxSourceBytes int64 = 500 * 1024 * 1024
	defaultBuildMaxFileBytes   int64 = 25 * 1024 * 1024
	defaultBuildMaxFileCount         = 20000
)

func (l BuildLimits) normalized() BuildLimits {
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = defaultBuildMaxSourceBytes
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = defaultBuildMaxSourceBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = defaultBuildMaxFileBytes
	}
	if l.MaxFileCount <= 0 {
		l.MaxFileCount = defaultBuildMaxFileCount
	}
	if l.MaxLogTailBytes <= 0 {
		l.MaxLogTailBytes = DefaultBuildLogTailBytes
	}
	if l.MaxInlineBytes <= 0 {
		l.MaxInlineBytes = DefaultBuildInlineBytes
	}
	return l
}

// BuildResult is what one build produced. Every outcome is typed: OK with the
// output, or an ErrorCode from the list above with the tail captured up to
// the moment it stopped.
type BuildResult struct {
	OK bool `json:"ok"`
	// NodeId is the workbench replica that ran the command -- the fact the
	// deployment row records as builtOn.nodeId and the caller uses as the
	// affinity pin for the run's next deployable.
	NodeId       string `json:"nodeId"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	// LogTail is the last MaxLogTailBytes of stdout and stderr, interleaved
	// as they arrived.
	LogTail    string `json:"logTail"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	// Output is the built tree as a tar.gz with a `dist/` top-level
	// directory, inline or by reference.
	Output      BuildBytes `json:"output"`
	FileCount   int        `json:"fileCount"`
	OutputBytes int64      `json:"outputBytes"`
}

func buildRefusal(code, msg string) BuildResult {
	return BuildResult{OK: false, ErrorCode: code, ErrorMessage: msg}
}

// buildForwarder is the slice of ForwardRouter the build entry needs. An
// interface so the in-process hop test can stand a workbench-side handler
// behind it without a PeerManager, which cannot be built outside
// component/node.
type buildForwarder interface {
	Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error)
	SelfNodeId() string
	SelfNodeType() string
}

// buildBlobStore is the slice of the blob uploader the transports need when a
// tarball is too large to ride inline.
type buildBlobStore interface {
	Upload(ctx context.Context, container, objectName string, data []byte, contentType string) (string, error)
	Download(ctx context.Context, container, objectName string) ([]byte, error)
}

// SetBuildForwarder installs the hop the build entry forwards over. The
// production wiring installs the ForwardRouter through SetForwardRouter and
// never calls this; it exists for the hop test.
func (i *Integration) SetBuildForwarder(f buildForwarder) {
	i.buildForwarder = f
}

// SetBuildBlobStore installs the object store the transports use for a
// tarball past the inline cap, with the container it writes under. Production
// resolves the cluster's own uploader lazily; a test injects a map.
func (i *Integration) SetBuildBlobStore(store buildBlobStore, container string) {
	i.buildBlob = store
	i.buildContainer = strings.TrimSpace(container)
}

func (i *Integration) forwarder() buildForwarder {
	if i.buildForwarder != nil {
		return i.buildForwarder
	}
	if i.router != nil {
		return i.router
	}
	return nil
}

// RunBuild runs one deployable's build and answers with a typed result. It is
// the whole of the entry's Go surface.
//
// In remote mode it forwards to a workbench peer (preferring pinnedNodeId when
// that replica is healthy) and, exactly as handleDispatchHost does, REFUSES
// rather than running here when no peer is reachable -- unless the operator
// asked for local execution by name (MEMQL_WORKBENCH_LOCAL_FALLBACK).
func (i *Integration) RunBuild(ctx context.Context, req BuildRequest, pinnedNodeId string) BuildResult {
	if err := req.validate(); err != nil {
		return buildRefusal(BuildCodeInvalid, "workbench build: "+err.Error())
	}
	if i.remote {
		if fwd := i.forwarder(); fwd != nil {
			res, err := i.forwardBuild(ctx, fwd, req, pinnedNodeId)
			if !errors.Is(err, ErrNoWorkbenchPeer) {
				if err != nil {
					return buildRefusal(BuildCodeForwardFailed, "workbench build: the hop to the workbench node failed: "+err.Error())
				}
				return res
			}
		}
		if !i.localFallback {
			return i.refuseNoWorkbenchPeerForBuild(ctx, req)
		}
		if i.logger != nil {
			i.logger.Warn("workbench build: no remote peer; building LOCALLY because MEMQL_WORKBENCH_LOCAL_FALLBACK is set. This is not the isolation MEMQL_WORKBENCH_REMOTE asks for.",
				slog.String("deployment", req.DeploymentId), slog.String("deployable", req.DeployableName))
		}
	}
	return i.runBuildLocal(ctx, req)
}

func (i *Integration) refuseNoWorkbenchPeerForBuild(ctx context.Context, req BuildRequest) BuildResult {
	msg := "MEMQL_WORKBENCH_REMOTE is set, so this build must run on a workbench node, but no healthy " +
		"workbench peer is reachable. Check that a workbench node is running and that MEMQL_WORKER_PEERS names it " +
		"(e.g. MEMQL_WORKER_PEERS=workbench=workbench:50060). Refusing rather than building on this node, because " +
		"running somebody else's build script here is not the isolation MEMQL_WORKBENCH_REMOTE asks for."
	if i.logger != nil {
		i.logger.LogAttrs(ctx, slog.LevelError, "workbench build: refusing -- remote required, no peer reachable",
			slog.String("deployment", req.DeploymentId),
			slog.String("deployable", req.DeployableName),
			slog.String("remedy", "MEMQL_WORKER_PEERS=workbench=<addr>"),
		)
	}
	return buildRefusal(BuildCodeNoPeer, msg)
}

// forwardBuild is the originating side of the hop.
func (i *Integration) forwardBuild(ctx context.Context, fwd buildForwarder, req BuildRequest, pinnedNodeId string) (BuildResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return BuildResult{}, fmt.Errorf("encoding the build request: %w", err)
	}
	// THE SYSTEM CLASS IS THE SIGNAL. A tool-loop forward re-asserts the
	// caller's own authority (a user, a badge, an app session); this one is
	// the node acting for itself, which VerifyForwardedAuthority admits only
	// under ForwardedClassSystem and only with a subject that is not a person.
	// The receiving handler refuses `build` under any other class, so the
	// class inside the signed assertion is what makes "this came from the
	// engine" checkable rather than assumed.
	authority, err := auth.ForwardedAuthorityForSystem(buildAuthoritySubject(fwd.SelfNodeId()), time.Now())
	if err != nil {
		return BuildResult{}, err
	}
	wire := &nodev1.WorkbenchForwardRequest{
		PlanId:     buildKey(req),
		Action:     BuildAction,
		ArgsJson:   payload,
		TimeoutSec: int32(req.timeout() / time.Second),
		Authority:  node.ForwardedAuthorityToProto(authority, fwd.SelfNodeId(), fwd.SelfNodeType()),
	}
	// The hop waits for the whole build, plus a minute for the bytes to move.
	// A workbench that hangs past that is a failed build, not a parked run.
	hopCtx, cancel := context.WithTimeout(ctx, req.timeout()+time.Minute)
	defer cancel()
	resp, servedBy, err := fwd.Forward(hopCtx, wire, pinnedNodeId)
	if err != nil {
		return BuildResult{}, err
	}
	if len(resp.GetPayloadJson()) == 0 {
		if resp.GetErrorCode() != "" {
			return buildRefusal(resp.GetErrorCode(), resp.GetErrorMessage()), nil
		}
		return buildRefusal(BuildCodeForwardFailed, "the workbench node answered with no result"), nil
	}
	var res BuildResult
	if err := json.Unmarshal(resp.GetPayloadJson(), &res); err != nil {
		return buildRefusal(BuildCodeForwardFailed, "the workbench node's reply could not be read: "+err.Error()), nil
	}
	if strings.TrimSpace(res.NodeId) == "" {
		res.NodeId = servedBy
	}
	return res, nil
}

// buildAuthoritySubject names the engine as the principal of a build forward.
// Never a user id: ForwardedAuthorityForSystem refuses one, and the point of
// the class is that no person is being impersonated.
func buildAuthoritySubject(selfNodeId string) string {
	self := strings.TrimSpace(selfNodeId)
	if self == "" {
		self = "unnamed"
	}
	return "workbench-build:" + self
}

// buildKey is the directory name under MEMQL_WORKBENCH_ROOT and the plan_id
// slot of the forward request: keyed on the DEPLOYMENT, as the design fixes,
// with the deployable's name so a package's apps never share a tree.
func buildKey(req BuildRequest) string {
	return "deployment:" + shortId(req.DeploymentId) + "-" + safeName(req.DeployableName)
}

var unsafeNameRune = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func safeName(name string) string {
	clean := unsafeNameRune.ReplaceAllString(strings.TrimSpace(name), "_")
	if clean == "" {
		clean = "app"
	}
	if len(clean) > 64 {
		clean = clean[:64]
	}
	return clean
}

func shortId(canonical string) string {
	canonical = strings.TrimSpace(canonical)
	if i := strings.LastIndexByte(canonical, ':'); i >= 0 && i+1 < len(canonical) {
		return canonical[i+1:]
	}
	return canonical
}

func (r BuildRequest) timeout() time.Duration {
	if r.TimeoutSec <= 0 {
		return DefaultBuildTimeout
	}
	d := time.Duration(r.TimeoutSec) * time.Second
	if d > MaxBuildTimeout {
		return MaxBuildTimeout
	}
	return d
}

func (r BuildRequest) validate() error {
	if strings.TrimSpace(r.DeploymentId) == "" {
		return errors.New("the request names no deployment")
	}
	if strings.TrimSpace(r.DeployableName) == "" {
		return errors.New("the request names no deployable")
	}
	if strings.TrimSpace(r.OwnerUserId) == "" {
		return errors.New("the request names no owner")
	}
	if strings.TrimSpace(r.Command) == "" {
		return errors.New("the request carries no build command")
	}
	if _, err := cleanTreePath(r.Path); err != nil {
		return fmt.Errorf("the deployable path %q: %w", r.Path, err)
	}
	if _, err := cleanTreePath(r.Output); err != nil {
		return fmt.Errorf("the output path %q: %w", r.Output, err)
	}
	if r.Source.Empty() {
		return errors.New("the request carries no source")
	}
	return nil
}

// cleanTreePath validates a manifest path as a relative, non-escaping path
// inside the tree and returns its slash form. "" and "." mean the tree root.
//
// IT REFUSES AN ESCAPE RATHER THAN NORMALISING ONE. The natural spelling --
// Clean("/" + p) and then trim the slash -- collapses `../../etc` into `etc`,
// because Clean treats `..` at the root as a no-op. That is SAFE, in that
// nothing lands outside the tree, and it is wrong: the manifest said one thing
// and the build would quietly do another, which is how somebody spends an hour
// wondering why their `../shared` path built the wrong directory. So the
// cleaning happens on the RELATIVE path, where `..` still means what it says.
func cleanTreePath(p string) (string, error) {
	raw := strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("is absolute")
	}
	if raw == "" {
		return ".", nil
	}
	clean := path.Clean(raw)
	if clean == "." {
		return ".", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || !fs.ValidPath(clean) {
		return "", errors.New("escapes the tree")
	}
	return clean, nil
}

// ---------------------------------------------------------------------------
// The local run
// ---------------------------------------------------------------------------

// runBuildLocal is the build itself, on THIS node: the forwarded path lands
// here on the workbench replica, and a non-remote node lands here directly.
func (i *Integration) runBuildLocal(ctx context.Context, req BuildRequest) BuildResult {
	started := time.Now()
	limits := req.Limits.normalized()
	self := selfNodeId()
	finish := func(res BuildResult) BuildResult {
		res.NodeId = self
		res.DurationMs = time.Since(started).Milliseconds()
		return res
	}

	// THE WORKSPACE ROOT IS OPENED, NOT JOINED INTO.
	//
	// Provisioning and tearing down the run directory are filesystem writes
	// like any other in this file, so they are addressed root-relative like
	// every other one. This is UNIFORMITY, not a fix: `RemoveAll` deletes a
	// symlink as the link it is either way, so no escape was open here. What
	// it buys is that no site in this file is left for somebody to re-derive
	// whether a string check was the right tool -- a question this surface
	// got wrong once, on the OUTPUT directory, where `..` being a property of
	// the string and a symlink a property of the filesystem was the whole
	// bug. `containedJoin` still runs, because the ABSOLUTE path is what the
	// command's working directory and the chown walk need; it is no longer
	// what any write is addressed by.
	root := i.manager.Root()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: could not provision the workspace root: "+err.Error()))
	}
	wsRoot, wserr := os.OpenRoot(root)
	if wserr != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: the workspace root could not be opened: "+wserr.Error()))
	}
	defer wsRoot.Close()

	key := buildKey(req)
	dir, derr := containedJoin(root, key)
	if derr != nil {
		return finish(buildRefusal(BuildCodeInvalid, "workbench build: "+derr.Error()))
	}
	// A LEFTOVER FROM AN EARLIER RUN IS REMOVED THROUGH THE ROOT, so a
	// symlink standing where the directory should be is refused rather than
	// followed -- and removed as the link it is rather than emptying whatever
	// it points at.
	_ = wsRoot.RemoveAll(key)
	if err := wsRoot.MkdirAll(path.Join(key, "tmp"), 0o755); err != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: could not provision the build directory: "+err.Error()))
	}
	// The source directory is made HERE rather than by the extractor: an
	// os.Root cannot confine the creation of its own directory, so the one
	// mkdir that has to happen outside a root happens under the root above.
	if err := wsRoot.MkdirAll(path.Join(key, "src"), 0o755); err != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: could not provision the source directory: "+err.Error()))
	}
	// TORN DOWN WHATEVER HAPPENS. The directory is the whole of the build's
	// footprint on this replica; a build that failed, timed out or panicked
	// leaves exactly as much behind as one that succeeded: nothing.
	defer func() { _ = wsRoot.RemoveAll(key) }()

	src, err := i.buildBytes(ctx, req.Source)
	if err != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: the source snapshot could not be read: "+err.Error()))
	}
	// `src` and `tmp` are literals under a directory this function just made,
	// so they need no validation of their own -- what needs it is the
	// deployable's PATH out of the manifest, below.
	srcDir := filepath.Join(dir, "src")
	topDir, err := extractTarGz(bytes.NewReader(src), srcDir, limits.MaxFileCount, limits.MaxFileBytes, limits.MaxSourceBytes)
	if err != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: the source snapshot could not be expanded: "+err.Error()))
	}
	// EVERYTHING FROM HERE IS RELATIVE TO A ROOT. The manifest's path and
	// output directory are the untrusted strings in this function, and they
	// are never joined into a filesystem call -- they are handed to os.Root,
	// which refuses a name resolving outside its directory even through a
	// symlink the build itself created.
	srcRoot, rerr := os.OpenRoot(srcDir)
	if rerr != nil {
		return finish(buildRefusal(BuildCodeSourceUnreadable, "workbench build: the expanded source could not be opened: "+rerr.Error()))
	}
	defer srcRoot.Close()

	workRel, err := treeRelative(topDir, req.Path)
	if err != nil {
		return finish(buildRefusal(BuildCodeInvalid, "workbench build: "+err.Error()))
	}
	if info, serr := srcRoot.Stat(workRel); serr != nil || !info.IsDir() {
		return finish(buildRefusal(BuildCodeSourceUnreadable,
			fmt.Sprintf("workbench build: the deployable's path %q is not a directory in the snapshot", req.Path)))
	}
	// The ONE real path this function composes from untrusted input, and it
	// exists because exec.Cmd.Dir takes a path rather than a directory
	// handle. Every component of it has been through safeName/cleanTreePath
	// AND been resolved by os.Root above, so what is joined here is a name
	// the kernel has already confirmed lands inside srcDir.
	workdir, jerr := containedJoin(srcDir, workRel)
	if jerr != nil {
		return finish(buildRefusal(BuildCodeInvalid, "workbench build: "+jerr.Error()))
	}

	tail := newTailBuffer(limits.MaxLogTailBytes)
	lines := i.newBuildLineLogger(ctx, req)
	sink := io.MultiWriter(tail, lines)

	cctx, cancel := context.WithTimeout(ctx, req.timeout())
	defer cancel()
	// ===================================================================
	// YES, THIS RUNS A COMMAND FROM AN UNTRUSTED SOURCE. THAT IS THE JOB.
	// ===================================================================
	// Static analysis flags this line, correctly, as a command built from
	// user-controlled input -- and there is no version of this feature where
	// it is not. `npm ci && npm run build` comes out of somebody's
	// memql-package.yaml, and deploying their package means running it.
	//
	// So the answer is not to sanitise the command, which would be theatre
	// (any shell metacharacter it needs is one their build legitimately
	// uses). The answer is everything around it, and each piece is here
	// because this line is:
	//
	//   - it runs on a WORKBENCH node, never on the node that took the
	//     request, and never on the bff holding the cluster's front door;
	//   - in a directory that exists for this one call and is removed when
	//     it returns, whatever the outcome;
	//   - under an environment this cluster CONSTRUCTS (buildEnv), so the
	//     node's own credentials are not in it;
	//   - as a non-root uid (applyBuildUser), so it cannot read them out of
	//     /proc/1/environ either;
	//   - with a timeout, and killed by process group so its children go too.
	//
	// Removing any one of those would make this line the vulnerability the
	// analyser thinks it already is.
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", req.Command) //nolint:gosec // the manifest's build command is the feature; see the block above
	cmd.Dir = workdir
	cmd.Env = buildEnv(dir, req)
	cmd.Stdout = sink
	cmd.Stderr = sink
	// The command's children (npm spawns node spawns vite) die with it: the
	// build runs in its own process group and a timeout kills the group.
	isolateProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	// The uid drop, and it is the second half of the environment claim above:
	// buildEnv decides what the command is GIVEN, and this decides what it can
	// go and read. See build_user_unix.go.
	uid, noDropReason := applyBuildUser(cmd, dir)

	if i.logger != nil {
		i.logger.LogAttrs(ctx, slog.LevelInfo, "workbench build: starting",
			buildSubjectAttrs(req,
				slog.String("command", req.Command),
				slog.String("path", req.Path),
				slog.Int("uid", uid),
				slog.String("noDropReason", noDropReason))...)
	}
	runErr := cmd.Run()
	lines.flush()

	res := BuildResult{LogTail: tail.String()}
	if cctx.Err() == context.DeadlineExceeded {
		res.ErrorCode = BuildCodeTimeout
		res.ErrorMessage = fmt.Sprintf("the build for %q did not finish within %s and was stopped", req.DeployableName, req.timeout())
		return finish(res)
	}
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			res.ExitCode = exit.ExitCode()
			res.ErrorCode = BuildCodeFailed
			res.ErrorMessage = fmt.Sprintf("the build command for %q exited with status %d", req.DeployableName, res.ExitCode)
			return finish(res)
		}
		res.ErrorCode = BuildCodeFailed
		res.ErrorMessage = "the build command could not be started: " + runErr.Error()
		return finish(res)
	}

	// The output directory, resolved through the SAME root -- so a build that
	// tried to point its output at somewhere else, by writing a symlink named
	// `dist`, is refused by the kernel rather than by a comparison.
	outRel, err := treeRelative(workRel, req.Output)
	if err != nil {
		res.ErrorCode = BuildCodeInvalid
		res.ErrorMessage = "workbench build: " + err.Error()
		return finish(res)
	}
	if info, serr := srcRoot.Stat(outRel); serr != nil || !info.IsDir() {
		res.ErrorCode = BuildCodeOutputMissing
		res.ErrorMessage = fmt.Sprintf("the build for %q finished but left no %q directory under %q", req.DeployableName, req.Output, req.Path)
		return finish(res)
	}
	outDir, jerr := containedJoin(srcDir, outRel)
	if jerr != nil {
		res.ErrorCode = BuildCodeInvalid
		res.ErrorMessage = "workbench build: " + jerr.Error()
		return finish(res)
	}
	packed, count, total, err := packTarGz(outDir, buildOutputTop, limits.MaxFileCount, limits.MaxFileBytes, limits.MaxOutputBytes)
	if err != nil {
		res.ErrorCode = BuildCodeOutputTooLarge
		res.ErrorMessage = "the built output could not be packed: " + err.Error()
		return finish(res)
	}
	res.FileCount = count
	res.OutputBytes = total
	if int64(len(packed)) <= limits.MaxInlineBytes {
		res.Output.Inline = packed
	} else {
		ref, uerr := i.buildBlobPut(ctx, buildOutputObject(req), packed)
		if uerr != nil {
			res.ErrorCode = BuildCodeOutputTooLarge
			res.ErrorMessage = "the built output is too large to send inline and could not be stored: " + uerr.Error()
			return finish(res)
		}
		res.Output.Ref = ref
	}
	res.OK = true
	if i.logger != nil {
		i.logger.LogAttrs(ctx, slog.LevelInfo, "workbench build: finished",
			buildSubjectAttrs(req, slog.Int("files", count), slog.Int64("bytes", total))...)
	}
	return finish(res)
}

// buildOutputObject is where an oversized output tree is stored between the
// workbench node and the pipeline: under the same packages/ prefix the
// snapshots live under, keyed on the deployment so a retry never reads
// another run's bytes.
func buildOutputObject(req BuildRequest) string {
	return "packages/builds/" + shortId(req.DeploymentId) + "/" + safeName(req.DeployableName) + "/output.tar.gz"
}

// treeRelative composes one root-RELATIVE name from a prefix and a manifest
// path, for handing to os.Root.
//
// It returns a NAME rather than a path, and that is the whole point: a name
// is something os.Root resolves under a directory it holds open, so the
// containment is the kernel's. The previous version of this function joined a
// path and then checked it, which is correct and is a check a reader has to
// find and trust -- and which said nothing about symlinks.
func treeRelative(prefix, p string) (string, error) {
	clean, err := cleanTreePath(p)
	if err != nil {
		return "", fmt.Errorf("the path %q escapes the tree", p)
	}
	joined := path.Join(prefix, clean)
	if joined == "" {
		return ".", nil
	}
	// path.Join has already cleaned; a `..` surviving it means the prefix was
	// shallower than the path climbs, which is an escape however it is spelled.
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", fmt.Errorf("the path %q escapes the tree", p)
	}
	return joined, nil
}

// buildEnv is the whole environment a build sees. Constructed, never
// inherited: the node's own environment holds every credential the node has.
func buildEnv(home string, req BuildRequest) []string {
	pathVar := strings.TrimSpace(os.Getenv("PATH"))
	if pathVar == "" {
		pathVar = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return []string{
		"PATH=" + pathVar,
		"HOME=" + home,
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
		"CI=true",
		"npm_config_cache=" + filepath.Join(home, ".npm"),
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
		"npm_config_progress=false",
		"YARN_CACHE_FOLDER=" + filepath.Join(home, ".yarn"),
		"MEMQL_BUILD_DEPLOYMENT_ID=" + req.DeploymentId,
		"MEMQL_BUILD_DEPLOYABLE=" + req.DeployableName,
	}
}

// buildSubjectAttrs are the attributes every log line of a build carries: the
// deployment as the line's SUBJECT (the Logs epic's join, memql#4893) and the
// deployable by name. Spelled as the two attribute names the log store reads,
// so the switch to logger.Subject when it lands is a rename and nothing moves.
func buildSubjectAttrs(req BuildRequest, extra ...slog.Attr) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("component", "packages.build"),
		slog.String("subject", shortId(req.DeploymentId)),
		slog.String("subjectConcept", buildSubjectConcept),
		slog.String("deployment", req.DeploymentId),
		slog.String("deployable", req.DeployableName),
	}
	return append(attrs, extra...)
}

// ---------------------------------------------------------------------------
// Bytes in and out
// ---------------------------------------------------------------------------

const blobRefPrefix = "blob://"

func (i *Integration) buildBytes(ctx context.Context, b BuildBytes) ([]byte, error) {
	if len(b.Inline) > 0 {
		return b.Inline, nil
	}
	object, ok := strings.CutPrefix(strings.TrimSpace(b.Ref), blobRefPrefix)
	if !ok || object == "" {
		return nil, fmt.Errorf("%q is not a blob reference this workbench reads", b.Ref)
	}
	store, container, err := i.buildBlobs(ctx)
	if err != nil {
		return nil, err
	}
	return store.Download(ctx, container, object)
}

func (i *Integration) buildBlobPut(ctx context.Context, object string, data []byte) (string, error) {
	store, container, err := i.buildBlobs(ctx)
	if err != nil {
		return "", err
	}
	if _, err := store.Upload(ctx, container, object, data, "application/gzip"); err != nil {
		return "", err
	}
	return blobRefPrefix + object, nil
}

// buildBlobs resolves the object store lazily, once, so a node that never
// builds anything oversized never builds a client.
func (i *Integration) buildBlobs(ctx context.Context) (buildBlobStore, string, error) {
	i.buildBlobOnce.Do(func() {
		if i.buildBlob != nil {
			return
		}
		container := azureblob.ContainerFromEnv()
		if container == "" {
			i.buildBlobErr = errors.New("this cluster has no object storage configured (MEMQL_AZURE_BLOB_CONTAINER), so a tarball past the inline cap has nowhere to go")
			return
		}
		up, err := azureblob.New(ctx)
		if err != nil {
			i.buildBlobErr = err
			return
		}
		i.buildBlob, i.buildContainer = up, container
	})
	if i.buildBlobErr != nil {
		return nil, "", i.buildBlobErr
	}
	if i.buildBlob == nil {
		return nil, "", errors.New("no object store is configured for build transports")
	}
	return i.buildBlob, i.buildContainer, nil
}

// ---------------------------------------------------------------------------
// The bounded tail and the line logger
// ---------------------------------------------------------------------------

// tailBuffer keeps the LAST cap bytes written to it. A build's whole output
// goes to the log store line by line; the row keeps only this.
type tailBuffer struct {
	mu  sync.Mutex
	cap int
	buf []byte
}

func newTailBuffer(cap int) *tailBuffer {
	if cap <= 0 {
		cap = DefaultBuildLogTailBytes
	}
	return &tailBuffer{cap: cap}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.cap {
		t.buf = append(t.buf[:0], p[len(p)-t.cap:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		// Drop from the front in one move rather than per write: the common
		// case is a stream of small writes, and re-slicing on each keeps the
		// backing array from growing without bound.
		excess := len(t.buf) - t.cap
		copy(t.buf, t.buf[excess:])
		t.buf = t.buf[:t.cap]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// buildLineLogger turns the command's output into one slog line per line of
// output, each carrying the deployment as its subject, capped so a build that
// prints a million lines costs the store two thousand and one count.
type buildLineLogger struct {
	ctx     context.Context
	logger  *slog.Logger
	attrs   []slog.Attr
	mu      sync.Mutex
	partial []byte
	logged  int
	dropped int
}

func (i *Integration) newBuildLineLogger(ctx context.Context, req BuildRequest) *buildLineLogger {
	return &buildLineLogger{ctx: ctx, logger: i.logger, attrs: buildSubjectAttrs(req)}
}

func (l *buildLineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		nl := bytes.IndexByte(l.partial, '\n')
		if nl < 0 {
			break
		}
		l.emit(string(l.partial[:nl]))
		l.partial = l.partial[nl+1:]
	}
	return len(p), nil
}

func (l *buildLineLogger) emit(line string) {
	if l.logger == nil {
		return
	}
	if l.logged >= buildLogLineCap {
		l.dropped++
		return
	}
	l.logged++
	// THE BUILD'S OWN OUTPUT, verbatim. Static analysis reads this as
	// clear-text logging, and the honest answer is that a build log IS its
	// output -- truncating or filtering it would defeat the one thing it is
	// for, which is answering "why did my build fail".
	//
	// What bounds the exposure is upstream: the build is handed no cluster
	// credential to print (buildEnv), so anything secret in these lines is
	// something the package's OWN build script chose to echo, on a log the
	// package's owner is the one who reads. That is their decision about
	// their code, and the same one they make on any CI.
	l.logger.LogAttrs(l.ctx, slog.LevelInfo, strings.TrimRight(line, "\r"), l.attrs...)
}

func (l *buildLineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.emit(string(l.partial))
		l.partial = nil
	}
	if l.dropped > 0 && l.logger != nil {
		l.logger.LogAttrs(l.ctx, slog.LevelWarn,
			fmt.Sprintf("workbench build: %d further lines of output were not logged (cap %d); the row's tail has the end", l.dropped, buildLogLineCap),
			l.attrs...)
	}
}
