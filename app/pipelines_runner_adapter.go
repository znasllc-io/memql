package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// pipelines_runner_adapter.go -- the workbench node's pipeline runner, behind
// the four pipeline forward actions (epic memql#5478, #5493, #5495).
//
// integrations/workbench carries the actions' payloads as JSON and never
// imports the runner, which imports it for the action names; the runner speaks
// its own types. This adapter is the one place the two meet: it decodes what
// the agent's executor sent into the runner's request, and encodes the
// runner's answer into what the executor reads. It decides nothing about a
// step.
//
// Untagged, though only the workbench builds a runner, so the translation and
// the decision below are tested by every `make test`; integrations_workbench.go
// (workbench) is what calls them.

// The codes the adapter answers on a forward's envelope, where the agent's
// executor reads a refusal without parsing a payload. They are this hop's
// transport vocabulary, never a step's outcome: a step's own codes are the
// seam's catalogue (component/pipelines) and ride its result.
const (
	// pipelineArgsUnreadable is integrations/workbench's own code for a
	// forward whose args do not decode, so one mistake has one name on this
	// hop whichever action made it.
	pipelineArgsUnreadable = "decode_args"
	// pipelineAckFailed: the runner could not delete the acked step's Job or
	// Secret. Its TTL still collects them.
	pipelineAckFailed = "ack_failed"
	// pipelineCancelFailed: the runner could not delete every Job and Secret
	// of the cancelled run. The reply still counts the Jobs it did delete.
	pipelineCancelFailed = "cancel_failed"
	// pipelineReplyUnencodable: the runner answered and its answer could not
	// be encoded. A code rather than an empty payload, because the executor
	// reads a cancel with no code as done.
	pipelineReplyUnencodable = "encode_reply"
)

// pipelineStepRunner is *pipelinesteps.Runner, as the adapter calls it.
type pipelineStepRunner interface {
	Run(ctx context.Context, run pipelinesteps.StepRun) pl.StepResult
	Status(ctx context.Context, req pipelinesteps.StatusRequest) pipelinesteps.StatusReply
	Ack(ctx context.Context, req pipelinesteps.AckRequest) error
	CancelRun(ctx context.Context, req pipelinesteps.CancelRequest) (int, error)
}

var (
	_ pipelineStepRunner       = (*pipelinesteps.Runner)(nil)
	_ workbench.PipelineRunner = (*pipelinesRunnerAdapter)(nil)
)

// pipelinesRunnerAdapter implements workbench.PipelineRunner over the
// substrate's runner.
type pipelinesRunnerAdapter struct {
	runner pipelineStepRunner
	logger *slog.Logger
}

// pipelineCancelReply is the pipelineCancel reply: how many of the run's Jobs
// were deleted.
type pipelineCancelReply struct {
	JobsDeleted int `json:"jobsDeleted"`
}

// RunStep runs one step to its outcome. ctx ending is a CANCEL -- the runner
// deletes the step's Job -- which is why the handler ends it only on a
// WorkbenchForwardCancel and never with the stream the step arrived on.
func (p *pipelinesRunnerAdapter) RunStep(ctx context.Context, argsJSON []byte) []byte {
	var run pipelinesteps.StepRun
	if err := json.Unmarshal(argsJSON, &run); err != nil {
		// The decode error stays in this node's log and out of the outcome:
		// the step's args carry its secrets, and an outcome is persisted and
		// shown to people.
		p.log().Warn("pipelines: a forwarded step could not be read, so nothing ran",
			"component", "pipelinesteps", "error", err)
		return p.outcome(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: -1, Failure: &pl.Failure{
			Code:    pl.CodeExecutorError,
			Message: "The workbench replica could not read the step it was sent (its log says why), so nothing ran.",
		}})
	}
	return p.outcome(p.runner.Run(ctx, run))
}

// Status says where a step's Job stands.
func (p *pipelinesRunnerAdapter) Status(ctx context.Context, argsJSON []byte) ([]byte, string) {
	var req pipelinesteps.StatusRequest
	if !p.decode(workbench.PipelineStatusAction, argsJSON, &req) {
		return nil, pipelineArgsUnreadable
	}
	return p.reply(workbench.PipelineStatusAction, p.runner.Status(ctx, req))
}

// Ack deletes a step's Job and Secret once the agent holds its outcome.
func (p *pipelinesRunnerAdapter) Ack(ctx context.Context, argsJSON []byte) string {
	var req pipelinesteps.AckRequest
	if !p.decode(workbench.PipelineAckAction, argsJSON, &req) {
		return pipelineArgsUnreadable
	}
	if err := p.runner.Ack(ctx, req); err != nil {
		p.log().Warn("pipelines: an acked step's Job or Secret could not be deleted; its TTL will",
			"component", "pipelinesteps", "jobName", req.JobName, "error", err)
		return pipelineAckFailed
	}
	return ""
}

// CancelRun deletes every Job and Secret of a run.
func (p *pipelinesRunnerAdapter) CancelRun(ctx context.Context, argsJSON []byte) ([]byte, string) {
	var req pipelinesteps.CancelRequest
	if !p.decode(workbench.PipelineCancelAction, argsJSON, &req) {
		return nil, pipelineArgsUnreadable
	}
	deleted, err := p.runner.CancelRun(ctx, req)
	reply, code := p.reply(workbench.PipelineCancelAction, pipelineCancelReply{JobsDeleted: deleted})
	if err != nil {
		p.log().Warn("pipelines: a cancelled run's Jobs or Secrets could not all be deleted",
			"component", "pipelinesteps", "runId", req.RunID, "jobsDeleted", deleted, "error", err)
		return reply, pipelineCancelFailed
	}
	return reply, code
}

// decode reads a forward's args, logging what it cannot.
func (p *pipelinesRunnerAdapter) decode(action string, argsJSON []byte, into any) bool {
	if err := json.Unmarshal(argsJSON, into); err != nil {
		p.log().Warn("pipelines: a forwarded "+action+" could not be read; the runner was not asked",
			"component", "pipelinesteps", "error", err)
		return false
	}
	return true
}

// reply encodes a runner's answer for the forward's payload.
func (p *pipelinesRunnerAdapter) reply(action string, v any) ([]byte, string) {
	out, err := json.Marshal(v)
	if err != nil {
		p.log().Error("pipelines: the runner's "+action+" answer could not be encoded",
			"component", "pipelinesteps", "error", err)
		return nil, pipelineReplyUnencodable
	}
	return out, ""
}

// outcome encodes a step's result. One that cannot be encoded is answered as
// the engine's own failure rather than as nothing: the agent's executor parks
// on this reply, and an empty one reads as no answer at all.
func (p *pipelinesRunnerAdapter) outcome(res pl.StepResult) []byte {
	out, err := json.Marshal(res)
	if err == nil {
		return out
	}
	p.log().Error("pipelines: a step's outcome could not be encoded", "component", "pipelinesteps", "error", err)
	failed, _ := json.Marshal(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: -1, Where: res.Where, Failure: &pl.Failure{
		Code:    pl.CodeExecutorError,
		Message: "The workbench replica could not encode the step's outcome (its log says why).",
	}})
	return failed
}

func (p *pipelinesRunnerAdapter) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// workbenchPipelineRunner is this workbench node's pipeline runner, or a NIL
// INTERFACE when the node cannot run steps -- never a typed nil, which the
// forward handler would call instead of answering pipelines_not_configured.
//
// A node runs steps when it has BOTH a clone image (cfg.CloneImage, which has
// no default: an image nobody chose would be a guess about what may run with a
// repository token) and an in-cluster API server to create Jobs through. A
// node with either missing is a cluster set up without pipelines, and says so
// ONCE, at Info, naming what is missing; one that has both and still cannot
// build its API client is broken, and says so at Warn. Neither builds a
// client it will not use: newAPI and blobStore are called only on the way to
// a runner.
//
// cfg comes from pipelinesteps.ConfigFromEnv, NodeID included: this pod's
// MEMQL_NODE_ID, by the derivation component/node's identity uses, so the
// heartbeat this runner stamps on a Job names this replica exactly as the
// agent's peer table does -- which is what adoption keys on.
func (a *App) workbenchPipelineRunner(
	cfg pipelinesteps.Config,
	inCluster bool,
	newAPI func() (*deploycontrol.ClusterAPI, error),
	blobStore func() (server.FileUploader, string),
) workbench.PipelineRunner {
	var missing []string
	if strings.TrimSpace(cfg.CloneImage) == "" {
		missing = append(missing, "MEMQL_PIPELINES_CLONE_IMAGE (the image a step's clone runs, which has no default)")
	}
	if !inCluster {
		missing = append(missing, "an in-cluster Kubernetes API (a projected ServiceAccount token and KUBERNETES_SERVICE_HOST)")
	}
	if len(missing) > 0 {
		a.Logger.Info("pipelines: this workbench node cannot run pipeline steps; every pipeline forward to it answers "+
			workbench.ErrCodePipelinesNotConfigured,
			"component", "pipelinesteps", "missing", strings.Join(missing, "; "))
		return nil
	}
	api, err := newAPI()
	if err != nil {
		a.Logger.Warn("pipelines: this workbench node cannot run pipeline steps: its Kubernetes API client could not be built; "+
			"every pipeline forward to it answers "+workbench.ErrCodePipelinesNotConfigured,
			"component", "pipelinesteps", "error", err)
		return nil
	}
	uploader, bucket := blobStore()
	runner := pipelinesteps.NewRunner(cfg, pipelinesteps.NewKube(api, cfg.Namespace), currentPipelinesLineSink,
		a.pipelinesLibraryStoreFor(uploader, bucket), a.pipelinesTokenMinterFor())
	a.Logger.Info("pipelines: this workbench node runs pipeline steps as Kubernetes Jobs",
		"component", "pipelinesteps", "namespace", cfg.Namespace, "nodeId", cfg.NodeID,
		"objectStorage", uploader != nil && strings.TrimSpace(bucket) != "")
	return &pipelinesRunnerAdapter{runner: runner, logger: a.Logger}
}
