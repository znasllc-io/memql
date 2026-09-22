// Package procedure is the wiring half of epic memql#5402 (design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// section 4 epic C; decisions D6, D13, D14, D24). It backs the two builtins
// declared in dsl/procedure/builtins.memql:
//
//	integration.procedure.learnFromRun -- mine the corpus a finished run belongs to
//	integration.procedure.mineCorpus   -- the scheduled sweep, per owner and goal signature
//
// THE DIVISION OF LABOUR IS THE DESIGN, and it is the same one the work spine
// draws. Every DECISION is a pure function in component/procedure -- what a
// symbol is, what recurs, what a hole means, what an abstraction is worth --
// so the epic's headline claim, that a recorded corpus becomes a parameterized
// construct with no provider call, is a property of values and is provable
// with no engine and no database. This package is responsible only for the
// three things a pure module cannot do: reading rows under the right actor,
// rendering the winner as MemQL, and making the ONE bounded model call D6
// allows -- whose answer it hands straight back to the module to check.
package procedure

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
)

// integrationName is the plug-in name and the middle segment of every
// capability FQN. Spelled as a STRING LITERAL in RegisterPlugin below as well,
// because the module-taxonomy gate finds registrations by scanning source for
// the literal.
const integrationName = "procedure"

// resultConcept is the synthetic concept a capability reply rides on. Never
// persisted.
const resultConcept = "v1:procedure:result"

// Deriver makes the one bounded model call of D6. It is an interface so that
// the pipeline can be driven with NO deriver at all -- which is the ordinary
// case, and the case the zero-provider-call test asserts.
type Deriver interface {
	// ProposeDerivation returns a derivation expression for one unexplained
	// hole, or "" when it has nothing to propose. Whatever it returns is
	// handed straight to component/procedure.CheckDerivation: this seam
	// cannot decide that a proposal is good, only that one was made.
	ProposeDerivation(ctx context.Context, h proc.Hole, instances [][]proc.Action) (string, error)
}

// Integration exposes the procedure capabilities.
type Integration struct {
	store   *store
	logger  *slog.Logger
	now     func() time.Time
	deriver Deriver
	params  proc.Params
}

// New builds the integration.
func New(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		store:  &store{engine: engine},
		logger: logger,
		now:    time.Now,
		params: proc.DefaultParams(),
	}
}

func init() {
	memql.RegisterPlugin("procedure", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.Engine == nil {
			return nil, fmt.Errorf("procedure plug-in: no engine in plugin context")
		}
		return New(pctx.Engine, pctx.Logger), nil
	})
}

// SetDeriver installs the one bounded model call. Called once, from the node
// that holds a router. Absent, every unexplained hole simply stays free --
// which is a correct procedure with one more parameter, never a failure.
func (i *Integration) SetDeriver(d Deriver) {
	if i.deriver == nil {
		i.deriver = d
	}
}

// SetParams overrides the pipeline knobs. The design record's cross-cutting
// rule makes the budget, support, gap and argument ceiling values rather than
// constants; this is where an operator's row reaches them.
func (i *Integration) SetParams(p proc.Params) { i.params = p }

// SetNow injects a clock. Tests only.
func (i *Integration) SetNow(f func() time.Time) {
	if f != nil {
		i.now = f
	}
}

func (i *Integration) clock() time.Time {
	if i == nil || i.now == nil {
		return time.Now()
	}
	return i.now()
}

func (i *Integration) log() *slog.Logger {
	if i == nil || i.logger == nil {
		return slog.Default()
	}
	return i.logger
}

// Capabilities are the two builtins dsl/procedure/builtins.memql declares.
// A capability the DSL names and the registry lacks is a BOOT failure on every
// node type.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "learnFromRun",
			Description: "Mine the corpus a finished run belongs to and, when an abstraction clears D14's floor, " +
				"lift it into a validated v1:authoring:bundle with its goalSignature and provenance. Nothing " +
				"auto-activates: the construct enters the trust ladder as a candidate. Returns " +
				"{runId, goalSignature, candidates, constructId, accepted}.",
			Handler: i.handleLearnFromRun,
			ArgsSchema: map[string]string{
				"runId": "string (required) -- the v1:work:run that just succeeded",
				"level": "integer -- 1 mines the actions inside sessions (default), 2 mines automation invocations",
			},
		},
		{
			Name: "mineCorpus",
			Description: "The scheduled sweep: mine one owner's recorded corpus for one goal signature at one " +
				"level. Disliked recordings are excluded. Returns {ownerUserId, goalSignature, level, " +
				"sequences, candidates, constructId, accepted}.",
			Handler: i.handleMineCorpus,
			ArgsSchema: map[string]string{
				"ownerUserId":   "string (required) -- whose corpus to mine; every read runs as this person",
				"goalSignature": "string -- restrict to one goal's runs; empty mines every signature the owner has",
				"level":         "integer -- 1 actions (default), 2 automation invocations",
			},
		},
	}
}

// IntegrationName is the plug-in name.
func (i *Integration) IntegrationName() string { return integrationName }
