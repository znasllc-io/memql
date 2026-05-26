package safety

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Mode controls whether the Gate enforces the decision policy or
// just observes. Tunable per-deployment via
// MEMQL_COMMAND_CLASSIFIER_MODE; meant to ride through three states
// during rollout (#235):
//
//   - ModeOff: skip classification entirely (zero-cost bypass). Used
//     to fully disable the system if a regression ships.
//   - ModeShadow: classify + record, always return Allow. Used to
//     measure false-positive / false-negative rates against live
//     traffic before flipping enforcement on. **Default.**
//   - ModeEnforce: classify + record + honor the decision policy
//     (allow/ask/deny).
type Mode string

const (
	ModeOff     Mode = "off"
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

// EnvMode is the env var that selects the gate's mode. Read at gate
// construction (NewGate) so changing it requires a restart.
const EnvMode = "MEMQL_COMMAND_CLASSIFIER_MODE"

// ModeFromEnv reads EnvMode and returns the corresponding Mode,
// defaulting to ModeShadow on empty or unrecognized values. Shadow
// is the safe default: it costs a classifier call but cannot block
// live traffic, so an accidentally-enabled rollout is observation-
// only.
func ModeFromEnv() Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvMode))) {
	case "off":
		return ModeOff
	case "enforce":
		return ModeEnforce
	default:
		return ModeShadow
	}
}

// DecisionRecorder receives every Gate verdict. The slog-backed
// default ships in Phase 0; #234 adds a recorder that persists each
// decision as a v1:safety:classification row, and the Gate can be
// reconfigured to fan out to both. Recorder errors MUST NOT block
// the dispatch path -- implementations swallow + log internally.
type DecisionRecorder interface {
	Record(ctx context.Context, desc ActionDescriptor, cls Classification, decision Decision, mode Mode)
}

// SlogRecorder is the default DecisionRecorder: it emits a single
// structured INFO-level log line per gated action. Allows operators
// to grep for classifier decisions in stdout/journald before #234's
// persistence lands.
type SlogRecorder struct {
	Logger *slog.Logger
}

// Record writes the decision to the recorder's logger. Payload is
// passed through RedactedPayload before being logged so no secrets
// land in stdout. A nil logger silently drops the record.
func (r SlogRecorder) Record(ctx context.Context, desc ActionDescriptor, cls Classification, decision Decision, mode Mode) {
	if r.Logger == nil {
		return
	}
	red := RedactedPayload(desc.Payload)
	cats := make([]string, 0, len(cls.Categories))
	for _, c := range cls.Categories {
		cats = append(cats, string(c))
	}
	r.Logger.LogAttrs(ctx, slog.LevelInfo, "safety: command classified",
		slog.String("component", "safety.gate"),
		slog.String("mode", string(mode)),
		slog.String("surface", string(desc.Surface)),
		slog.String("action", string(desc.Action)),
		slog.String("tool", red.ToolName),
		slog.String("command", red.Command),
		slog.String("url", red.URL),
		slog.Any("paths", red.Paths),
		slog.String("agent_id", desc.Caller.AgentID),
		slog.String("owner_user_id", desc.Caller.OwnerUserID),
		slog.String("plan_id", desc.Caller.PlanID),
		slog.String("correlation_id", desc.Caller.CorrelationID),
		slog.String("scope", desc.Caller.Scope),
		slog.String("capability", desc.Caller.Capability),
		slog.String("tier", cls.Tier.String()),
		slog.Any("categories", cats),
		slog.String("source", string(cls.Source)),
		slog.String("rule_id", cls.RuleID),
		slog.Float64("confidence", cls.Confidence),
		slog.Int64("classifier_ms", cls.LatencyMs),
		slog.String("decision", string(decision)),
		slog.String("reason", cls.Reason),
	)
}

// Gate is the single choke point every execution surface (worker /
// tool / workbench) routes through before dispatch. Phase 0 shipped
// the gate; #229 wired the call sites; #231 plugs in the real
// decision policy (the `decide` stub now delegates to a
// DecisionPolicy). Fail-closed semantics for computer-use are
// handled by per-surface `EnforceDecision` calls (see global.go).
type Gate struct {
	classifier     Classifier
	recorder       DecisionRecorder
	mode           Mode
	decisionPolicy DecisionPolicy
}

// GateOptions configures NewGate. All fields are optional; sensible
// defaults are applied: NoopClassifier, SlogRecorder with the
// default logger, ModeFromEnv, DefaultDecisionPolicy.
type GateOptions struct {
	Classifier Classifier
	Recorder   DecisionRecorder
	Mode       Mode
	// DecisionPolicy maps Classification + caller context to an
	// allow/ask/deny Decision. nil -> DefaultDecisionPolicy (the
	// #231 matrix). Override for tests (deterministic verdicts)
	// or to swap in a DSL-backed impl once procedural Policy DSL
	// is exercised.
	DecisionPolicy DecisionPolicy
	// Logger is used only when Recorder is nil -- the default
	// SlogRecorder is built around it.
	Logger *slog.Logger
}

// NewGate constructs a Gate from the options. Nil fields are
// defaulted; the only side effect is reading EnvMode when Mode is
// the zero value (empty string).
func NewGate(opts GateOptions) *Gate {
	classifier := opts.Classifier
	if classifier == nil {
		classifier = NoopClassifier{}
	}
	recorder := opts.Recorder
	if recorder == nil {
		logger := opts.Logger
		if logger == nil {
			logger = slog.Default()
		}
		recorder = SlogRecorder{Logger: logger}
	}
	mode := opts.Mode
	if mode == "" {
		mode = ModeFromEnv()
	}
	decisionPolicy := opts.DecisionPolicy
	if decisionPolicy == nil {
		decisionPolicy = DefaultDecisionPolicy{}
	}
	return &Gate{
		classifier:     classifier,
		recorder:       recorder,
		mode:           mode,
		decisionPolicy: decisionPolicy,
	}
}

// Mode returns the gate's configured mode. Useful for tests + for
// surface adapters that want to short-circuit when the gate is Off.
func (g *Gate) Mode() Mode { return g.mode }

// Evaluate is the entry every surface calls. The returned Decision
// is what the caller should honor; Classification is returned
// alongside so the caller (or the audit path) can include the
// verdict context in its own structured refusal / telemetry.
//
// Semantics by mode:
//
//   - ModeOff: returns (Allow, empty Classification, nil) without
//     calling the classifier. Zero-cost bypass.
//   - ModeShadow: classifies + records, but ALWAYS returns Allow --
//     even if the (future) decision logic would have said Deny. This
//     is what makes a shadow rollout safe: we measure without ever
//     blocking live traffic.
//   - ModeEnforce: classifies + records + honors the decision. The
//     decision logic is the always-allow stub in Phase 0; #231
//     replaces it with the `commandRiskDecision` Policy DSL
//     evaluation.
//
// On classifier error, the recorder still writes the attempt (with
// the error reason captured in the recorded Classification) and
// Evaluate returns the error. Surface adapters MUST decide
// fail-closed vs fail-open per #229 -- the Gate itself stays
// neutral, since the policy hasn't been consulted.
func (g *Gate) Evaluate(ctx context.Context, desc ActionDescriptor) (Decision, Classification, error) {
	if g.mode == ModeOff {
		return DecisionAllow, Classification{
			Source: SourceDisabled,
			Reason: "classifier off (MEMQL_COMMAND_CLASSIFIER_MODE=off)",
		}, nil
	}
	cls, err := g.classifier.Classify(ctx, desc)
	if err != nil {
		// Capture the error reason so the recorder + caller see WHY
		// the verdict was empty. The decision stays Allow in shadow;
		// in enforce, surface adapters apply fail-closed per #229.
		errCls := Classification{
			Source:    cls.Source,
			LatencyMs: cls.LatencyMs,
			Reason:    "classifier error: " + err.Error(),
		}
		g.recorder.Record(ctx, desc, errCls, DecisionAllow, g.mode)
		return DecisionAllow, errCls, err
	}
	decision := g.decide(cls, desc)
	if g.mode == ModeShadow {
		// Record what we WOULD have decided, but always allow.
		g.recorder.Record(ctx, desc, cls, decision, g.mode)
		return DecisionAllow, cls, nil
	}
	g.recorder.Record(ctx, desc, cls, decision, g.mode)
	return decision, cls, nil
}

// decide consults the configured DecisionPolicy. The DefaultDecisionPolicy
// (the #231 matrix) ships with NewGate when no override is supplied.
// Pass desc so the policy can read surface + caller scope/capability --
// computer-use is one notch stricter than sandboxed surfaces, and
// medium-tier on sandboxed surfaces depends on scope.
func (g *Gate) decide(cls Classification, desc ActionDescriptor) Decision {
	return g.decisionPolicy.Decide(DecisionInput{
		Classification: cls,
		Surface:        desc.Surface,
		Scope:          desc.Caller.Scope,
		Capability:     desc.Caller.Capability,
	})
}
