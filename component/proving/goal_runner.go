package proving

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/proving/goaleval"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// GoalClient is the ordinary authenticated work API. The evaluator has no
// provider client, solver, patch writer, or bypass of the worker's permissions.
type GoalClient interface {
	ActiveEmbedderBinding(context.Context, client.ActiveEmbedderBindingArgs) (*client.Result, error)
	CreateGoal(context.Context, client.CreateGoalArgs) (*client.Result, error)
	CancelGoal(context.Context, client.CancelGoalArgs) (*client.Result, error)
	WorkRunForOwner(context.Context, client.WorkRunForOwnerArgs) (*client.Result, error)
	WorkModelCallsForOwnerRun(context.Context, client.WorkModelCallsForOwnerRunArgs) (*client.Result, error)
	WorkObservationsForOwnerRun(context.Context, client.WorkObservationsForOwnerRunArgs) (*client.Result, error)
	WorkStepsForOwnerRun(context.Context, client.WorkStepsForOwnerRunArgs) (*client.Result, error)
	WorkRecordGoalEvaluation(context.Context, client.WorkRecordGoalEvaluationArgs) (*client.Result, error)
}

type GoalRunner struct {
	Client        GoalClient
	Files         fs.FS
	CatalogDir    string
	OutputDir     string
	EngineCommit  string
	HarnessCommit string
	PollInterval  time.Duration
}

type GoalAttempt struct {
	ScenarioID string           `json:"scenarioId"`
	Setup      string           `json:"setup"`
	Repetition int              `json:"repetition"`
	State      string           `json:"state"`
	Reason     string           `json:"reason,omitempty"`
	Result     *goaleval.Result `json:"result,omitempty"`
}

type GoalReport struct {
	SchemaVersion int             `json:"schemaVersion"`
	Matrix        goaleval.Matrix `json:"matrix"`
	StartedAt     time.Time       `json:"startedAt"`
	FinishedAt    time.Time       `json:"finishedAt"`
	Attempts      []GoalAttempt   `json:"attempts"`
}

// Run executes the selected matrix sequentially. Missing injection authority is
// recorded as not-run; it is never counted as a failed or successful model run.
func (g *GoalRunner) Run(ctx context.Context, catalog goaleval.Catalog, matrix goaleval.Matrix) (GoalReport, error) {
	report := GoalReport{SchemaVersion: 1, Matrix: matrix, StartedAt: time.Now().UTC()}
	if err := catalog.Validate(); err != nil {
		return report, err
	}
	if err := matrix.Validate(catalog); err != nil {
		return report, err
	}
	if g.Client == nil || g.Files == nil {
		return report, fmt.Errorf("goal evaluator needs an authenticated work client and fixture source")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(g.EngineCommit) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(g.HarnessCommit) {
		return report, fmt.Errorf("exact engine and harness commits are required")
	}
	if err := os.MkdirAll(g.OutputDir, 0700); err != nil {
		return report, err
	}
	if _, err := os.Stat(filepath.Join(g.OutputDir, "report.json")); !os.IsNotExist(err) {
		return report, fmt.Errorf("refusing to replace an existing evaluation report")
	}
	for _, setup := range matrix.Setups {
		for _, id := range matrix.ScenarioIDs {
			var scenario goaleval.Scenario
			for _, s := range catalog.Scenarios {
				if s.ID == id {
					scenario = s
				}
			}
			for repetition := 1; repetition <= matrix.Repetitions; repetition++ {
				if err := ctx.Err(); err != nil {
					return report, err
				}
				a, err := g.runCase(ctx, scenario, setup, matrix)
				a.Repetition = repetition
				report.Attempts = append(report.Attempts, a)
				report.FinishedAt = time.Now().UTC()
				if saveErr := writeGoalJSON(filepath.Join(g.OutputDir, "report.json"), report); saveErr != nil {
					return report, saveErr
				}
				if err != nil {
					return report, err
				}
			}
		}
	}
	return report, nil
}

func (g *GoalRunner) runCase(ctx context.Context, s goaleval.Scenario, setup goaleval.SetupRequest, matrix goaleval.Matrix) (attempt GoalAttempt, err error) {
	attempt = GoalAttempt{ScenarioID: s.ID, Setup: setup.Name, State: "not-run", Reason: goaleval.Availability(s)}
	var workspace *goaleval.Workspace
	var result goaleval.Result
	var run map[string]any
	var calls, observations []client.Row
	var submitted, observed, verified bool
	ctx, cancel := context.WithTimeout(ctx, time.Duration(matrix.WallClockMinutes)*time.Minute)
	defer cancel()
	// If observing fails, request cancellation and preserve the known identities.
	// An uncertain submit is never retried: it may already have opened a goal.
	defer func() {
		if err != nil && !submitted && workspace != nil {
			_ = writeGoalJSON(filepath.Join(workspace.Root, "submission-error.json"), map[string]any{"error": err.Error(), "uncertainOutcome": true})
		}
		if err != nil && submitted {
			stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer stop()
			_, cancelErr := g.Client.CancelGoal(stopCtx, client.CancelGoalArgs{GoalId: result.GoalID, Reason: "Evaluation observer stopped: " + err.Error()})
			_ = writeGoalJSON(filepath.Join(workspace.Root, "observer-error.json"), map[string]any{"error": err.Error(), "cancelError": fmt.Sprint(cancelErr), "goalId": result.GoalID, "runId": result.RunID})
		}
	}()
	_, err = workflowhost.Run(ctx, "runGoalEvaluation", nil, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"goalEvaluationPrepare": func(ctx context.Context, _ map[string]any) (any, error) {
			if attempt.Reason != "" {
				return map[string]any{"available": false}, nil
			}
			if workspace != nil {
				return nil, fmt.Errorf("fixture already prepared")
			}
			binding, err := g.Client.ActiveEmbedderBinding(ctx, client.ActiveEmbedderBindingArgs{})
			if err != nil {
				return nil, fmt.Errorf("read active embedding route before admitting evaluation: %w", err)
			}
			bindingRows := binding.Rows()
			if len(bindingRows) != 1 || !strings.HasPrefix(client.RowString(bindingRows[0], "providerRef"), "fleet:") {
				attempt.Reason = "a fleet embedding binding is required, including for asynchronous indexing; no configuration was changed"
				return map[string]any{"available": false}, nil
			}
			files, err := goaleval.ReadFixture(g.Files, g.CatalogDir, s.Fixture)
			if err != nil {
				return nil, err
			}
			workspace, err = goaleval.Prepare(ctx, g.OutputDir, s, files)
			if err != nil {
				return nil, err
			}
			result = goaleval.Result{SchemaVersion: 1, ID: filepath.Base(workspace.Root), ScenarioID: s.ID, ScenarioVersion: s.Version, ScenarioFingerprint: s.Fingerprint(), FixtureVersion: s.Fixture.Version, EngineCommit: g.EngineCommit, HarnessCommit: g.HarnessCommit, StartedAt: time.Now().UTC(), Outcome: "failed"}
			result.Setup = goalSetup(setup)
			embedder := bindingRows[0]
			result.Setup.Routes = append(result.Setup.Routes, goaleval.Route{Role: "retrieval and indexing", Level: "embeddings", Rule: "active embedder binding", Doors: []goaleval.Door{{Source: "fleet", Model: strings.TrimPrefix(client.RowString(embedder, "providerRef"), "fleet:"), Configuration: embedder}}})
			result.Setup.RoutingDigest = result.Setup.Fingerprint()
			result.InputTokens = unknownUsage("aggregate work journal does not establish complete provider input usage")
			result.OutputTokens = unknownUsage("aggregate work journal does not establish complete provider output usage")
			result.CostUSD = unknownUsage("no provider billing receipt; exact fleet pin forbids paid fallback")
			if err := writeGoalJSON(filepath.Join(workspace.Root, "fixture.json"), map[string]any{"scenario": s, "workspace": workspace, "setup": result.Setup}); err != nil {
				return nil, err
			}
			return map[string]any{"available": true}, nil
		},
		"goalEvaluationSubmit": func(ctx context.Context, _ map[string]any) (any, error) {
			if workspace == nil || submitted {
				return nil, fmt.Errorf("submit needs a fresh prepared fixture")
			}
			statement := s.Goal + "\n\nEvaluation checkout: " + workspace.Checkout + "\n" + strings.Join(s.Constraints, "\n") + "\nUse only this checkout. Its NOTES.txt contains uncommitted work; operator-untracked.txt must remain untracked. Do not change go.mod or the evaluation marker. Do not call another model or delegate to an external app. Record commands and their outcomes. Preserve a Go test as the reproduction or regression artifact. For the recorded package check, use workerHost exec with cmd=go test -count=1 ./... and cwd set to this exact checkout."
			args := client.CreateGoalArgs{NoPaidProviders: true, Statement: statement, Source: setup.ReasoningModel, Level: "reasoning", RequestedVia: "api", Input: map[string]any{"workspace": workspace.Checkout}, Ceilings: map[string]any{"maxModelCalls": matrix.MaxModelCalls, "wallClockMs": matrix.WallClockMinutes * 60000, "maxRetries": 2}}
			if err := writeGoalJSON(filepath.Join(workspace.Root, "submission.json"), args); err != nil {
				return nil, err
			}
			reply, err := g.Client.CreateGoal(ctx, args)
			if err != nil {
				return nil, fmt.Errorf("goal submission uncertain; inspect submission.json before retrying: %w", err)
			}
			rows := reply.Rows()
			if len(rows) != 1 {
				return nil, fmt.Errorf("goal intake returned no unique receipt")
			}
			result.GoalID, result.RunID = client.RowString(rows[0], "goalId"), client.RowString(rows[0], "runId")
			if result.GoalID == "" || result.RunID == "" {
				return nil, fmt.Errorf("goal intake omitted goal/run provenance")
			}
			submitted = true
			return rows[0], writeGoalJSON(filepath.Join(workspace.Root, "intake.json"), rows[0])
		},
		"goalEvaluationObserve": func(ctx context.Context, _ map[string]any) (any, error) {
			if !submitted || observed {
				return nil, fmt.Errorf("observe needs one submitted goal")
			}
			interval := g.PollInterval
			if interval <= 0 {
				interval = 5 * time.Second
			}
			for {
				reply, err := g.Client.WorkRunForOwner(ctx, client.WorkRunForOwnerArgs{RunId: result.RunID})
				if err != nil {
					return nil, err
				}
				rows := reply.Rows()
				if len(rows) != 1 {
					return nil, fmt.Errorf("owned evaluation run is unavailable")
				}
				run = rows[0]
				routing, _ := run["routing"].(map[string]any)
				if client.RowString(routing, "source") != setup.ReasoningModel || routing["noPaidProviders"] != true {
					return nil, fmt.Errorf("persisted run does not carry the exact no-fallback fleet pin")
				}
				status := client.RowString(run, "status")
				if status == "waiting" && result.Outcome != "blocked" {
					result.Outcome = "blocked"
					if _, err := g.Client.CancelGoal(ctx, client.CancelGoalArgs{GoalId: result.GoalID, Reason: "Evaluation parked for external input; preserve blocker evidence"}); err != nil {
						return nil, err
					}
					// cancelGoal requests cancellation. Keep observing until the run
					// stops before taking a filesystem snapshot or publishing it.
				}
				if status == "succeeded" || status == "failed" || status == "cancelled" || status == "abandoned" {
					break
				}
				timer := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
			}
			if err := writeGoalJSON(filepath.Join(workspace.Root, "run.json"), run); err != nil {
				return nil, err
			}
			evidence, err := goaleval.CollectRunEvidence(ctx, result.RunID, setup.ReasoningModel, g.readEvidence)
			if err != nil {
				return nil, err
			}
			calls, observations = evidence.ModelCalls, evidence.Observations
			if err := writeGoalJSON(filepath.Join(workspace.Root, "receipts.json"), evidence); err != nil {
				return nil, err
			}
			result.ModelsUsed, result.Bindings, result.Interventions = goalReceiptEvidence(calls, observations, result.StartedAt)
			result.Artifacts = []string{filepath.Join(workspace.Root, "run.json"), filepath.Join(workspace.Root, "receipts.json"), filepath.Join(workspace.Root, "fixture.json")}
			observed = true
			return run, nil
		},
		"goalEvaluationVerify": func(ctx context.Context, _ map[string]any) (any, error) {
			if !observed || verified {
				return nil, fmt.Errorf("verify needs one observed terminal or parked run")
			}
			var err error
			result.Assertions, err = workspace.Verify(ctx, s)
			if err != nil {
				return nil, err
			}
			if err := workspace.ConfirmRecordedChecks(result.Assertions, observations); err != nil {
				return nil, err
			}
			verified = true
			if client.RowString(run, "status") == "succeeded" {
				result.Outcome = "autonomous"
				if len(result.Interventions) > 0 {
					result.Outcome = "assisted"
				}
				for _, a := range result.Assertions {
					if a.Verdict != "pass" {
						result.Outcome = "failed"
					}
				}
				if len(result.ModelsUsed) == 0 {
					result.Outcome = "failed"
				}
				for _, m := range result.ModelsUsed {
					if m.Source != "fleet" {
						result.Outcome = "failed"
					}
				}
			}
			result.FinishedAt = time.Now().UTC()
			return result.Assertions, nil
		},
		"goalEvaluationRecord": func(ctx context.Context, _ map[string]any) (any, error) {
			if attempt.Reason != "" {
				return attempt, nil
			}
			if !verified {
				return nil, fmt.Errorf("cannot publish an unverified attempt")
			}
			if err := result.Validate(s); err != nil {
				return nil, err
			}
			if err := writeGoalJSON(filepath.Join(workspace.Root, "result.json"), result); err != nil {
				return nil, err
			}
			attempt.State, attempt.Result = "measured", &result
			resultMap, err := goalObject(result)
			if err != nil {
				return nil, err
			}
			scenarioMap, err := goalObject(s)
			if err != nil {
				return nil, err
			}
			published, err := g.Client.WorkRecordGoalEvaluation(ctx, client.WorkRecordGoalEvaluationArgs{Scenario: scenarioMap, Result: resultMap})
			if err != nil {
				return nil, fmt.Errorf("local evidence saved; publishing owned evaluation receipt: %w", err)
			}
			if len(published.Rows()) != 1 || client.RowString(published.Rows()[0], "receiptId") == "" {
				return nil, fmt.Errorf("publication returned no receipt")
			}
			if err := writeGoalJSON(filepath.Join(workspace.Root, "publication.json"), published.Rows()[0]); err != nil {
				return nil, err
			}
			return attempt, nil
		},
	}})
	if err != nil {
		attempt.Reason = err.Error()
		if submitted {
			attempt.State = "observer-error"
		}
	}
	return attempt, err
}

func goalObject(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}

func goalSetup(s goaleval.SetupRequest) goaleval.Setup {
	setup := goaleval.Setup{Name: s.Name}
	for _, role := range []struct{ role, level string }{{"planner/triage", "fast"}, {"planner/compile", "strong"}, {"work steps", "reasoning"}, {"answer validator", "reasoning"}} {
		setup.Routes = append(setup.Routes, goaleval.Route{Role: role.role, Level: role.level, Rule: "owner source pin", Doors: []goaleval.Door{{Source: "fleet", Model: strings.TrimPrefix(s.ReasoningModel, "fleet:"), Configuration: map[string]any{"source": s.ReasoningModel, "fallback": false, "noPaidProviders": true, "settings": "recorded per model call"}}}})
	}
	setup.RoutingDigest = setup.Fingerprint()
	return setup
}

func unknownUsage(reason string) goaleval.Measure {
	return goaleval.Measure{Basis: "unavailable", Source: reason}
}

func goalReceiptEvidence(calls, observations []client.Row, started time.Time) (uses []goaleval.ModelUse, bindings []goaleval.Binding, interventions []goaleval.Intervention) {
	for _, call := range calls {
		if client.RowString(call, "served") == "journal" {
			continue
		}
		decision, _ := call["decision"].(map[string]any)
		level := client.RowString(decision, "requestedLevel")
		if level == "" {
			level = client.RowString(decision, "level")
		}
		if level == "" {
			level = "unreported"
		}
		role := client.RowString(call, "promptRef")
		if role == "" {
			role = "step:" + client.RowString(call, "stepKey")
		}
		source := "federation"
		provider := client.RowString(call, "provider")
		if strings.HasPrefix(provider, "fleet:") {
			source = "fleet"
		} else if strings.HasPrefix(provider, "app:") {
			source = "app"
		}
		model := client.RowString(call, "model")
		if model == "" {
			model = provider
		}
		uses = append(uses, goaleval.ModelUse{Role: role, Level: level, Source: source, Model: model, Calls: 1, Evidence: []string{"modelCall:" + client.RowString(call, "id")}, InputTokens: unknownUsage("see raw journal; usage attribution is not a complete provider total"), OutputTokens: unknownUsage("see raw journal; usage attribution is not a complete provider total"), CostUSD: unknownUsage("no provider billing receipt")})
	}
	for _, o := range observations {
		data, _ := o["data"].(map[string]any)
		if selection, ok := data["skillSelection"].(map[string]any); ok {
			if selected, ok := selection["selected"].([]any); ok {
				for _, value := range selected {
					if item, ok := value.(map[string]any); ok {
						bindings = append(bindings, goaleval.Binding{ID: client.RowString(item, "id"), Version: fmt.Sprint(item["version"]), Digest: client.RowString(item, "digest"), Source: "observation:" + client.RowString(o, "id")})
					}
				}
			}
		}
		if client.RowString(o, "kind") == "feedback" {
			at, _ := time.Parse(time.RFC3339Nano, client.RowString(o, "createdAt"))
			if at.Before(started) {
				at = started
			}
			interventions = append(interventions, goaleval.Intervention{At: at, Kind: "feedback", Description: "Recorded human feedback; see owned observation", Evidence: "observation:" + client.RowString(o, "id")})
		}
	}
	return
}

func writeGoalJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

func (g *GoalRunner) readEvidence(ctx context.Context, kind, runID string) ([]map[string]any, error) {
	var r *client.Result
	var err error
	switch kind {
	case "run":
		r, err = g.Client.WorkRunForOwner(ctx, client.WorkRunForOwnerArgs{RunId: runID})
	case "modelCalls":
		r, err = g.Client.WorkModelCallsForOwnerRun(ctx, client.WorkModelCallsForOwnerRunArgs{RunId: runID})
	case "observations":
		r, err = g.Client.WorkObservationsForOwnerRun(ctx, client.WorkObservationsForOwnerRunArgs{RunId: runID})
	case "steps":
		r, err = g.Client.WorkStepsForOwnerRun(ctx, client.WorkStepsForOwnerRunArgs{RunId: runID})
	default:
		return nil, fmt.Errorf("unknown evaluation evidence kind")
	}
	if err != nil {
		return nil, err
	}
	return r.Rows(), nil
}
