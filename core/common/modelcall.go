package common

// modelcall.go -- what a model request IS, for the two questions the work
// spine asks about one: which run made it, and is this the same request as one
// already journaled (memql#4999).
//
// WHY HERE. Both answers are functions of the types declared beside them:
// ChatMessage, ToolDefinition and StructuredSchema. Putting them in the
// provider package that already carries ContextWithToolDefaults keeps the hash
// next to what it hashes, and lets the engine's model seam and the automations
// executor share them without either importing the other.
//
// The DECISION taken with these values -- serve from the journal, call live,
// or diverge -- is deliberately NOT here. That is component/work.DecideServe,
// a pure decision over a pure input, and it stays there.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Run modes. A run's mode decides what the journal is allowed to serve.
const (
	RunModeLive   = "live"
	RunModeReplay = "replay"
	RunModeFork   = "fork"
)

// Replay policies for a run in replay mode.
const (
	ReplayStrict     = "strict"
	ReplayPermissive = "permissive"
)

// RunContext is the work run a model call belongs to, as it travels on the Go
// context.
//
// IT RIDES THE CONTEXT because the call site knows nothing about runs. A DSL
// `ai(...)` inside an automation step reaches the engine through the step
// registry, the shape evaluator and the prompt renderer, none of which have any
// business carrying a run id -- and threading one through all three would put
// the work spine's vocabulary in every signature it passes. The precedent is
// ContextWithBudgetScope, which the LLM guard reads off the outbound HTTP
// request's context for exactly this reason.
//
// A ZERO VALUE MEANS "not part of a run", which is most calls in the product: a
// chat turn, a suggest, a safety classification. Those are never journaled and
// never served from a journal, and that is not a gap -- a journal entry only
// means anything relative to a run that can be replayed.
type RunContext struct {
	// RunId is v1:work:run.id. Empty means there is no run, whatever else is
	// set.
	RunId string
	// GoalId is the run's goal. It is carried because the cross-goal rule
	// outranks the mode: a journal entry from another goal is never served,
	// in any mode.
	GoalId string
	// StepKey is the step making the call, and it is what a divergence is
	// pinned to.
	StepKey string
	// Mode is RunModeLive / RunModeReplay / RunModeFork.
	Mode string
	// ReplayPolicy is ReplayStrict (the default) or ReplayPermissive.
	ReplayPolicy string
	// SourceRunId is the run being replayed or forked FROM -- the run whose
	// journal is read. Empty in live mode.
	SourceRunId string
	// SourceGoalId is that run's goal, which is what SameGoal compares
	// against.
	SourceGoalId string
	// ForkAtStepKey is the step a fork diverges at; steps before it are
	// served from the journal, steps at or after it run live.
	ForkAtStepKey string
	// StepOrder is the source run's step keys in execution order, which is
	// the only way to answer "is this step before the fork point".
	StepOrder []string
	// OwnerUserId owns the run, and therefore owns every journal row written
	// for it.
	OwnerUserId string
	// Override is what a person changed for the ONE step a re-run or a
	// branch targets (epic memql#5414, design D20). The executor sets it on
	// that step's context only, so it can never leak into the next step;
	// every other step's context carries nil.
	Override *StepOverride
	// Workspace is a fresh workspace for this execution's app sessions, set
	// by a re-run or a branch of a session step so the new session does not
	// run inside the directory the previous version's later steps changed.
	// Empty means the default workspace.
	Workspace string
	// Snapshot is the workspace a session step's new session is started
	// against -- the content-addressed files the recording held before that
	// step (design D19). Set only on the targeted step, and only when it is
	// a session step.
	Snapshot *WorkspaceSnapshot
	// Routing is the person's routing choice for EVERY model call of this
	// run (route_choice.go): read off the run row by whichever node builds
	// this context, never carried in memory from the node that took the
	// request. The zero value routes by the rules.
	Routing RouteChoice
}

// StepOverride is a person's change to one version of one step (epic
// memql#5414, design D20). Declared here rather than in component/work so the
// model seam, which cannot import the work module, can read it off the
// context; component/work.Override is the validated form it is built from.
type StepOverride struct {
	// Level is the level to request instead of the step's own: fast, strong
	// or reasoning. Empty keeps the step's.
	Level string
	// Model is a policy entry to pin instead of routing by level -- a
	// provider name, fleet:<modelId>, app:<id> or app:<id>:<model> -- walked
	// as a one-entry chain. Empty routes by level.
	Model string
	// Effort is the effort to ask an app to spend: low, medium, high, xhigh
	// or max. Empty lets the level decide.
	Effort string
	// Prompt is, when WholePrompt is set, the WHOLE prompt the new session
	// runs with; otherwise instructions added to the step's own prompt.
	Prompt string
	// WholePrompt says Prompt replaces an app session's prompt: the replaced
	// version was answered by a session, and the person edited the prompt it
	// ran with. It is decided by the act from that version, never by where
	// the new version happens to be served.
	WholePrompt bool
	// Inputs replace the step's own arguments by name.
	Inputs map[string]any
	// GuidanceAxes and GuidanceReason are what the person disliked about the
	// version this one replaces (design D23, repair): the axes by name
	// (product, process, performance) and their reason.
	GuidanceAxes   []string
	GuidanceReason string
	// FeedbackId is the feedback observation the guidance came from.
	FeedbackId string
	// RequestedBy is the person who asked for this version.
	RequestedBy string
}

// Empty reports whether the override changes nothing about the call.
func (o *StepOverride) Empty() bool {
	return o == nil || (o.Level == "" && o.Model == "" && o.Effort == "" && o.Prompt == "" &&
		len(o.Inputs) == 0 && o.GuidanceReason == "" && len(o.GuidanceAxes) == 0)
}

// WorkspaceSnapshot is the set of content-addressed files a session step's
// workspace held before that step, rebuilt from its run's recordings (epic
// memql#5414, design D19).
type WorkspaceSnapshot struct {
	Files []SnapshotFile
}

// SnapshotFile is one file of a WorkspaceSnapshot: its workspace-relative
// path and the v1:library:file holding its bytes.
type SnapshotFile struct {
	Path   string
	FileId string
}

// IsRun reports whether this context names a run at all.
func (rc RunContext) IsRun() bool { return strings.TrimSpace(rc.RunId) != "" }

// BeforeForkPoint reports whether stepKey falls before ForkAtStepKey in the
// source run's step order.
//
// A step key that is not in the order at all is NOT before the fork point: a
// step the source run never executed cannot have a journaled answer, and
// guessing "before" would serve it something recorded for a different step.
func (rc RunContext) BeforeForkPoint(stepKey string) bool {
	fork := strings.TrimSpace(rc.ForkAtStepKey)
	if fork == "" {
		return false
	}
	forkAt, stepAt := -1, -1
	for i, k := range rc.StepOrder {
		if k == fork && forkAt < 0 {
			forkAt = i
		}
		if k == stepKey && stepAt < 0 {
			stepAt = i
		}
	}
	if forkAt < 0 || stepAt < 0 {
		return false
	}
	return stepAt < forkAt
}

type runCtxKey struct{}

// ContextWithRun stamps the run a model call belongs to.
//
// A RunContext naming no run returns ctx UNCHANGED, so a caller may stamp
// unconditionally without turning "no run" into "a run with a blank id" --
// the same shape ContextWithBudgetScope uses for an empty scope list.
func ContextWithRun(ctx context.Context, rc RunContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !rc.IsRun() {
		return ctx
	}
	return context.WithValue(ctx, runCtxKey{}, rc)
}

// RunFromContext returns the run this call belongs to, if any.
func RunFromContext(ctx context.Context) (RunContext, bool) {
	if ctx == nil {
		return RunContext{}, false
	}
	rc, ok := ctx.Value(runCtxKey{}).(RunContext)
	if !ok || !rc.IsRun() {
		return RunContext{}, false
	}
	return rc, true
}

// ModelRequest is everything about a model request that decides whether a
// journaled answer is the SAME answer.
//
// WHAT IS IN IT IS THE CONTRACT. Provider, model, settings, messages, tools and
// output schema: change any one of them and the recorded response is an answer
// to a different question, so a replay must not serve it. What is deliberately
// OUT: the run id, the step key, timestamps, token counts and cost -- those
// describe the CALL, not the REQUEST, and including any of them would make
// every hash unique and every replay a miss.
type ModelRequest struct {
	Provider string
	Model    string
	Settings map[string]any
	Messages []ChatMessage
	Images   []VisionContent
	Tools    []ToolDefinition
	Schema   StructuredSchema
}

// Hash is the replay key: a stable digest of the request.
//
// STABLE ACROSS PROCESSES AND RUNS, which rules out fmt of a struct (map order
// is random and Go does not promise the format), and rules out hashing a
// json.Marshal of the whole thing (a map still marshals in sorted key order,
// but a struct's field order is the struct's, and any field added later would
// silently invalidate every journal ever written). Instead each part is written
// in a fixed order with an explicit separator, and maps are walked in sorted
// key order.
//
// The separator matters. Without one, {Provider: "ab", Model: "c"} and
// {Provider: "a", Model: "bc"} hash the same, and a journal that confuses two
// providers is worse than no journal.
func (r ModelRequest) Hash() string {
	h := sha256.New()
	write := func(part, value string) {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", part, len(value), value)
	}

	write("provider", strings.TrimSpace(r.Provider))
	write("model", strings.TrimSpace(r.Model))

	keys := make([]string, 0, len(r.Settings))
	for k := range r.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// A setting whose value cannot be encoded is written as its Go
		// rendering rather than skipped: skipping would make two different
		// requests hash alike, which is the one outcome this must never have.
		b, err := json.Marshal(r.Settings[k])
		if err != nil {
			b = []byte(fmt.Sprintf("%q", fmt.Sprint(r.Settings[k])))
		}
		write("setting:"+k, string(b))
	}

	for i, m := range r.Messages {
		write(fmt.Sprintf("message:%d:role", i), m.Role)
		write(fmt.Sprintf("message:%d:name", i), m.Name)
		write(fmt.Sprintf("message:%d:content", i), m.Content)
		write(fmt.Sprintf("message:%d:toolCallId", i), m.ToolCallId)
		for j, tc := range m.ToolCalls {
			write(fmt.Sprintf("message:%d:call:%d:id", i, j), tc.ID)
			write(fmt.Sprintf("message:%d:call:%d:name", i, j), tc.Name)
			write(fmt.Sprintf("message:%d:call:%d:args", i, j), tc.Arguments)
		}
	}

	// Tools are sorted by name: the set a model is offered is a set, and the
	// order a caller happens to build it in is not part of the request.
	for n, image := range r.Images {
		write(fmt.Sprintf("image:%d:mime", n), image.MimeType)
		write(fmt.Sprintf("image:%d:bytes", n), string(image.Data))
	}
	tools := make([]ToolDefinition, len(r.Tools))
	copy(tools, r.Tools)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for i, t := range tools {
		write(fmt.Sprintf("tool:%d:name", i), t.Name)
		write(fmt.Sprintf("tool:%d:description", i), t.Description)
		b, err := json.Marshal(t.InputSchema)
		if err != nil {
			b = []byte(fmt.Sprint(t.InputSchema))
		}
		write(fmt.Sprintf("tool:%d:schema", i), string(b))
	}

	write("schema:name", r.Schema.Name)
	write("schema:description", r.Schema.Description)
	write("schema:body", string(r.Schema.Schema))
	if r.Schema.Strict {
		write("schema:strict", "true")
	} else {
		write("schema:strict", "false")
	}

	return hex.EncodeToString(h.Sum(nil))
}
