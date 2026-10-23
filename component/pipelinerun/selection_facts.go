package pipelinerun

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Stage selection is an opt-in read contract, never repository authority. A
// legacy resolver must not acquire a new dependency on provider availability.
func (dr *runDriver) stageUsesPullRequest() (bool, error) {
	if dr.workflow == nil {
		return false, errors.New("stage policy has not been prepared")
	}
	a := dr.workflow.definitions["pipelineStageIncluded"]
	if s := dr.workflow.snapshot; s != nil {
		entry := s.PhaseEntry("pipelineStageIncluded")
		prepared, err := workflowhost.PrepareSnapshotEntry(s, pipelinePolicyContract, entry, workflowhost.Options{Operations: pipelinePolicyOperations()})
		if err != nil {
			return false, err
		}
		a, err = prepared.Automation(entry)
		if err != nil {
			return false, err
		}
	}
	if a == nil || a.Args == nil {
		return false, nil
	}
	for _, f := range a.Args.Fields {
		if f != nil && f.Name == "pullRequest" {
			if f.Type != "object" && f.Type != "any" {
				return false, errors.New("stage policy pullRequest argument must be an object")
			}
			return true, nil
		}
	}
	return false, nil
}

type selectionBinding struct {
	RunID          string `json:"runId"`
	OwnerUserID    string `json:"ownerUserId"`
	PipelineID     string `json:"pipelineId"`
	Repository     string `json:"repository"`
	SHA            string `json:"sha"`
	Event          string `json:"event"`
	PullRequest    int    `json:"pullRequest"`
	HeadBranch     string `json:"headBranch"`
	InstallationID string `json:"installationId"`
	CredentialID   string `json:"credentialId"`
	PolicyVersion  string `json:"policyVersion"`
}

type selectionObservation struct {
	Version     int              `json:"version"`
	Binding     selectionBinding `json:"binding"`
	ObservedAt  string           `json:"observedAt"`
	PullRequest map[string]any   `json:"pullRequest"`
}

func (dr *runDriver) selectionBinding(r Run, p Pipeline) (selectionBinding, error) {
	if bareID(r.ID) != bareID(dr.runID) || bareID(p.ID) != bareID(r.PipelineID) || !sameID(r.OwnerUserID, p.OwnerUserID) ||
		r.Repository == "" || r.Repository != p.Repository || r.SHA == "" || p.CredentialID == "" ||
		p.InstallationID <= 0 || dr.installation != p.InstallationID || dr.workflowIdentity() == "" {
		return selectionBinding{}, errors.New("stage facts do not match the run's current source identity")
	}
	return selectionBinding{
		RunID: bareID(r.ID), OwnerUserID: bareID(r.OwnerUserID), PipelineID: bareID(r.PipelineID),
		Repository: r.Repository, SHA: r.SHA, Event: string(r.Event), PullRequest: r.PullRequest,
		HeadBranch: r.HeadBranch, InstallationID: strconv.FormatInt(p.InstallationID, 10),
		CredentialID: bareID(p.CredentialID), PolicyVersion: dr.workflowIdentity(),
	}, nil
}

func selectionPR(head githubapp.PullRequestHead) map[string]any {
	return map[string]any{
		"present": true, "number": head.Number, "state": head.State, "author": head.Author,
		"headRef": head.HeadRef, "headRepository": normalizeRepository(head.HeadRepository),
		"headSha": strings.ToLower(head.HeadSHA), "baseSha": strings.ToLower(head.BaseSHA),
	}
}

func validateSelectionPR(value map[string]any, binding selectionBinding) error {
	if binding.Event != string(pipelines.EventPullRequest) {
		if len(value) == 1 && value["present"] == false {
			return nil
		}
		return errors.New("non-PR stage facts must explicitly report no pull request")
	}
	// Decode through the typed boundary, rejecting both missing and unknown
	// fields. Persisted JSON numbers and in-process integer values mean the same
	// thing; fractional/overflowing numbers are rejected by the JSON decoder.
	var pr struct {
		Present        bool   `json:"present"`
		Number         int    `json:"number"`
		State          string `json:"state"`
		Author         string `json:"author"`
		HeadRef        string `json:"headRef"`
		HeadRepository string `json:"headRepository"`
		HeadSHA        string `json:"headSha"`
		BaseSHA        string `json:"baseSha"`
	}
	if err := decodeSelectionObject(value, &pr); err != nil {
		return errors.New("pull-request facts are malformed")
	}
	validSHA := func(value string) bool { b, err := hex.DecodeString(value); return err == nil && len(b) == 20 }
	if len(value) != 8 || !pr.Present || binding.PullRequest <= 0 || pr.Number != binding.PullRequest || pr.State != "open" ||
		pr.HeadSHA != binding.SHA || pr.HeadRepository != binding.Repository || pr.HeadRef == "" || pr.HeadRef != binding.HeadBranch ||
		!plainSelectionText(pr.Author, 100) || !validSHA(pr.HeadSHA) || !validSHA(pr.BaseSHA) {
		return errors.New("pull request moved, closed, or did not match the run's exact source; no stages were scheduled")
	}
	return nil
}

func decodeSelectionObject(value map[string]any, result any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	return decoder.Decode(result)
}

func readSelectionObservation(value map[string]any, binding selectionBinding) (map[string]any, error) {
	var saved selectionObservation
	if err := decodeSelectionObject(value, &saved); err != nil || len(value) != 4 || saved.Version != 1 || saved.Binding != binding {
		return nil, errors.New("saved stage facts do not match this run, source and installed policy")
	}
	if _, err := time.Parse(time.RFC3339Nano, saved.ObservedAt); err != nil {
		return nil, errors.New("saved stage facts have no observation time")
	}
	if err := validateSelectionPR(saved.PullRequest, binding); err != nil {
		return nil, err
	}
	return maps.Clone(saved.PullRequest), nil
}

func (dr *runDriver) selectionActive(ctx context.Context, current Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.CancelRequested || dr.lease.isCancelled() {
		return context.Canceled
	}
	return nil
}

// Capture outside the run gate, then persist under its current claim and row
// version. A replacement replica consumes this exact observation, not the old
// process's memory or a newer provider answer. Existing write operations still
// revalidate current authority independently before every repository effect.
func (dr *runDriver) stagePullRequest(ctx context.Context) (map[string]any, error) {
	var binding selectionBinding
	var result map[string]any
	check := func(gctx context.Context, current Run) error {
		if err := dr.selectionActive(gctx, current); err != nil {
			return err
		}
		p, err := dr.d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), current.PipelineID)
		if err != nil || p == nil {
			return errors.New("stage facts source could not be verified")
		}
		binding, err = dr.selectionBinding(current, *p)
		if err != nil {
			return err
		}
		// Token authority was acquired for dr.p, not an edited source.
		original, err := dr.selectionBinding(dr.run, dr.p)
		if err != nil || binding != original {
			return errors.New("stage facts source changed during preparation")
		}
		if current.SelectionFacts != nil {
			result, err = readSelectionObservation(current.SelectionFacts, binding)
			if err == nil {
				dr.run.SelectionFacts = current.SelectionFacts
			}
			return err
		}
		return nil
	}
	if err := dr.underLease(ctx, check); err != nil || result != nil {
		return result, err
	}
	observedBinding := binding
	observed := map[string]any{"present": false}
	if dr.run.Event == pipelines.EventPullRequest {
		providerCtx, done := dr.stoppable(ctx, githubCallTimeout)
		head, err := dr.d.GitHub.PullRequestHead(providerCtx, dr.token, binding.Repository, binding.PullRequest)
		providerStopped := providerCtx.Err()
		done()
		if err != nil || providerStopped != nil {
			return nil, errors.New("pull-request facts could not be verified with the repository provider; no stages were scheduled")
		}
		observed = selectionPR(head)
	}
	if err := validateSelectionPR(observed, binding); err != nil {
		return nil, err
	}
	proof := selectionObservation{Version: 1, Binding: binding, ObservedAt: dr.d.now().UTC().Format(time.RFC3339Nano), PullRequest: observed}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	var saved map[string]any
	if err := json.Unmarshal(encoded, &saved); err != nil {
		return nil, err
	}
	err = dr.underLease(ctx, func(gctx context.Context, current Run) error {
		if err := check(gctx, current); err != nil {
			return err
		}
		if result != nil {
			return nil // Another accepted observation is immutable.
		}
		if binding != observedBinding {
			return errors.New("stage facts source changed while the provider was answering")
		}
		if err := dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, RunPatch{SelectionFacts: &saved}); err != nil {
			return err
		}
		dr.run.SelectionFacts = saved
		result = maps.Clone(observed)
		return nil
	})
	return result, err
}

func plainSelectionText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0
}

func stageSelectionDecision(value any) (bool, string, error) {
	if included, ok := value.(bool); ok {
		return included, "", nil
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return false, "", errors.New("stage policy must return bool or {included: bool, reason: string}")
	}
	included, includedOK := object["included"].(bool)
	reason, reasonOK := object["reason"].(string)
	if !includedOK || !reasonOK || !plainSelectionText(reason, 512) {
		return false, "", errors.New("stage policy decision requires included and a plain-text reason of at most 512 characters")
	}
	return included, strings.TrimSpace(reason), nil
}

func (dr *runDriver) recordStageDecision(ctx context.Context, stage string, included bool, reason string) error {
	if reason == "" {
		return nil
	}
	outcome := "Skipped"
	if included {
		outcome = "Selected"
	}
	note := Note{Code: "pipeline_stage_selection:" + stage, Message: fmt.Sprintf("%s %s: %s", outcome, stage, dr.mask(reason))}
	return dr.underLease(ctx, func(gctx context.Context, current Run) error {
		if err := dr.selectionActive(gctx, current); err != nil {
			return err
		}
		for _, existing := range current.Notes {
			if existing.Code == note.Code {
				if existing.Message != note.Message {
					return errors.New("stage policy decision changed from its saved explanation")
				}
				return nil
			}
		}
		notes := withNote(current.Notes, note)
		if err := dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, RunPatch{Notes: &notes}); err != nil {
			return err
		}
		dr.run.Notes = notes
		return nil
	})
}
