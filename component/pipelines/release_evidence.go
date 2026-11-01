package pipelines

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// ReleaseWorkReceipt is derived from server-written journal rows, never from
// a client verdict. ReceiptDigest binds the source, execution and terminal
// result; a mutable step ID alone cannot identify a particular receipt.
type ReleaseWorkReceipt struct {
	OwnerUserID, WorkRunID, StepKey, ReceiptID string
	Repository, Commit, Mode, Event            string
	Attempt                                    int
	DefinitionDigest, ReceiptDigest            string
	ArtifactIntentIDs                          []string
	StepKind, Status, SkipCode                 string
	// TreeHash is the git tree the run tested, from its work input. Empty on a
	// run opened before tree hashes existed, or whose read failed. Never encoded:
	// the native overlay hashes these receipts, and an overlay's saved evidence must
	// read the same on any engine.
	TreeHash string `json:"-"`
}

// InspectReleaseWorkReceipt is a pure integrity check over an authorized,
// fresh journal read. It does not authorize a read, choose required checks,
// or certify the artifact bytes. Those responsibilities remain with the
// native reader, installed release workflow and artifact verifier respectively.
func InspectReleaseWorkReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error) {
	out, err := InspectReleaseStepReceipt(owner, runID, stepKey, run, step)
	if err != nil {
		return ReleaseWorkReceipt{}, err
	}
	if out.StepKind != string(StepCommand) || out.Status != "done" {
		return ReleaseWorkReceipt{}, errors.New("artifact producer requires an actually successful command receipt")
	}
	return out, nil
}

// InspectReleaseStepReceipt preserves typed facts for every declared step.
// Planned skips and delivered notifications remain distinguishable from tests
// that actually executed. DSL decides which skip reasons its policy permits;
// an unplanned skip, failed delivery or ambiguous command can never pass here.
func InspectReleaseStepReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error) {
	return inspectPipelineStepReceipt(owner, runID, stepKey, run, step, admitReleased)
}

// InspectScannerReceipt admits a completed scanner while its enclosing run is
// still running or has failed elsewhere. It never relaxes release admission.
// Its identity excludes the enclosing run's changing status, so recovery after
// completion still addresses the same external publication.
func InspectScannerReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error) {
	out, err := inspectPipelineStepReceipt(owner, runID, stepKey, run, step, admitScanner)
	if err != nil {
		return out, err
	}
	if out.StepKind != string(StepCommand) || out.Status != "done" {
		return ReleaseWorkReceipt{}, errors.New("scanner evidence requires an actually successful command")
	}
	return out, nil
}

// InspectCarriedReleaseReceipt admits the receipt a failed-only release re-run
// carries: a step that actually ran and passed in an earlier attempt of the
// same release, whose work run then failed or was cancelled elsewhere. That
// terminal state is the one relaxation. The step must still be a successful
// command with exit zero, and it must name no artifact intents, because every
// artifact a candidate pins comes from the attempt the candidate names
// (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md).
// Its identity excludes the enclosing run's terminal state, so one step
// receipt has one digest however its attempt ended.
func InspectCarriedReleaseReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error) {
	out, err := inspectPipelineStepReceipt(owner, runID, stepKey, run, step, admitCarried)
	if err != nil {
		return out, err
	}
	if out.StepKind != string(StepCommand) || out.Status != "done" || len(out.ArtifactIntentIDs) != 0 {
		return ReleaseWorkReceipt{}, errors.New("a carried release receipt requires an executed, artifact-free successful command")
	}
	return out, nil
}

// receiptAdmission names which enclosing work runs an inspection admits.
type receiptAdmission int

const (
	// admitReleased: the enclosing run succeeded and was never cancelled.
	admitReleased receiptAdmission = iota
	// admitScanner: a completed scanner inside a running or failed run.
	admitScanner
	// admitCarried: a pass inside an attempt that ended in any terminal state
	// of a work run: succeeded, failed, cancelled (even mid-request) or
	// abandoned.
	admitCarried
)

func inspectPipelineStepReceipt(owner, runID, stepKey string, run, step map[string]any, admission receiptAdmission) (ReleaseWorkReceipt, error) {
	var out ReleaseWorkReceipt
	var r struct {
		ID, OwnerUserID, Status, TemplateFingerprint, TriggeredBy, Mode string
		CancelRequested, CallerSuppliedPayload                          bool
		Input                                                           struct {
			Repository, SHA, Mode, Event, PipelineID, PipelineRunID string
			Attempt                                                 int
		}
	}
	var s struct {
		ID, CreatedAt, OwnerUserID, RunID, Key, Status, StepType, Kind, ErrorCode, FinishedAt string
		Attempt                                                                               int
		Call                                                                                  struct {
			Construct, DefinitionFingerprint, PipelineKind string
			Skip                                           *Skip
		}
		Result struct {
			Status, Code, Reason, Channel, Kind, NotificationStatus string
			RequestIDs                                              []string
			PublicationID, UploadID, SHA256, IngestionStatus        string
			RepositoryChangeState, OperationID, Commit              string
			Metadata                                                struct {
				ExitCode          *int
				ArtifactIntentIDs []string
			}
		}
	}
	decode := func(in map[string]any, dst any) error {
		b, err := json.Marshal(in)
		if err != nil || len(b) > 1<<20 {
			return errors.New("work receipt exceeds encoding bound")
		}
		return json.Unmarshal(b, dst)
	}
	if err := decode(run, &r); err != nil {
		return out, err
	}
	if err := decode(step, &s); err != nil {
		return out, err
	}
	runAdmitted := r.Status == "succeeded"
	cancelAdmitted := false
	switch admission {
	case admitScanner:
		runAdmitted = r.Status == "running" || r.Status == "succeeded" || r.Status == "failed"
	case admitCarried:
		runAdmitted = r.Status == "succeeded" || r.Status == "failed" || r.Status == "cancelled" || r.Status == "abandoned"
		cancelAdmitted = true
	}
	definition := "sha256:" + strings.TrimPrefix(s.Call.DefinitionFingerprint, "work-definition-v2:")
	if !releaseIdentity(owner) || !releaseIdentity(runID) || !releaseIdentity(stepKey) ||
		r.ID != runID || r.OwnerUserID != owner || s.OwnerUserID != owner || s.RunID != runID || s.Key != stepKey ||
		!releaseIdentity(s.ID) || s.CreatedAt == "" || s.FinishedAt == "" || s.Attempt < 1 ||
		!runAdmitted || (r.CancelRequested && !cancelAdmitted) || r.CallerSuppliedPayload || r.Mode != "live" ||
		r.TriggeredBy != WorkTriggerPrefix+r.Input.Mode || r.Input.PipelineID == "" || r.Input.PipelineRunID == "" || r.Input.Attempt < 1 ||
		!strings.HasPrefix(r.TemplateFingerprint, "work-definition-v2:") || !releaseDigest("sha256:"+strings.TrimPrefix(r.TemplateFingerprint, "work-definition-v2:")) ||
		(s.Status != "done" && s.Status != "skipped") || s.StepType != "exec" || s.Kind != "deterministic" || s.Call.Construct != "pipeline" ||
		!strings.HasPrefix(s.Call.DefinitionFingerprint, "work-definition-v2:") || !releaseDigest(definition) ||
		(s.Call.PipelineKind != string(StepCommand) && s.Call.PipelineKind != string(StepNotify) && s.Call.PipelineKind != string(StepSARIF) && s.Call.PipelineKind != string(StepRepositoryChange)) ||
		!ValidArtifactIntentIDs(s.Result.Metadata.ArtifactIntentIDs) || !releaseSource.MatchString(r.Input.Repository) || !releaseCommit.MatchString(r.Input.SHA) {
		return out, errors.New("release evidence requires a successful live pipeline and exact successful execution receipt")
	}
	if s.Status == "skipped" {
		if s.Call.Skip == nil || s.Call.Skip.Code == "" || s.Call.Skip.Reason == "" || s.ErrorCode != s.Call.Skip.Code ||
			s.Result.Code != s.Call.Skip.Code || s.Result.Reason != s.Call.Skip.Reason || s.Result.Status != "" || s.Result.Metadata.ExitCode != nil || len(s.Result.Metadata.ArtifactIntentIDs) != 0 {
			return out, errors.New("release skip does not match the original declared selection")
		}
	} else {
		if s.ErrorCode != "" || s.Call.Skip != nil || s.Result.Status != string(OutcomeSucceeded) {
			return out, errors.New("release step lacks an actual successful outcome")
		}
		switch StepKind(s.Call.PipelineKind) {
		case StepRepositoryChange:
			if !releaseDigest("sha256:"+s.Result.OperationID) || !releaseDigest("sha256:"+s.Result.SHA256) ||
				(s.Result.RepositoryChangeState != "unchanged" && s.Result.RepositoryChangeState != "applied") ||
				(s.Result.RepositoryChangeState == "applied" && !releaseCommit.MatchString(s.Result.Commit)) ||
				(s.Result.RepositoryChangeState == "unchanged" && s.Result.Commit != "") ||
				s.Result.Metadata.ExitCode != nil || len(s.Result.Metadata.ArtifactIntentIDs) != 0 {
				return out, errors.New("repository change requires an exact confirmed write or unchanged proposal receipt")
			}
		case StepCommand:
			if s.Result.Metadata.ExitCode == nil || *s.Result.Metadata.ExitCode != 0 {
				return out, errors.New("release command requires explicit exit zero")
			}
		case StepSARIF:
			if s.Result.IngestionStatus != "complete" || !releaseDigest("sha256:"+s.Result.SHA256) ||
				!releaseDigest("sha256:"+s.Result.PublicationID) || !releaseIdentity(s.Result.UploadID) ||
				s.Result.Metadata.ExitCode != nil || len(s.Result.Metadata.ArtifactIntentIDs) != 0 {
				return out, errors.New("release security report requires confirmed ingestion of exact bytes")
			}
		case StepNotify:
			if s.Result.NotificationStatus != "delivered" || s.Result.Channel == "" || (s.Result.Kind != "email" && s.Result.Kind != "discord") ||
				len(s.Result.RequestIDs) == 0 || len(s.Result.RequestIDs) > 20 || len(s.Result.Metadata.ArtifactIntentIDs) != 0 || s.Result.Metadata.ExitCode != nil {
				return out, errors.New("release notification requires an actual delivery receipt")
			}
			seen := map[string]bool{}
			for _, requestID := range s.Result.RequestIDs {
				if !releaseIdentity(requestID) || seen[requestID] {
					return out, errors.New("release notification has invalid delivery identities")
				}
				seen[requestID] = true
			}
		}
	}
	// Hash only journal facts relevant to the receipt. Heartbeats and the node
	// currently serving the read may change without changing completed work.
	// Full call/result values are included, including fields unknown here.
	prefix := "memql.release-work-receipt.v1\x00"
	switch admission {
	case admitScanner:
		r.Status = "" // mutable enclosing state is gated above, not publication identity
		prefix = "memql.scanner-work-receipt.v1\x00"
	case admitCarried:
		// The attempt's terminal state is gated above. Leaving it out keeps one
		// digest per step receipt, and the prefix keeps that digest apart from
		// any release receipt's.
		r.Status, r.CancelRequested = "", false
		prefix = "memql.carried-work-receipt.v1\x00"
	}
	identity := struct {
		Run  any
		Step any
	}{r, map[string]any{
		"id": s.ID, "createdAt": s.CreatedAt, "ownerUserId": s.OwnerUserID,
		"runId": s.RunID, "key": s.Key, "status": s.Status, "stepType": s.StepType,
		"kind": s.Kind, "attempt": s.Attempt, "finishedAt": s.FinishedAt,
		"call": step["call"], "result": step["result"],
	}}
	body, err := json.Marshal(identity)
	if err != nil {
		return out, err
	}
	digest := sha256.Sum256(append([]byte(prefix), body...))
	ids := slices.Clone(s.Result.Metadata.ArtifactIntentIDs)
	slices.Sort(ids)
	// The input's tree hash stays out of the digest above, read beside the decode
	// rather than through it. An engine that predates tree hashes computes the same
	// digest for the same rows, so a rollback refuses no candidate that did not
	// reuse a step (TestAReceiptsDigestIgnoresItsInputTree). The digest still commits
	// to the tree where it matters: the step's treeFingerprint is part of its call.
	input, _ := run["input"].(map[string]any)
	treeHash, _ := input["treeHash"].(string)
	out = ReleaseWorkReceipt{
		OwnerUserID: owner, WorkRunID: runID, StepKey: stepKey, ReceiptID: s.ID,
		Repository: r.Input.Repository, Commit: r.Input.SHA, Mode: r.Input.Mode, Event: r.Input.Event,
		Attempt: s.Attempt, DefinitionDigest: definition, ReceiptDigest: "sha256:" + hex.EncodeToString(digest[:]), ArtifactIntentIDs: ids,
		StepKind: s.Call.PipelineKind, Status: s.Status, TreeHash: treeHash,
	}
	if s.Call.Skip != nil {
		out.SkipCode = s.Call.Skip.Code
	}
	return out, nil
}
