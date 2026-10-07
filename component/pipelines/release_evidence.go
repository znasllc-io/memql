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
}

// InspectReleaseWorkReceipt is a pure integrity check over an authorized,
// fresh journal read. It does not authorize a read, choose required checks,
// or certify the artifact bytes. Those responsibilities remain with the
// native reader, installed release workflow and artifact verifier respectively.
func InspectReleaseWorkReceipt(owner, runID, stepKey string, run, step map[string]any) (ReleaseWorkReceipt, error) {
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
		Call                                                                                  struct{ Construct, DefinitionFingerprint string }
		Result                                                                                struct {
			Status   string
			Metadata struct {
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
	definition := "sha256:" + strings.TrimPrefix(s.Call.DefinitionFingerprint, "work-definition-v2:")
	if !releaseIdentity(owner) || !releaseIdentity(runID) || !releaseIdentity(stepKey) ||
		r.ID != runID || r.OwnerUserID != owner || s.OwnerUserID != owner || s.RunID != runID || s.Key != stepKey ||
		!releaseIdentity(s.ID) || s.CreatedAt == "" || s.FinishedAt == "" || s.Attempt < 1 ||
		r.Status != "succeeded" || r.CancelRequested || r.CallerSuppliedPayload || r.Mode != "live" ||
		r.TriggeredBy != WorkTriggerPrefix+r.Input.Mode || r.Input.PipelineID == "" || r.Input.PipelineRunID == "" || r.Input.Attempt < 1 ||
		!strings.HasPrefix(r.TemplateFingerprint, "work-definition-v2:") || !releaseDigest("sha256:"+strings.TrimPrefix(r.TemplateFingerprint, "work-definition-v2:")) ||
		s.Status != "done" || s.ErrorCode != "" || s.StepType != "exec" || s.Kind != "deterministic" || s.Call.Construct != "pipeline" ||
		!strings.HasPrefix(s.Call.DefinitionFingerprint, "work-definition-v2:") || !releaseDigest(definition) ||
		s.Result.Status != string(OutcomeSucceeded) || s.Result.Metadata.ExitCode == nil || *s.Result.Metadata.ExitCode != 0 ||
		!ValidArtifactIntentIDs(s.Result.Metadata.ArtifactIntentIDs) || !releaseSource.MatchString(r.Input.Repository) || !releaseCommit.MatchString(r.Input.SHA) {
		return out, errors.New("release evidence requires a successful live pipeline and exact successful execution receipt")
	}
	// Hash only journal facts relevant to the receipt. Heartbeats and the node
	// currently serving the read may change without changing completed work.
	// Full call/result values are included, including fields unknown here.
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
	digest := sha256.Sum256(append([]byte("memql.release-work-receipt.v1\x00"), body...))
	ids := slices.Clone(s.Result.Metadata.ArtifactIntentIDs)
	slices.Sort(ids)
	return ReleaseWorkReceipt{
		OwnerUserID: owner, WorkRunID: runID, StepKey: stepKey, ReceiptID: s.ID,
		Repository: r.Input.Repository, Commit: r.Input.SHA, Mode: r.Input.Mode, Event: r.Input.Event,
		Attempt: s.Attempt, DefinitionDigest: definition, ReceiptDigest: "sha256:" + hex.EncodeToString(digest[:]), ArtifactIntentIDs: ids,
	}, nil
}
