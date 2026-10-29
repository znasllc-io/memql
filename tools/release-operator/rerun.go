package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	client "github.com/znasllc-io/memql/sdk/go/client"
)

// Exact original PR attempts interrupted by the controller revision change.
// No run ID, head, event, mode or source override is admitted.
//
//go:embed rerun-sources.json
var rerunSourceJSON []byte

type rerunSource struct {
	ID              string `json:"id"`
	OwnerUserID     string `json:"ownerUserId"`
	AccountID       string `json:"accountId"`
	PipelineID      string `json:"pipelineId"`
	Repository      string `json:"repository"`
	SHA             string `json:"sha"`
	BaseSHA         string `json:"baseSha"`
	Event           string `json:"event"`
	Mode            string `json:"mode"`
	Trigger         string `json:"trigger"`
	Status          string `json:"status"`
	Conclusion      string `json:"conclusion"`
	FailureCode     string `json:"failureCode"`
	Attempt         int    `json:"attempt"`
	PullRequest     int    `json:"pullRequest"`
	HeadBranch      string `json:"headBranch"`
	WorkRunID       string `json:"workRunId"`
	FinishedAt      string `json:"finishedAt"`
	QueuedAt        string `json:"queuedAt"`
	CheckRunID      string `json:"checkRunId"`
	RerunOf         string `json:"rerunOf"`
	RunKey          string `json:"runKey"`
	Version         string `json:"version"`
	CancelRequested bool   `json:"cancelRequested"`
}

type rerunPipeline struct {
	ID             string `json:"id"`
	OwnerUserID    string `json:"ownerUserId"`
	AccountID      string `json:"accountId"`
	PackageID      string `json:"packageId"`
	Repository     string `json:"repository"`
	Status         string `json:"status"`
	DefaultBranch  string `json:"defaultBranch"`
	InstallationID string `json:"installationId"`
	CredentialID   string `json:"credentialId"`
}

var rerunSources = func() map[string]rerunSource {
	var m map[string]rerunSource
	if err := json.Unmarshal(rerunSourceJSON, &m); err != nil || len(m) != 2 {
		panic("invalid fixed rerun source map")
	}
	return m
}()

const (
	rerunWhole              = "whole"
	rerunFailedOnly         = "failed-only"
	failedOnlyRunID         = "cec99162f28f29f58fb802029ca05864"
	failedOnlyEngineVersion = "0.31.7"
	failedOnlyEngineCommit  = "565af0d938e56e8aa79bc53ed5aefc457b8344c6"
)

// The failed-only candidate was observed while running. Its immutable source
// fields are fixed here; terminal lifecycle fields must be observed live.
//
//go:embed rerun-failed-only-source.json
var failedOnlySourceJSON []byte
var failedOnlySource = func() rerunSource {
	var source rerunSource
	if err := json.Unmarshal(failedOnlySourceJSON, &source); err != nil || source.ID != failedOnlyRunID {
		panic("invalid fixed failed-only source")
	}
	return source
}()

func rerunMode(o options) string {
	if o.rerunMode == "" {
		return rerunWhole
	}
	return o.rerunMode
}
func selectedRerun(o options) (rerunSource, error) {
	var source rerunSource
	switch rerunMode(o) {
	case rerunWhole:
		var ok bool
		source, ok = rerunSources[o.run]
		if !ok {
			return source, errors.New("whole rerun is limited to the two reviewed original PR run IDs")
		}
	case rerunFailedOnly:
		if o.run != failedOnlyRunID {
			return source, errors.New("failed-only rerun is limited to the reviewed installer attempt 2")
		}
		source = failedOnlySource
	default:
		return source, errors.New("rerun-mode must be whole or failed-only")
	}
	if o.commit != "" || o.tag != "" || o.candidate != "" || o.approval != "" || o.target != "" || o.artifact != "" || o.configuration != "" || o.output != "" {
		return rerunSource{}, errors.New("rerun cannot accept release, source or destination overrides")
	}
	return source, nil
}

func rerunCall(o options) string {
	return client.PipelinesRerunBuild(client.PipelinesRerunArgs{RunId: o.run, FailedOnly: rerunMode(o) == rerunFailedOnly, FailedOnlySet: true})
}

func checkRerunEngine(o options, version, commit string) error {
	if rerunMode(o) == rerunFailedOnly && (version != failedOnlyEngineVersion || (commit != failedOnlyEngineCommit && commit != failedOnlyEngineCommit[:12])) {
		return errors.New("failed-only source qualification requires the reviewed installed engine 0.31.7 at 565af0d938e5")
	}
	return nil
}

const rerunRequirements = "Before invocation root must independently capture the newest-attempt projection for this exact run key and require this source is newest, the current GitHub PR head, admission and controller stability. The helper reads only exact owner-scoped rows. The native builtin rechecks owner/source and refuses an unfinished newest attempt under its run-key gate; it does not promise the selected source is newest if a newer completed attempt exists."
const failedOnlySemantics = "The native driver alone decides carry-forward: command steps previously done or pipeline_passed_earlier need the same nonempty execution definition fingerprint and package slice. Fingerprints bind the engine revision. Notifications and report producers never carry; unreached work, changed shards and unmatched definitions can execute. This is not an OS-check-only retry."

func rerunPlan(o options, call string) map[string]any {
	return map[string]any{"ok": true, "planned": true, "operation": o.op, "nativeCall": call,
		"source": func() rerunSource { s, _ := selectedRerun(o); return s }(), "rerunMode": rerunMode(o), "failedOnly": rerunMode(o) == rerunFailedOnly, "livePreflightPerformed": false,
		"carryForward": failedOnlySemantics, "requiredEngineForFailedOnly": failedOnlyEngineCommit,
		"requires": rerunRequirements,
		"retry":    "no automatic retry; failed-only execution reserves a local exclusive intent before its single call. Reconcile durable native runs and GitHub checks after any uncertain response; do not remove or relocate the intent to replay."}
}

// Query results are shaped rows, not builtin result nodes.
func decodeOneRow[T any](raw any) (T, error) {
	rows, err := decodeRows[T](raw)
	if err != nil || len(rows) != 1 {
		var zero T
		if err == nil {
			err = errors.New("native query did not return exactly one owner-scoped row")
		}
		return zero, err
	}
	return rows[0], nil
}
func decodeRows[T any](raw any) ([]T, error) {
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []T `json:"data"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func checkRerunSource(want, got rerunSource) error {
	if got.Status != "completed" || got.Conclusion != "failure" || got.FinishedAt == "" {
		return errors.New("source run is not the recorded terminal failed attempt")
	}
	if got != want {
		return errors.New("source run differs from the exact reviewed repository/head/event/attempt/receipt binding")
	}
	return nil
}
func checkRerunPipeline(p rerunPipeline, source rerunSource) error {
	if p.ID != canonicalPipeline || p.ID != source.PipelineID || p.OwnerUserID != source.OwnerUserID ||
		p.AccountID != source.AccountID || p.Repository != source.Repository || p.Status != "active" ||
		p.PackageID != "6a97392a-6801-4ab3-b779-e02460329b65" || p.DefaultBranch != "main" ||
		p.InstallationID != "158882932" || p.CredentialID != "b1649763-a25a-4df7-beb4-b948c08e3294" {
		return errors.New("current pipeline source, owner, repository or active connection differs from the reviewed source")
	}
	return nil
}

// Only lifecycle fields may change between the initial running observation and
// the first terminal preflight. The two live observations must match in full.
func checkFailedOnlySource(got rerunSource) error {
	if got.Status != "completed" || (got.Conclusion != "failure" && got.Conclusion != "cancelled") {
		return errors.New("failed-only source must be a completed failure or cancellation")
	}
	finished, err := time.Parse(time.RFC3339Nano, got.FinishedAt)
	queued, queuedErr := time.Parse(time.RFC3339Nano, got.QueuedAt)
	if err != nil || queuedErr != nil || finished.Before(queued) {
		return errors.New("failed-only source has no valid terminal timestamp")
	}
	want := failedOnlySource
	want.Status, want.Conclusion, want.FailureCode, want.FinishedAt, want.CancelRequested = got.Status, got.Conclusion, got.FailureCode, got.FinishedAt, got.CancelRequested
	if got != want {
		return errors.New("failed-only source differs from the reviewed repository/head/event/attempt/receipt binding")
	}
	return nil
}
func checkSelectedRerunSource(o options, want, got rerunSource) error {
	if rerunMode(o) == rerunFailedOnly {
		return checkFailedOnlySource(got)
	}
	return checkRerunSource(want, got)
}

type rerunStep struct {
	ID          string `json:"id"`
	OwnerUserID string `json:"ownerUserId"`
	RunID       string `json:"runId"`
	Key         string `json:"key"`
	Status      string `json:"status"`
}

func checkFailedOnlyStep(step rerunStep, source rerunSource) error {
	if step.ID == "" || step.OwnerUserID != source.OwnerUserID || step.RunID != source.WorkRunID || step.Key != "tests.os-checks" || (step.Status != "failed" && step.Status != "cancelled") {
		return errors.New("the reviewed owner-scoped OS-check receipt is not failed or cancelled in this exact source Work run")
	}
	return nil
}

type rerunReader interface {
	source(context.Context, string) (rerunSource, error)
	pipeline(context.Context, string) (rerunPipeline, error)
}
type failedOnlyReader interface {
	failedStep(context.Context, string) (rerunStep, error)
}
type sdkRerunReader struct{ qc *client.QueryClient }

func (r sdkRerunReader) failedStep(ctx context.Context, id string) (rerunStep, error) {
	result, err := r.qc.WorkStepForOwnerRun(ctx, client.WorkStepForOwnerRunArgs{RunId: id, StepKey: "tests.os-checks"})
	if err != nil {
		return rerunStep{}, err
	}
	return decodeOneRow[rerunStep](result.Raw())
}

func (r sdkRerunReader) source(ctx context.Context, id string) (rerunSource, error) {
	result, err := r.qc.PipelineRunForOwner(ctx, client.PipelineRunForOwnerArgs{RunId: id})
	if err != nil {
		return rerunSource{}, err
	}
	return decodeOneRow[rerunSource](result.Raw())
}
func (r sdkRerunReader) pipeline(ctx context.Context, id string) (rerunPipeline, error) {
	result, err := r.qc.PipelineForOwner(ctx, client.PipelineForOwnerArgs{PipelineId: id})
	if err != nil {
		return rerunPipeline{}, err
	}
	return decodeOneRow[rerunPipeline](result.Raw())
}
func rerunStage(o options, stage string) {
	if o.op == "rerun" || o.op == "rerun-preflight" {
		fmt.Fprintln(os.Stderr, "rerun preflight stage:", stage)
	}
}

func preflightRerun(ctx context.Context, reader rerunReader, o options) (rerunSource, error) {
	expected, err := selectedRerun(o)
	if err != nil {
		return rerunSource{}, err
	}
	rerunStage(o, "read-exact-source")
	source, err := reader.source(ctx, o.run)
	if err != nil {
		return rerunSource{}, err
	}
	if err = checkSelectedRerunSource(o, expected, source); err != nil {
		return rerunSource{}, err
	}
	rerunStage(o, "read-active-pipeline")
	pipeline, err := reader.pipeline(ctx, source.PipelineID)
	if err != nil {
		return rerunSource{}, err
	}
	if err = checkRerunPipeline(pipeline, source); err != nil {
		return rerunSource{}, err
	}
	if rerunMode(o) == rerunFailedOnly {
		stepReader, ok := reader.(failedOnlyReader)
		if !ok {
			return rerunSource{}, errors.New("exact failed Work-step reader is unavailable")
		}
		rerunStage(o, "read-exact-failed-step")
		step, err := stepReader.failedStep(ctx, source.WorkRunID)
		if err != nil {
			return rerunSource{}, err
		}
		if err = checkFailedOnlyStep(step, source); err != nil {
			return rerunSource{}, err
		}
	}
	first := source
	// Reobserve after the other reads. The native builtin still enforces current
	// owner, grant, admission and execution; this does not bypass those fences.
	rerunStage(o, "reread-exact-source")
	source, err = reader.source(ctx, o.run)
	if err != nil {
		return rerunSource{}, err
	}
	if err = checkSelectedRerunSource(o, expected, source); err != nil {
		return rerunSource{}, err
	}
	if source != first {
		return rerunSource{}, errors.New("source changed during rerun preflight")
	}
	return source, nil
}
