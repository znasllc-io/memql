package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const RepositoryChangeConfigurationVariable = "MEMQL_PIPELINES_REPOSITORY_CHANGE_RULES"

type repositoryChangeGitHub interface {
	RepositoryWriterToken(context.Context, string, string, string) (string, int64, error)
	ApplyBranchChange(context.Context, string, string, githubapp.BranchChange) (string, error)
	EnsureChangePullRequest(context.Context, string, string, string, string, string, string) (string, error)
}

func repositoryRule(ctx context.Context, d Deps, name string) (pipelines.RepositoryChangeRule, error) {
	if d.SystemVariable == nil {
		return pipelines.RepositoryChangeRule{}, errors.New("repository maintenance has no operator configuration")
	}
	raw, err := d.SystemVariable(memql.ContextWithFreshRead(ctx), RepositoryChangeConfigurationVariable)
	if err != nil {
		return pipelines.RepositoryChangeRule{}, errors.New("repository maintenance configuration could not be read")
	}
	rules, err := pipelines.ReadRepositoryChangeRules([]byte(raw))
	if err != nil {
		return pipelines.RepositoryChangeRule{}, err
	}
	for _, rule := range rules {
		if rule.Name == name {
			return rule, nil
		}
	}
	return pipelines.RepositoryChangeRule{}, errors.New("repository maintenance rule is not configured")
}

// An owner retry retains the original effect identity and timestamp. Its new
// producer must independently return the same proposal bytes; a changed
// proposal gets a different identity and cannot adopt an earlier write.
func repositoryChangeOriginal(ctx context.Context, d Deps, r Run) (Run, error) {
	original := r
	seen := map[string]bool{bareID(r.ID): true}
	for original.RerunOf != "" {
		id := bareID(original.RerunOf)
		if seen[id] || len(seen) >= 32 {
			return Run{}, errors.New("repository maintenance retry lineage is cyclic or too long")
		}
		seen[id] = true
		prior, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), id)
		if err != nil {
			return Run{}, err
		}
		if prior == nil || !sameID(prior.OwnerUserID, r.OwnerUserID) || !sameID(prior.PipelineID, r.PipelineID) || prior.Repository != r.Repository || prior.SHA != r.SHA || prior.Event != r.Event || prior.HeadBranch != r.HeadBranch || prior.InboundRequestID != r.InboundRequestID {
			return Run{}, errors.New("repository maintenance retry changed its original source")
		}
		original = *prior
	}
	if original.QueuedAt.IsZero() {
		return Run{}, errors.New("repository maintenance has no durable source timestamp")
	}
	return original, nil
}

// readProposalArtifact binds the successful producer's journal, immutable
// receipt, object version and digest. A Library file ID or command verdict
// alone cannot authorize a repository write.
func readProposalArtifact(ctx context.Context, d Deps, owner string, r Run, change pipelines.RepositoryChange) (pipelinesteps.RunFileReceiptScope, pipelinesteps.StoredFileReceipt, error) {
	var empty pipelinesteps.StoredFileReceipt
	proof, err := readProducerEvidence(ctx, d.SARIF.Engine, owner, r, change.Step)
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: r.WorkRunID, StepKey: change.Step, Attempt: proof.Attempt}
	if err != nil {
		return scope, empty, err
	}
	rows, err := d.SARIF.Library.ReadRunFileReceipts(auth.ContextWithInternalOrigin(ctx), scope, proof.ArtifactIntentIDs)
	if err != nil {
		return scope, empty, err
	}
	if len(rows) != len(proof.ArtifactIntentIDs) {
		return scope, empty, errors.New("proposal producer has missing artifact receipts")
	}
	seen := map[string]bool{}
	var artifact pipelinesteps.StoredFileReceipt
	for _, row := range rows {
		if row.OwnerUserID != owner || row.WorkRunID != scope.WorkRunID || row.StepKey != scope.StepKey || row.Attempt != scope.Attempt || !slices.Contains(proof.ArtifactIntentIDs, row.IntentID) || seen[row.IntentID] {
			return scope, empty, errors.New("proposal artifact receipt is outside its producer scope")
		}
		seen[row.IntentID] = true
		if row.Path == change.Path {
			if artifact.IntentID != "" {
				return scope, empty, errors.New("proposal artifact path is ambiguous")
			}
			artifact = row
		}
	}
	digest, e := hex.DecodeString(artifact.SHA256)
	if artifact.IntentID == "" || artifact.ETag == "" || artifact.Size <= 0 || artifact.Size > pipelines.RepositoryProposalMaxBytes || e != nil || len(digest) != 32 {
		return scope, empty, errors.New("proposal has no bounded immutable byte receipt")
	}
	return scope, artifact, nil
}

// repositoryChange applies one exact compiled effect. The driver journals its
// intent before calling; deterministic Git objects and ref read-back reconcile
// an interrupted effect on any replica. The scoped bearer never reaches work.
func (i *Integration) repositoryChange(ctx context.Context, runID string, change pipelines.RepositoryChange, holds func() error) (result map[string]any, retErr error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return nil, err
	}
	owner = memql.BareShortId(owner)
	d := i.snapshot()
	remote, ok := d.GitHub.(repositoryChangeGitHub)
	if d.Store == nil || !ok || d.SARIF == nil || d.SARIF.Library == nil || d.SARIF.Engine == nil {
		return nil, errors.New("native repository maintenance is unavailable")
	}
	r, p, err := sarifSource(ctx, d, owner, runID)
	if err != nil {
		return nil, err
	}
	original, err := repositoryChangeOriginal(ctx, d, r)
	if err != nil {
		return nil, err
	}
	rule, err := repositoryRule(ctx, d, change.Rule)
	if err != nil {
		return nil, err
	}
	if rule.Repository != r.Repository {
		return nil, errors.New("repository maintenance rule does not grant this source")
	}
	scope, artifact, err := readProposalArtifact(ctx, d, owner, r, change)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(struct {
		Run, Producer, Path, Digest string
		Rule                        pipelines.RepositoryChangeRule
	}{original.ID, change.Step, change.Path, artifact.SHA256, rule})
	hash := sha256.Sum256(identity)
	operation := hex.EncodeToString(hash[:])
	reference := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "repository-change:" + operation, IntentIDs: []string{artifact.IntentID}}
	raw, err := readRetainedArtifactBytes(ctx, d.SARIF.Library, scope, artifact)
	if err != nil {
		return nil, err
	}
	proposal, err := pipelines.ReadRepositoryProposal(raw, rule, r.SHA)
	if err != nil {
		return nil, err
	}
	// Nothing to write is an explicit receipt, not an actor-authorized mutation.
	if len(proposal.Files) == 0 {
		return map[string]any{"status": string(pipelines.OutcomeSucceeded), "repositoryChangeState": "unchanged", "operationId": operation, "sha256": artifact.SHA256}, nil
	}
	grant := func(prepared string) (string, string, error) {
		if err := holds(); err != nil {
			return "", "", err
		}
		currentRule, err := repositoryRule(ctx, d, change.Rule)
		if err != nil {
			return "", "", err
		}
		before, _ := json.Marshal(rule)
		now, _ := json.Marshal(currentRule)
		if string(before) != string(now) {
			return "", "", errors.New("repository maintenance grant changed during execution")
		}
		current, currentPipeline, err := sarifSource(ctx, d, owner, r.ID)
		if err != nil {
			return "", "", err
		}
		if current.SHA != r.SHA || current.WorkRunID != r.WorkRunID || current.InboundRequestID != r.InboundRequestID || current.Event != r.Event || current.HeadBranch != r.HeadBranch || currentPipeline.InstallationID != p.InstallationID || currentPipeline.CredentialID != p.CredentialID {
			return "", "", errors.New("repository maintenance source changed")
		}
		readToken, err := sarifGrant(ctx, d, owner, r, p)
		if err != nil {
			return "", "", err
		}
		branch, err := repositoryChangeOrigin(ctx, d, readToken, current, currentPipeline, rule, prepared)
		if err != nil {
			return "", "", err
		}
		token, installation, err := remote.RepositoryWriterToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
		if err != nil {
			return "", "", err
		}
		if token == "" || installation != p.InstallationID {
			return "", "", errors.New("repository maintenance installation changed")
		}
		return token, branch, nil
	}
	// Before preparing immutable Git objects, verify the original signed actor
	// and current PR author. A matching operation marker permits preparation
	// after a lost reply, but can NEVER authorize a branch write: the callback
	// below additionally requires the exact independently prepared commit SHA.
	readToken, err := sarifGrant(ctx, d, owner, r, p)
	if err != nil {
		return nil, err
	}
	recoveryHead := ""
	if rule.Mode == "update" {
		head, message, err := d.GitHub.BranchHead(ctx, readToken, r.Repository, r.HeadBranch)
		if err != nil {
			return nil, err
		}
		if head != r.SHA && message == proposal.Title+"\n\nMemQL-Operation: "+operation {
			recoveryHead = head
		}
	}
	token, _, err := grant(recoveryHead)
	if err != nil {
		return nil, err
	}
	branch := r.HeadBranch
	if rule.Mode == "propose" {
		branch = rule.BranchPrefix + proposal.Key
	}
	timestamp := original.QueuedAt
	edit := githubapp.BranchChange{Branch: branch, ExpectedHead: r.SHA, OperationID: operation, Title: proposal.Title, Timestamp: timestamp, Create: rule.Mode == "propose", Authorize: func(_ context.Context, prepared string) (string, error) {
		token, _, err := grant(prepared)
		return token, err
	}}
	for _, f := range proposal.Files {
		edit.Files = append(edit.Files, githubapp.BranchReplacement{Path: f.Path, BeforeSHA256: f.BaseSHA256, Content: f.Content})
	}
	if err := holds(); err != nil {
		return nil, err
	}
	// Keep this evidence pinned until the successful effect has a durable work
	// receipt. Releasing here would fence a replacement driver out if the
	// process stopped after GitHub accepted the write but before Finish.
	pinned, err := d.SARIF.Library.PinRunFileReceipts(auth.ContextWithInternalOrigin(ctx), reference)
	if err != nil {
		return nil, err
	}
	if len(pinned) != 1 || pinned[0] != artifact {
		return nil, errors.New("repository proposal changed while pinning")
	}
	commit, err := remote.ApplyBranchChange(ctx, token, p.Repository, edit)
	if err != nil {
		return nil, err
	}
	var pr string
	if rule.Mode == "propose" {
		token, base, err := grant(commit)
		if err != nil {
			return nil, err
		}
		pr, err = remote.EnsureChangePullRequest(ctx, token, p.Repository, branch, base, proposal.Title, proposal.Body)
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"status": string(pipelines.OutcomeSucceeded), "repositoryChangeState": "applied", "operationId": operation, "sha256": artifact.SHA256, "commit": commit, "pullRequestUrl": pr,
		"proposalStep": change.Step, "proposalPath": change.Path, "proposalAttempt": scope.Attempt, "proposalIntentId": artifact.IntentID}, nil
}

func (dr *runDriver) runRepositoryChange(ctx context.Context, t *stepTrack) {
	if !dr.stillHolds(ctx) {
		return
	}
	var err error
	t.handle, err = dr.work().Step(ctx, t.step.Key)
	if err != nil {
		dr.journalUnavailable(t.step.Key, err)
		return
	}
	s := t.snapshot()
	s.Status = StepRunning
	t.set(s)
	began := time.Now()
	finish := func(r receipt) { r.durationMs = max(time.Since(began).Milliseconds(), 1); dr.settle(ctx, t, r) }
	if t.step.RepositoryChange == nil {
		finish(failReceipt(pipelines.CodeStepInvalid, "Repository change has no declared proposal."))
		return
	}
	deadline := min(time.Duration(t.step.TimeoutSeconds)*time.Second, 10*time.Minute)
	if deadline <= 0 {
		deadline = 5 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(auth.ContextWithUserActor(ctx, dr.p.OwnerUserID), deadline)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-dr.lease.stop:
			cancel()
		case <-done:
		}
	}()
	holds := func() error {
		if callCtx.Err() != nil {
			return callCtx.Err()
		}
		if !dr.stillHolds(callCtx) || dr.lease.isCancelled() {
			return errors.New("repository maintenance driver no longer holds its run")
		}
		return dr.qualificationFence(callCtx)
	}
	result, err := dr.i.repositoryChange(callCtx, dr.runID, *t.step.RepositoryChange, holds)
	if dr.lease.isLost() {
		return
	}
	if dr.lease.isCancelled() {
		finish(cancelReceipt())
		return
	}
	if err != nil {
		finish(failReceipt(pipelines.CodeExecutorError, "Repository change could not be confirmed: "+dr.mask(err.Error())))
		return
	}
	finish(receipt{status: WorkStepDone, report: StepSucceeded, result: result})
}
