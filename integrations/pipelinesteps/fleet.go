//go:build agent

package pipelinesteps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// fleet.go -- a step naming a need the cluster cannot meet, run on one of its
// owner's own machines (epic memql#5478, #5494; design D10: the fleet is the
// routed exception).
//
// It goes through the agent's existing dispatcher as workerHost.pipeline_step
// under the PIPELINE PURPOSE (integrations/agent/worker/pipeline_purpose.go):
// no agent is named, and the two consents are the pipeline owner's
// (compute: cluster_and_fleet, checked before the step got here) and the
// machine owner's (its policy advertises pipelines=allowed, which the request
// requires). The owner's off switch still holds. The cockpit clones the
// repository at the SHA and runs the command with the same contract a Job
// gives it; its output streams back as chunks, which become the step's log
// through the same Capture the cluster's runner uses -- masked, stored under
// the run, archived to the Library, tailed -- and its artifacts come back
// packed in the result, read within the same limits.
//
// Agent-tagged: the dispatcher is.

// FleetDispatcher is the agent's worker dispatcher as the fleet path uses it:
// *worker.Dispatcher on an agent node, a fake in a test.
type FleetDispatcher interface {
	Dispatch(ctx context.Context, req worker.Request) (worker.Result, error)
}

const (
	// fleetCloneSlack is the dispatch's time beyond the step's own timeout:
	// the machine clones before the command and packs artifacts after it,
	// and the dispatcher's default (five minutes) would end a long step early.
	fleetCloneSlack = 5 * time.Minute
	// fleetMaxPartialLine bounds a line still waiting for its newline. The
	// cockpit cuts a line longer than 64 KiB where no secret straddles the
	// cut, so a partial past this is fed as a line of its own at exactly such
	// a cut, rather than held without bound.
	fleetMaxPartialLine = 64 << 10
	// surfaceFleet is pl.Where.Surface for a step run on a machine.
	surfaceFleet = "fleet"
)

// Fleet runs steps on the owner's machines.
type Fleet struct {
	cfg      Config
	dispatch FleetDispatcher
	library  LibraryStore
	tokens   TokenMinter
	sink     func() LineSink
	logger   *slog.Logger

	now         func() time.Time
	tempDir     string
	openCapture func(CaptureOptions) (*Capture, error)
}

var _ FleetRouter = (*Fleet)(nil)

// NewFleet builds the fleet path. library or tokens may be nil: a step's
// files are then a note rather than a Library file, and only a public
// repository can be cloned.
func NewFleet(cfg Config, d FleetDispatcher, library LibraryStore, tokens TokenMinter, sink func() LineSink, logger *slog.Logger) *Fleet {
	if logger == nil {
		logger = slog.Default()
	}
	return &Fleet{
		cfg:         cfg,
		dispatch:    d,
		library:     library,
		tokens:      tokens,
		sink:        sink,
		logger:      logger,
		now:         time.Now,
		openCapture: NewCapture,
	}
}

// fleetOutput is the cockpit's pipeline_step result.
type fleetOutput struct {
	ExitCode           *int     `json:"exitCode"`
	DurationMs         int64    `json:"durationMs"`
	ArtifactsTgzBase64 string   `json:"artifactsTgzBase64"`
	ArtifactsMissing   []string `json:"artifactsMissing"`
	ArtifactsTooLarge  bool     `json:"artifactsTooLarge"`
}

// RunStep runs one step on a machine that offers what it needs.
func (f *Fleet) RunStep(ctx context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error) {
	if f == nil || f.dispatch == nil {
		return failedResult(pl.CodeRunnerUnavailable,
			"This agent node has no dispatcher to reach the owner's machines with. Nothing ran."), nil
	}
	token, refusal, ok := f.cloneToken(ctx, run)
	if !ok {
		return refusal, nil
	}
	values := secretValues(run, token)
	mask := func(s string) string { return pl.MaskSecrets(s, values) }

	dir, err := os.MkdirTemp(f.tempDir, "memql-pipeline-step-")
	if err != nil {
		return failedResult(pl.CodeExecutorError, "The step's log could not be given a place on this node: "+err.Error()), nil
	}
	defer os.RemoveAll(dir)
	capture, err := f.openCapture(CaptureOptions{
		RunID:         run.RunID,
		WorkRunID:     run.WorkRunID,
		StepKey:       run.StepKey,
		Secrets:       values,
		StoreMaxLines: f.cfg.LogStoreMaxLines,
		ArchiveMax:    f.cfg.ArchiveMaxBytes,
		ArchivePath:   filepath.Join(dir, "step.log"),
		Sink:          f.lineSink(),
	})
	if err != nil {
		return failedResult(pl.CodeExecutorError, "The step's log could not be opened on this node: "+err.Error()), nil
	}
	defer capture.Close()
	lines := &fleetLines{capture: capture, pending: map[string][]byte{}}

	started := f.now()
	result, err := f.dispatchStep(ctx, req, run, token, lines.chunk)
	finished := f.now()
	// The machine's last partial lines are output; a chunk that arrives after
	// this -- the dispatcher stopped waiting on a timeout or a cancel -- is
	// dropped rather than racing the archive.
	lines.close()
	if err != nil {
		return failedResult(pl.CodeRunnerUnavailable, "The agent's dispatcher could not take the step: "+mask(err.Error())), nil
	}

	out, outErr := parseFleetOutput(result)
	res := f.classify(ctx, req, run, result, out, outErr, mask)
	if res.Status == pl.OutcomeRefused {
		// Nothing ran anywhere: no log, no files.
		return res, nil
	}
	res.StartedAt, res.FinishedAt = started.UTC().Format(time.RFC3339), finished.UTC().Format(time.RFC3339)
	if result.WorkerId != "" {
		capture.Note(fmt.Sprintf("memql: the step ran on machine %s, held by replica %s", result.WorkerId, result.NodeId))
	}

	files := &stepFiles{library: f.library, run: run, mask: mask}
	artifacts, tooLarge := f.artifacts(run, out, files)
	if tooLarge {
		if res.Status == pl.OutcomeSucceeded {
			// A failure-class code: the step fails with it, and keeps the
			// exit status its command chose.
			res.Status = pl.OutcomeFailed
			res.Failure = &pl.Failure{Code: pl.CodeArtifactTooLarge, Message: fmt.Sprintf(
				"The step's artifacts are larger than this cluster keeps (%d bytes), so none was stored.", f.cfg.ArtifactMaxBytes)}
		} else {
			// The command's own failure is the answer; the dropped artifacts
			// are said where the step's log says everything else.
			capture.Note("memql: the step's artifacts were larger than this cluster keeps and were dropped")
		}
	}

	closed, cerr := capture.Close()
	if cerr != nil {
		f.logger.Warn("pipelines: a fleet step's log archive could not be written in full",
			slog.String("runId", run.RunID), slog.String("stepKey", run.StepKey), slog.String("error", cerr.Error()))
	}
	res.LogTail, res.LogLines, res.LogCapped = closed.Tail, closed.Lines, closed.StoreCapped
	res.LogFileID = files.storeLog(ctx, closed.ArchivePath)
	res.ArtifactFileIDs = files.storeArtifacts(ctx, artifacts)
	if closed.StoreCapped {
		res.Notes = append(res.Notes, pl.Failure{Code: pl.CodeLogCapped, Message: fmt.Sprintf(
			"The live log of this step stops at %d lines; the complete log is in the Library.", f.cfg.LogStoreMaxLines)})
	}
	res.Notes = append(res.Notes, files.notes...)
	if run.GoTimings {
		var note *pl.Failure
		if res.Timings, note = f.goTimings(closed.ArchivePath); note != nil {
			note.Message = mask(note.Message)
			res.Notes = append(res.Notes, *note)
		}
	}
	return res, nil
}

// cloneToken mints the token the machine clones with: none for an anonymous
// clone of a public repository (installation 0). A token that cannot be
// minted runs nothing.
func (f *Fleet) cloneToken(ctx context.Context, run StepRun) (string, pl.StepResult, bool) {
	if run.InstallationID == 0 {
		return "", pl.StepResult{}, true
	}
	if f.tokens == nil {
		return "", failedResult(pl.CodeCloneFailed, fmt.Sprintf(
			"The repository %s is cloned under its GitHub App installation, and this agent node has no way to mint "+
				"its token. Nothing ran.", run.Repository.FullName())), false
	}
	token, err := f.tokens.CloneToken(ctx, run.InstallationID, run.Repository.Owner, run.Repository.Name)
	if err != nil {
		if ctx.Err() != nil {
			return "", stoppedResult(ctx, run.DeadlineCode), false
		}
		return "", failedResult(pl.CodeCloneFailed, fmt.Sprintf(
			"A token to clone %s could not be minted, so the step did not run: %v", run.Repository.FullName(), err)), false
	}
	return token, pl.StepResult{}, true
}

// dispatchStep sends the step through the agent's dispatcher.
//
// UNDER INTERNAL ORIGIN, because the dispatcher admits the pipeline purpose
// from the engine's own Go and from nothing a request can reach: the purpose
// is what switches the agent gates off, so a caller able to claim it would
// switch them off for itself. The stamp is on a context made for this one
// call and never returned.
//
// UNDER THE OWNER'S FORWARDED AUTHORITY, because a machine whose stream a
// sibling replica holds is reached over a forward, and the receiving replica
// checks the machine against the asserted subject -- never against the
// envelope's owner field. Without one a sibling-held machine is skipped.
func (f *Fleet) dispatchStep(ctx context.Context, req pl.StepRequest, run StepRun, token string, onChunk func(*nodev1.WorkerForwardStream)) (worker.Result, error) {
	labels := worker.EnvironmentNeedsLabels(req.Step.Needs, "")
	if labels == nil {
		labels = map[string]string{}
	}
	labels[worker.PipelinesLabel] = worker.PipelinesAllowed

	owned, err := ownerAuthority(ctx, run.OwnerUserID)
	if err != nil {
		return worker.Result{}, err
	}
	return f.dispatch.Dispatch(auth.ContextWithInternalOrigin(owned), worker.Request{
		Tool:          "workerHost",
		Action:        worker.PipelineStepAction,
		Purpose:       worker.PurposePipeline,
		Args:          fleetArgs(run, token),
		OwnerUserId:   run.OwnerUserID,
		RunId:         run.WorkRunID,
		StepId:        run.StepKey,
		CorrelationId: run.RunID,
		Timeout:       time.Duration(run.TimeoutSeconds)*time.Second + fleetCloneSlack,
		RequireLabels: labels,
		OnStreamChunk: onChunk,
	})
}

// ownerAuthority binds the pipeline owner's own assertion to ctx, as a
// verified forwarded authority (the model pull runner's precedent). The owner
// is the pipeline row's, copied by the driver -- never a caller's argument.
func ownerAuthority(ctx context.Context, ownerUserID string) (context.Context, error) {
	now := time.Now()
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: ownerUserID, Role: auth.RoleWriter}, "", "", time.Time{}, now)
	if err != nil {
		return nil, fmt.Errorf("asserting the pipeline owner's authority for the machine hop: %w", err)
	}
	verified, err := auth.VerifyForwardedAuthority(authority, now)
	if err != nil {
		return nil, fmt.Errorf("asserting the pipeline owner's authority for the machine hop: %w", err)
	}
	return auth.BindForwardedContext(ctx, authority.Principal().Claims, verified, authority), nil
}

// fleetArgs is the cockpit's pipeline_step contract. The clone URL and the
// repository name are built from ONE record, because the cockpit refuses a
// clone URL whose path is not the repository's -- and any path prefix with
// it -- so only the record's host is kept.
func fleetArgs(run StepRun, token string) map[string]any {
	secrets := run.Secrets
	if secrets == nil {
		secrets = map[string]string{}
	}
	artifacts := run.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}
	return map[string]any{
		"cloneUrl":   fleetCloneURL(run.Repository),
		"sha":        run.SHA,
		"token":      token,
		"repository": run.Repository.FullName(),
		"command":    run.Command,
		"env":        run.Env,
		"secrets":    secrets,
		"artifacts":  artifacts,
		"timeoutSec": run.TimeoutSeconds,
	}
}

// fleetCloneURL is https://<host>/<owner>/<name>.git, the host taken from the
// repository's clone URL when that is an https URL with a host, else GitHub.
func fleetCloneURL(repo pl.Repository) string {
	host := "github.com"
	if u, err := url.Parse(strings.TrimSpace(repo.CloneURL)); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
		host = u.Host
	}
	return "https://" + host + "/" + repo.Owner + "/" + repo.Name + ".git"
}

// parseFleetOutput reads a machine's answer: a result whose output is the
// contract, or a refusal (no output, nothing to read).
func parseFleetOutput(r worker.Result) (*fleetOutput, error) {
	if !r.OK {
		return nil, nil
	}
	var out fleetOutput
	if err := json.Unmarshal([]byte(r.OutputJSON), &out); err != nil {
		return nil, err
	}
	if out.ExitCode == nil {
		return nil, errors.New("it carries no exit code")
	}
	return &out, nil
}

// classify is how the step ended, read off the dispatcher's result. Every
// sentence that quotes the machine is masked: the result's text is the
// machine's, and a failed clone or an echoing command can fill it with the
// step's credentials.
func (f *Fleet) classify(ctx context.Context, req pl.StepRequest, run StepRun, r worker.Result, out *fleetOutput, outErr error, mask func(string) string) pl.StepResult {
	where := pl.Where{Surface: surfaceFleet, WorkerID: r.WorkerId, NodeID: r.NodeId, MachineLabels: maps.Clone(r.Labels)}
	machine := r.WorkerId
	if machine == "" {
		machine = "the machine"
	}
	if ctx.Err() != nil {
		return withWhere(stoppedResult(ctx, run.DeadlineCode), where)
	}
	if r.OK {
		if outErr != nil {
			return withWhere(failedResult(pl.CodeExecutorError, fmt.Sprintf(
				"%s answered something that is not a pipeline step's result (%s).", machine, mask(outErr.Error()))), where)
		}
		res := pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: *out.ExitCode, Where: where}
		if *out.ExitCode != 0 {
			// The exit code is the whole answer: no code beside it.
			res.Status = pl.OutcomeFailed
		}
		return res
	}

	msg := mask(strings.TrimSpace(r.ErrorMessage))
	needs := strings.Join(req.Step.Needs, ", ")
	limit := time.Duration(run.TimeoutSeconds) * time.Second
	switch code := r.ErrorCode; {
	case code == "kill_switch_engaged":
		// The owner's off switch, named as such: it says what to turn back on.
		return withWhere(refusedResult(pl.CodeFleetDisabled,
			"The owner has turned computer use off, so no step runs on their machines; turning it back on lets this "+
				"one run. Nothing ran."), where)
	case r.RefusedByGate:
		// THIS ENGINE refused before any routing decision, by its own checks
		// or reads (rulings R33b, R33c): its gate, or a read of the owner's
		// machines the router could not make. A decision about the request or
		// a fault of the engine's, not a fact about the owner's machines, so it
		// is the executor's error, carrying the dispatcher's code and words.
		return withWhere(refusedResult(pl.CodeExecutorError, fmt.Sprintf(
			"The engine's dispatcher refused the step before routing it, on a check or a read of its own (%s): %s. Nothing ran.",
			code, msg)), where)
	case r.RefusedBeforeStart:
		// THE DISPATCHER'S VERDICT, never a guess from the code: nothing
		// started on any machine -- no candidate, or every one refused before
		// anything was sent to it, a sibling replica's refusals under codes of
		// their own included. The machine the dispatcher named, if it named
		// one, is kept: it is where the step was last looked for.
		return withWhere(refusedResult(pl.CodeNoMachineForNeed, fmt.Sprintf(
			"No machine of the owner's that offers %s and allows pipeline steps could take the step (%s): %s",
			needs, code, msg)), where)
	case code == "denied_by_policy":
		return withWhere(failedResult(pl.CodeNoMachineForNeed, fmt.Sprintf(
			"%s refused the step under its own pipelines policy: %s", machine, msg)), where)
	case code == "timeout":
		return withWhere(failedResult(run.DeadlineCode, fmt.Sprintf(
			"The step did not finish within its %s deadline on %s; it was stopped.", limit, machine)), where)
	case code == "cancelled":
		return withWhere(cancelledResult("The step was cancelled on "+machine+"."), where)
	case code == "worker_disconnected":
		// Not refused before start: the call may have started, and the
		// connection ended before the machine reported it.
		return withWhere(failedResult(pl.CodeNodeLost, fmt.Sprintf(
			"The connection to %s ended before it reported the step: %s", machine, msg)), where)
	case code == pl.CodeCloneFailed:
		return withWhere(failedResult(pl.CodeCloneFailed, msg), where)
	default:
		return withWhere(failedResult(pl.CodeExecutorError, fmt.Sprintf(
			"%s could not run the step (%s): %s", machine, code, msg)), where)
	}
}

// artifacts reads the step's artifacts out of the machine's answer, within
// this cluster's cap. tooLarge is an archive past its limits, on the machine
// or here: nothing of it is kept.
func (f *Fleet) artifacts(run StepRun, out *fleetOutput, files *stepFiles) (kept []ArtifactFile, tooLarge bool) {
	if len(run.Artifacts) == 0 || out == nil {
		return nil, false
	}
	if out.ArtifactsTooLarge {
		return nil, true
	}
	var missing []string
	missing = append(missing, out.ArtifactsMissing...)
	if encoded := strings.TrimSpace(out.ArtifactsTgzBase64); encoded != "" {
		// A gzip stream is no larger than what it holds plus framing, so an
		// archive far past the cap compressed is past it uncompressed: refused
		// before it is decoded rather than after.
		if int64(base64.StdEncoding.DecodedLen(len(encoded))) > f.cfg.ArtifactMaxBytes+f.cfg.ArtifactMaxBytes/64+extractStreamSlack {
			return nil, true
		}
		tgz, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			files.note("the machine's artifact archive is not base64, so no artifact was stored: %v", err)
			return nil, false
		}
		var gone []string
		kept, gone, tooLarge = files.extractedArtifacts(tgz, run.Artifacts, f.cfg.ArtifactMaxBytes)
		if tooLarge {
			return nil, true
		}
		missing = append(missing, gone...)
	}
	files.missingNote(run.Artifacts, missing)
	return kept, false
}

// goTimings reads the passing Go packages' times out of the archived log, as
// the cluster path does (goTestTimings); a log it cannot read is a note.
func (f *Fleet) goTimings(archive string) (map[string]float64, *pl.Failure) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, &pl.Failure{Code: pl.CodeTimingsUnreadable, Message: "the step's Go test timings could not be read from its log: " + err.Error()}
	}
	defer file.Close()
	return goTestTimings(file)
}

func (f *Fleet) lineSink() LineSink {
	if f.sink == nil {
		return nil
	}
	return f.sink()
}

// fleetLines assembles the machine's stream chunks into whole lines for the
// capture. Each stream keeps its own partial line, so stdout and stderr chunks
// interleaving mid-line never splice two lines together. After close, chunks
// are dropped: the dispatcher may deliver one after it returned.
type fleetLines struct {
	mu      sync.Mutex
	capture *Capture
	pending map[string][]byte
	closed  bool
}

func (l *fleetLines) chunk(m *nodev1.WorkerForwardStream) {
	var stream string
	var data []byte
	switch p := m.GetPayload().(type) {
	case *nodev1.WorkerForwardStream_StdoutChunk:
		stream, data = "stdout", p.StdoutChunk
	case *nodev1.WorkerForwardStream_StderrChunk:
		stream, data = "stderr", p.StderrChunk
	default:
		// A data chunk is not output.
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	buf := append(l.pending[stream], data...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		l.capture.FeedLine(string(buf[:i]))
		buf = buf[i+1:]
	}
	if len(buf) > fleetMaxPartialLine {
		l.capture.FeedLine(string(buf))
		buf = nil
	}
	// A copy, so a large chunk's backing array is not held for its tail.
	l.pending[stream] = append([]byte(nil), buf...)
}

func (l *fleetLines) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for _, stream := range []string{"stdout", "stderr"} {
		if rest := l.pending[stream]; len(rest) > 0 {
			l.capture.FeedLine(string(rest))
		}
	}
	l.pending = nil
}
