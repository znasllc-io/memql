package pipelinerun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type SARIFLibrary interface {
	pipelinesteps.LibraryReceiptReader
	pipelinesteps.LibraryReceiptOpener
	pipelinesteps.LibraryArtifactLifecycle
}

type SARIFDependencies struct {
	Engine   Engine
	Database func() *sql.DB
	Library  SARIFLibrary
}

type sarifGitHub interface {
	UploadSARIF(context.Context, string, string, githubapp.SARIFAnalysis) (githubapp.SARIFUpload, error)
	SARIFUploadStatus(context.Context, string, string, string) (githubapp.SARIFProcessing, error)
}

func (i *Integration) handlePublishSARIF(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	r, err := i.PublishSARIF(handlerContext(ctx), stringArg(args, "runId"), stringArg(args, "stepKey"), stringArg(args, "path"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"publicationId": r.ID, "state": r.State, "uploadId": r.UploadID, "sha256": r.SHA256, "errors": r.Errors}), nil
}

// PublishSARIF performs one bounded external publication. The caller selects a
// successful scanner receipt, never a URL, repository, ref, token or raw report.
// Repeating the call reads a previously accepted upload's ingestion status; it
// never repeats a POST after an uncertain response or an interrupted uploader.
// DSL decides when to call, poll, notify or fail on the returned state.
func (i *Integration) PublishSARIF(ctx context.Context, runID, stepKey, filePath string) (out SARIFPublication, retErr error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return SARIFPublication{}, err
	}
	owner = memql.BareShortId(owner)
	if runID == "" || len(runID) > 512 || stepKey == "" || len(stepKey) > 512 || filePath == "" || len(filePath) > 1024 ||
		path.Clean(filePath) != filePath || strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, "../") || strings.ContainsAny(filePath, "\\\x00\r\n") || !strings.HasSuffix(filePath, ".sarif") {
		return SARIFPublication{}, errors.New("SARIF publication requires an exact run, step and repository-relative .sarif artifact path")
	}
	d := i.snapshot()
	remote, ok := d.GitHub.(sarifGitHub)
	if d.Store == nil || !ok || d.SARIF == nil || d.SARIF.Engine == nil || d.SARIF.Database == nil || d.SARIF.Library == nil {
		return SARIFPublication{}, errors.New("native SARIF publication is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, p, err := sarifSource(ctx, d, owner, runID)
	if err != nil {
		return SARIFPublication{}, err
	}
	proof, err := readProducerEvidence(ctx, d.SARIF.Engine, owner, r, stepKey)
	if err != nil {
		return SARIFPublication{}, err
	}
	ref, err := sarifRef(r)
	if err != nil {
		return SARIFPublication{}, err
	}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: r.WorkRunID, StepKey: stepKey, Attempt: proof.Attempt}
	rows, err := d.SARIF.Library.ReadRunFileReceipts(auth.ContextWithInternalOrigin(ctx), scope, proof.ArtifactIntentIDs)
	if err != nil {
		return SARIFPublication{}, err
	}
	if len(rows) != len(proof.ArtifactIntentIDs) {
		return SARIFPublication{}, errors.New("SARIF producer has missing artifact receipts")
	}
	var artifact pipelinesteps.StoredFileReceipt
	seen := map[string]bool{}
	for _, row := range rows {
		if row.OwnerUserID != owner || row.WorkRunID != scope.WorkRunID || row.StepKey != stepKey || row.Attempt != scope.Attempt ||
			!slices.Contains(proof.ArtifactIntentIDs, row.IntentID) || seen[row.IntentID] {
			return SARIFPublication{}, errors.New("SARIF artifact receipt is outside its producer scope")
		}
		seen[row.IntentID] = true
		if row.Path == filePath {
			if artifact.IntentID != "" {
				return SARIFPublication{}, errors.New("SARIF path has ambiguous receipts")
			}
			artifact = row
		}
	}
	digest, decodeErr := hex.DecodeString(artifact.SHA256)
	if artifact.IntentID == "" || artifact.ETag == "" || artifact.Size <= 0 || artifact.Size > githubapp.MaxSARIFBytes || decodeErr != nil || len(digest) != 32 {
		return SARIFPublication{}, errors.New("SARIF artifact has no bounded immutable byte receipt")
	}
	e := sarifEvidence{Owner: owner, PipelineRunID: r.ID, Repository: r.Repository, Commit: r.SHA, Ref: ref, ReceiptDigest: proof.ReceiptDigest, InstallationID: p.InstallationID, Artifact: artifact}
	defer func() {
		if retErr == nil {
			retErr = releaseFinishedSARIF(ctx, d.SARIF.Library, e, out)
		}
	}()
	l := sarifLedger{db: d.SARIF.Database()}
	rec, err := l.reserve(ctx, e)
	if err != nil {
		return rec, err
	}
	// Even an old receipt is read under the source owner's CURRENT grant.
	token, err := sarifGrant(ctx, d, owner, r, p)
	if err != nil {
		return rec, err
	}
	switch rec.State {
	case "complete", "failed", "rejected", "uncertain":
		return rec, nil
	case "uploading":
		rec.State = "uncertain"
		return rec, nil
	case "accepted":
		return sarifObserve(ctx, l, e, remote, token, rec)
	case "prepared":
	default:
		return rec, errors.New("unknown SARIF publication state")
	}
	refs, err := d.SARIF.Library.PinRunFileReceipts(auth.ContextWithInternalOrigin(ctx), pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "sarif:" + rec.ID, IntentIDs: []string{artifact.IntentID}})
	if err != nil {
		return rec, err
	}
	if len(refs) != 1 || refs[0] != artifact {
		return rec, errors.New("SARIF artifact changed while pinning")
	}
	body, err := readRetainedArtifactBytes(ctx, d.SARIF.Library, scope, artifact)
	if err != nil {
		return rec, err
	}
	// Object reads can be slow. Re-read source binding and mint again directly
	// before the POST claim, so a changed/revoked grant cannot reuse old evidence.
	token, err = sarifGrant(ctx, d, owner, r, p)
	if err != nil {
		return rec, err
	}
	claimed, err := l.claim(ctx, e)
	if err != nil {
		return rec, err
	}
	if !claimed {
		rec, err = l.read(ctx, e)
		if rec.State == "uploading" {
			rec.State = "uncertain"
		}
		return rec, err
	}
	upload, uploadErr := remote.UploadSARIF(ctx, token, e.Repository, githubapp.SARIFAnalysis{CommitSHA: e.Commit, Ref: e.Ref, CheckoutURI: "file:///workspace/", StartedAt: r.StartedAt, SARIF: body})
	// Preserve the intent if cancellation or a lost database write follows the
	// HTTP effect. No subsequent process may silently repeat this POST.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer writeCancel()
	if uploadErr != nil {
		state := "uncertain"
		if !errors.Is(uploadErr, githubapp.ErrSARIFUploadUncertain) {
			state = "rejected"
		}
		return l.settle(writeCtx, e, "uploading", state, "", []string{boundedSARIFError(uploadErr.Error())})
	}
	if upload.ID == "" || upload.SARIFSHA256 != artifact.SHA256 {
		return l.settle(writeCtx, e, "uploading", "uncertain", "", []string{"GitHub upload receipt did not identify the verified report"})
	}
	rec, err = l.settle(writeCtx, e, "uploading", "accepted", upload.ID, nil)
	if err != nil {
		return rec, err
	}
	return sarifObserve(ctx, l, e, remote, token, rec)
}

// ReadSARIF observes the durable receipt without requiring the source artifact
// to remain stored forever. Successful ingestion and definitive failures release
// this consumer's pin; uncertain uploads keep their evidence for investigation.
func (i *Integration) ReadSARIF(ctx context.Context, key string) (SARIFPublication, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return SARIFPublication{}, err
	}
	owner = memql.BareShortId(owner)
	d := i.snapshot()
	remote, ok := d.GitHub.(sarifGitHub)
	if d.Store == nil || !ok || d.SARIF == nil || d.SARIF.Database == nil || d.SARIF.Library == nil {
		return SARIFPublication{}, errors.New("native SARIF publication is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	l := sarifLedger{db: d.SARIF.Database()}
	e, err := l.evidence(ctx, owner, key)
	if err != nil {
		return SARIFPublication{}, err
	}
	r, p, err := sarifSource(ctx, d, owner, e.PipelineRunID)
	if err != nil {
		return SARIFPublication{}, err
	}
	ref, err := sarifRef(r)
	if err != nil || r.Repository != e.Repository || r.SHA != e.Commit || ref != e.Ref || p.InstallationID != e.InstallationID || r.WorkRunID != e.Artifact.WorkRunID {
		return SARIFPublication{}, errors.New("SARIF receipt no longer matches its source")
	}
	token, err := sarifGrant(ctx, d, owner, r, p)
	if err != nil {
		return SARIFPublication{}, err
	}
	rec, err := l.read(ctx, e)
	if err != nil {
		return rec, err
	}
	if rec.State == "accepted" {
		rec, err = sarifObserve(ctx, l, e, remote, token, rec)
		if err != nil {
			return rec, err
		}
	}
	if rec.State == "uploading" {
		rec.State = "uncertain"
	}
	return rec, releaseFinishedSARIF(ctx, d.SARIF.Library, e, rec)
}

func (i *Integration) handleReadSARIF(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	r, err := i.ReadSARIF(handlerContext(ctx), stringArg(args, "publicationId"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"publicationId": r.ID, "state": r.State, "uploadId": r.UploadID, "sha256": r.SHA256, "errors": r.Errors}), nil
}

func releaseFinishedSARIF(ctx context.Context, l SARIFLibrary, e sarifEvidence, r SARIFPublication) error {
	if r.State != "complete" && r.State != "failed" && r.State != "rejected" {
		return nil
	}
	a := e.Artifact
	return l.ReleaseRunFileReference(auth.ContextWithInternalOrigin(ctx), pipelinesteps.RunFileReference{
		Scope:       pipelinesteps.RunFileReceiptScope{OwnerUserID: e.Owner, WorkRunID: a.WorkRunID, StepKey: a.StepKey, Attempt: a.Attempt},
		ReferenceID: "sarif:" + r.ID, IntentIDs: []string{a.IntentID},
	})
}

func sarifSource(ctx context.Context, d Deps, owner, runID string) (Run, Pipeline, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	r, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return Run{}, Pipeline{}, err
	}
	if r == nil || memql.BareShortId(r.OwnerUserID) != owner || (r.Status != StatusInProgress && (r.Status != StatusCompleted || (r.Conclusion != "success" && r.Conclusion != "failure"))) || r.CancelRequested || r.WorkRunID == "" {
		return Run{}, Pipeline{}, errors.New("SARIF publication requires the caller's active or completed native pipeline without cancellation")
	}
	p, err := d.Store.PipelineForOwner(ctx, r.PipelineID)
	if err != nil {
		return Run{}, Pipeline{}, err
	}
	if p == nil || memql.BareShortId(p.OwnerUserID) != owner || p.Repository != r.Repository || p.CredentialID == "" || p.InstallationID <= 0 {
		return Run{}, Pipeline{}, errors.New("SARIF source is no longer bound to the caller's GitHub connection")
	}
	if r.Qualification != nil || r.Trigger == TriggerQualification {
		if err := ValidateQualification(*p, *r, d.now()); err != nil {
			return Run{}, Pipeline{}, err
		}
	}
	return *r, *p, nil
}

func sarifGrant(ctx context.Context, d Deps, owner string, want Run, p Pipeline) (string, error) {
	r, current, err := sarifSource(ctx, d, owner, want.ID)
	if err != nil {
		return "", err
	}
	if r.SHA != want.SHA || r.WorkRunID != want.WorkRunID || r.Event != want.Event || r.HeadBranch != want.HeadBranch || r.Version != want.Version ||
		current.CredentialID != p.CredentialID || current.InstallationID != p.InstallationID {
		return "", errors.New("SARIF source changed before publication")
	}
	if r.Qualification != nil || r.Trigger == TriggerQualification {
		dr := &runDriver{d: d, p: current, run: r}
		if err := dr.qualificationFence(ctx); err != nil {
			return "", err
		}
	}
	token, installation, err := d.GitHub.InstallationToken(ctx, current.CredentialID, current.OwnerUserID, current.Repository)
	if err != nil {
		return "", err
	}
	if token == "" || installation != p.InstallationID {
		return "", errors.New("SARIF GitHub installation changed; reconnect the source")
	}
	return token, nil
}

func sarifRef(r Run) (string, error) {
	if r.Event == pipelines.EventPullRequest && r.PullRequest > 0 {
		return fmt.Sprintf("refs/pull/%d/head", r.PullRequest), nil
	}
	if r.Event == pipelines.EventRelease && pipelines.ValidBranchName(r.Version) {
		return "refs/tags/" + r.Version, nil
	}
	branch := strings.TrimPrefix(r.HeadBranch, "refs/heads/")
	if r.Event != pipelines.EventRelease && r.Event != pipelines.EventPullRequest && pipelines.ValidBranchName(branch) {
		return "refs/heads/" + branch, nil
	}
	return "", errors.New("SARIF run has no verified full Git ref")
}

func readRetainedArtifactBytes(ctx context.Context, l SARIFLibrary, scope pipelinesteps.RunFileReceiptScope, want pipelinesteps.StoredFileReceipt) ([]byte, error) {
	got, body, err := l.OpenRunFileReceipt(auth.ContextWithInternalOrigin(ctx), scope, want.IntentID)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("SARIF artifact has no byte stream")
	}
	if got != want {
		body.Close()
		return nil, errors.New("SARIF object version changed")
	}
	b, readErr := io.ReadAll(io.LimitReader(body, want.Size+1))
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	if int64(len(b)) != want.Size || hex.EncodeToString(hash[:]) != want.SHA256 {
		return nil, errors.New("SARIF object bytes differ from their receipt")
	}
	return b, nil
}

func sarifObserve(ctx context.Context, l sarifLedger, e sarifEvidence, g sarifGitHub, token string, r SARIFPublication) (SARIFPublication, error) {
	status, err := g.SARIFUploadStatus(ctx, token, e.Repository, r.UploadID)
	if err != nil {
		return r, err
	}
	switch status.Status {
	case "pending":
		return r, nil
	case "complete":
		return l.settle(ctx, e, "accepted", "complete", r.UploadID, nil)
	case "failed":
		notes := []string{}
		for _, note := range status.Errors {
			if len(notes) < 8 {
				notes = append(notes, boundedSARIFError(note))
			}
		}
		return l.settle(ctx, e, "accepted", "failed", r.UploadID, notes)
	default:
		return r, errors.New("GitHub returned an unknown SARIF ingestion state")
	}
}

func boundedSARIFError(s string) string {
	if len(s) > 1500 {
		return s[:1500]
	}
	return s
}
