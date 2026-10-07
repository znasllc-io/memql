package installation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// sourceCaptureSpec is persisted by the preparation journal BEFORE dispatch.
// It is assembled from native installation configuration, never client/DSL
// parameters. A replacement replica must reconstruct this exact scope; it may
// not choose a new attempt, start time, image, source or artifact owner.
// Credentials are resolved afresh and are never part of the persisted scope.
type sourceCaptureSpec struct {
	OwnerUserID, RunID, WorkRunID, StepKey string
	Attempt                                int
	RunStartedAt                           string
	Repository                             pl.Repository
	CloneInstallationID                    int64
	CollectorImage, Platform               string
	ImagePullSecret                        string
	Render                                 argocd.RenderSpec
}

type sourceCapture struct {
	request pl.StepRequest
	render  argocd.RenderSpec
	digest  string
	path    string
}

// sourceCaptureReceipt is observation data, not permission. The preparation
// journal must bind it to ScopeDigest and commit it before acknowledging the
// Job. Never reconstruct it from an editable Library row or a client argument.
type sourceCaptureReceipt struct {
	ScopeDigest string `json:"scopeDigest"`
	IntentID    string `json:"intentId"`
}

// The result owns its proof and receipt; neither serialized scope digests nor
// the collector's stdout can manufacture a ClosedSource.
type verifiedSourceCapture struct {
	source  argocd.ClosedSource
	receipt pipelinesteps.StoredFileReceipt
	scope   string
}

func (v verifiedSourceCapture) String() string   { return v.source.String() }
func (v verifiedSourceCapture) GoString() string { return v.String() }

type sourceCaptureExecutor interface {
	Execute(context.Context, pl.StepRequest) (pl.StepResult, error)
	AcknowledgeReceipt(context.Context, pl.StepRequest) error
	Cancel(context.Context, string) error
}

type sourceCaptureFiles interface {
	pipelinesteps.LibraryReceiptOpener
	pipelinesteps.LibraryArtifactLifecycle
}

var captureStepKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func newSourceCapture(spec sourceCaptureSpec) (sourceCapture, error) {
	var empty sourceCapture
	for _, value := range []string{spec.OwnerUserID, spec.RunID, spec.WorkRunID} {
		if !identifier.MatchString(value) {
			return empty, errors.New("source capture requires persisted owner and run identities")
		}
	}
	started, err := time.Parse(time.RFC3339Nano, spec.RunStartedAt)
	if err != nil || started.IsZero() || spec.Attempt < 1 || !captureStepKey.MatchString(spec.StepKey) || spec.CloneInstallationID < 0 {
		return empty, errors.New("source capture requires a persisted attempt and start time")
	}
	if spec.Platform != "linux/amd64" && spec.Platform != "linux/arm64" {
		return empty, errors.New("source capture requires an explicit Linux collector platform")
	}
	if _, err := immutableImage(spec.CollectorImage); err != nil {
		return empty, errors.New("source capture requires an immutable collector image")
	}
	if spec.ImagePullSecret != "" && !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`).MatchString(spec.ImagePullSecret) {
		return empty, errors.New("source capture image credential name is invalid")
	}
	if strings.HasPrefix(spec.ImagePullSecret, "MEMQL_") || spec.ImagePullSecret == "GIT_TOKEN" {
		return empty, errors.New("source capture image credential name is reserved")
	}
	body, err := json.Marshal(spec.Render)
	// The fixed command carries only encoded, credential-free render config.
	// Bound it below the container's single environment-variable size limit.
	if err != nil || len(body) > 32<<10 {
		return empty, errors.New("source capture render specification exceeds its transport bound")
	}
	render, err := argocd.DecodeRenderSpec(body)
	if err != nil {
		return empty, err
	}
	// Reject duplicate/unknown fields before normalization can erase them.
	body, err = canonicalJSON(body)
	if err != nil {
		return empty, err
	}
	var source struct{ RepoURL, TargetRevision string }
	if json.Unmarshal(render.Source, &source) != nil || source.RepoURL != spec.Repository.CloneURL {
		return empty, errors.New("source capture repository does not match the native render specification")
	}
	// This runner mints GitHub App clone tokens. Other authenticated Git hosts
	// need a qualified acquisition codec; never send that token to another host.
	u, err := url.Parse(spec.Repository.CloneURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "/"+spec.Repository.Owner+"/"+spec.Repository.Name+".git" ||
		!identifier.MatchString(spec.Repository.Owner) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(spec.Repository.Name) {
		return empty, errors.New("source capture requires a canonical GitHub repository URL")
	}
	const directoryToken = "__MEMQL_SOURCE_DIRECTORY__"
	artifact := directoryToken + "/source.tar"
	// mkdir refuses a preexisting repository path, including a symlink. No
	// repository script executes; only the image's absolute collector runs.
	command := "main() {\nset -eu\numask 077\nmkdir '" + directoryToken + "'\nprintf '%s' '" + base64.StdEncoding.EncodeToString(body) + "' | base64 -d > '" + directoryToken + "/render.json'\n/app/installation-source --repository=/workspace --spec='" + directoryToken + "/render.json' --output='" + artifact + "'\n}\nmain"
	request := pl.StepRequest{
		RunID: spec.RunID, WorkRunID: spec.WorkRunID, StepKey: spec.StepKey, Attempt: spec.Attempt,
		RunAttempt: 1, RunStartedAt: spec.RunStartedAt, OwnerUserID: spec.OwnerUserID,
		Repository: spec.Repository, SHA: source.TargetRevision, InstallationID: spec.CloneInstallationID, Compute: pl.ComputeCluster,
		Step: pl.Step{Key: spec.StepKey, Kind: pl.StepCommand, Run: command, Image: spec.CollectorImage,
			ImagePullSecret: spec.ImagePullSecret, Placement: pl.PlacementCluster, Execution: pl.ExecutionContainer,
			Platform: spec.Platform, TimeoutSeconds: 300, CPUMilli: 1000, MemoryMiB: 512, Artifacts: []string{artifact}},
	}
	// Bind the entire native request, including the command and limits, before
	// substituting its content-addressed output directory. JSONB field ordering
	// must not change this identity when another replica reloads the scope.
	canonical, err := json.Marshal(request)
	if err != nil {
		return empty, err
	}
	canonical, err = canonicalJSON(canonical)
	if err != nil {
		return empty, err
	}
	key := string(id.NewUntracked().FromString("installation-source-capture-v1:" + string(canonical)))
	directory := ".memql-source-" + key
	request.Step.Run = strings.ReplaceAll(command, directoryToken, directory)
	request.Step.Artifacts = []string{directory + "/source.tar"}
	return sourceCapture{digest: "memql-id:" + key, render: render, path: request.Step.Artifacts[0], request: request}, nil
}

func (c sourceCapture) stepRequest() pl.StepRequest {
	request := c.request
	request.Step.Artifacts = append([]string(nil), request.Step.Artifacts...)
	return request
}

func (c sourceCapture) actor(ctx context.Context) (context.Context, error) {
	actor, err := installationActor(ctx)
	if err != nil || auth.OriginFromContext(ctx) != auth.OriginInternal || actor != c.request.OwnerUserID || !internalDigest.MatchString(c.digest) {
		return nil, errors.New("source capture requires an admitted native call under its identified installation operator")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ctx, nil
}

// run performs one fixed capture operation. Lost replies use the journal's
// recoverOnly path, which cannot create a replacement effect. The executor
// owns cancellation, cross-replica adoption and durable Job/cleanup receipts.
func (c sourceCapture) run(ctx context.Context, executor sourceCaptureExecutor, recoverOnly bool, imagePullValue string) (sourceCaptureReceipt, error) {
	ctx, err := c.actor(ctx)
	if err != nil {
		return sourceCaptureReceipt{}, err
	}
	if executor == nil || (c.request.Step.ImagePullSecret == "") != (imagePullValue == "") {
		return sourceCaptureReceipt{}, errors.New("source capture executor or image credential is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request := c.stepRequest()
	request.RecoverOnly = recoverOnly
	if imagePullValue != "" {
		request.Secrets = map[string]string{request.Step.ImagePullSecret: imagePullValue}
	}
	outcome, err := executor.Execute(ctx, request)
	if err != nil {
		return sourceCaptureReceipt{}, errors.New("source capture execution has no confirmed outcome")
	}
	if outcome.Status != pl.OutcomeSucceeded || outcome.ExitCode != 0 || outcome.Failure != nil || outcome.Where.Surface != "cluster" ||
		outcome.Where.JobName != pipelinesteps.JobName(request.RunID, request.StepKey, request.Attempt) ||
		len(outcome.ArtifactIntentIDs) != 1 || !pl.ValidArtifactIntentIDs(outcome.ArtifactIntentIDs) {
		return sourceCaptureReceipt{}, errors.New("source capture did not produce one confirmed private artifact")
	}
	return sourceCaptureReceipt{ScopeDigest: c.digest, IntentID: outcome.ArtifactIntentIDs[0]}, nil
}

func (c sourceCapture) reference(receipt sourceCaptureReceipt) (pipelinesteps.RunFileReference, error) {
	if !internalDigest.MatchString(c.digest) || receipt.ScopeDigest != c.digest || !pl.ValidArtifactIntentIDs([]string{receipt.IntentID}) {
		return pipelinesteps.RunFileReference{}, errors.New("source capture receipt belongs to another scope")
	}
	return pipelinesteps.RunFileReference{Scope: pipelinesteps.RunFileReceiptScope{
		OwnerUserID: c.request.OwnerUserID, WorkRunID: c.request.WorkRunID, StepKey: c.request.StepKey, Attempt: c.request.Attempt,
	}, ReferenceID: "installation-source:" + c.digest, IntentIDs: []string{receipt.IntentID}}, nil
}

// verify can run on any replica, including after the producer Job is gone.
// Pin before opening; a failed verification leaves that durable reference for
// the journal's explicit retirement path, never speculative automatic cleanup.
func (c sourceCapture) verify(ctx context.Context, files sourceCaptureFiles, receipt sourceCaptureReceipt) (verifiedSourceCapture, error) {
	ctx, err := c.actor(ctx)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	ref, err := c.reference(receipt)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if files == nil {
		return verifiedSourceCapture{}, errors.New("source capture artifact store is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pinned, err := files.PinRunFileReceipts(ctx, ref)
	if err != nil || len(pinned) != 1 {
		return verifiedSourceCapture{}, errors.New("source capture artifact could not be retained")
	}
	stored, body, err := files.OpenRunFileReceipt(ctx, ref.Scope, receipt.IntentID)
	if err != nil {
		return verifiedSourceCapture{}, errors.New("source capture artifact could not be opened")
	}
	if body == nil {
		return verifiedSourceCapture{}, errors.New("source capture artifact has no body")
	}
	defer body.Close()
	if stored != pinned[0] || stored.IntentID != receipt.IntentID || stored.OwnerUserID != ref.Scope.OwnerUserID ||
		stored.WorkRunID != ref.Scope.WorkRunID || stored.StepKey != ref.Scope.StepKey || stored.Attempt != ref.Scope.Attempt ||
		stored.Path != c.path || stored.Size < 1024 || stored.Size > argocd.MaxSourceArchiveBytes || stored.ETag == "" ||
		stored.FileID == "" || stored.Container == "" || stored.Object == "" || stored.URL == "" {
		return verifiedSourceCapture{}, errors.New("source capture artifact does not match its pinned private receipt")
	}
	data, err := io.ReadAll(io.LimitReader(body, argocd.MaxSourceArchiveBytes+1))
	if err != nil || int64(len(data)) != stored.Size {
		return verifiedSourceCapture{}, errors.New("source capture artifact did not complete within its bound")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != stored.SHA256 {
		return verifiedSourceCapture{}, errors.New("source capture artifact digest changed")
	}
	if err := ctx.Err(); err != nil {
		return verifiedSourceCapture{}, err
	}
	closed, err := argocd.VerifySourceArchive(ctx, data, c.render)
	if err != nil {
		return verifiedSourceCapture{}, fmt.Errorf("source capture verification refused: %w", err)
	}
	return verifiedSourceCapture{source: closed, receipt: stored, scope: c.digest}, nil
}

// acknowledge is called only after the preparation journal rereads its durable
// receipt. It frees the Job, not the private artifact. Verification and journal
// retirement separately control the artifact pin's lifetime.
func (c sourceCapture) acknowledge(ctx context.Context, executor sourceCaptureExecutor, committed sourceCaptureReceipt) error {
	ctx, err := c.actor(ctx)
	if err != nil {
		return err
	}
	if _, err := c.reference(committed); err != nil {
		return err
	}
	if executor == nil {
		return errors.New("source capture executor is unavailable")
	}
	return executor.AcknowledgeReceipt(ctx, c.stepRequest())
}

// cancel retires the preparation's OWN capture run after its durable stop
// record. It does not release artifacts. The journal must never reuse RunID
// for a successor; the executor's retirement fence rejects delayed creates.
func (c sourceCapture) cancel(ctx context.Context, executor sourceCaptureExecutor) error {
	ctx, err := c.actor(ctx)
	if err != nil {
		return err
	}
	if executor == nil {
		return errors.New("source capture executor is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return executor.Cancel(ctx, c.request.RunID)
}
