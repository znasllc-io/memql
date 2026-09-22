package procedure

import (
	"context"
	"sort"
	"strconv"
	"strings"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/core/num"
)

// corpus.go -- rows to values, and the only half of this epic that reads a
// row. Everything below it is a decision over values in component/procedure.

// Level names which of D24's two corpus levels to mine. The pipeline below is
// identical for both, which is why the level is a PARAMETER rather than two
// functions: a second copy is a copy that drifts, and the record's claim that
// "the one pipeline lifts repeated automation sequences into higher
// automations by the same compression rule" is only true if it is literally
// the same pipeline.
type Level int

const (
	// LevelAction mines the actions inside recorded sessions.
	LevelAction Level = 1
	// LevelAutomation mines automation invocations -- subrun steps -- so a
	// repeated sequence of automations becomes a higher automation.
	LevelAutomation Level = 2
)

// appActionTypes are the step types an app session records (epic memql#5396,
// design D2 and D12).
var appActionTypes = map[string]bool{
	"exec":       true,
	"fs_write":   true,
	"fs_read":    true,
	"fetch":      true,
	"mcp":        true,
	"app_answer": true,
}

// pureReadTypes duplicates component/procedure's set for ONE purpose: deciding
// which steps need a Consumed verdict. The module drops an unconsumed pure
// read; only this package can compute Consumed, because it needs the whole
// run.
var pureReadTypes = map[string]bool{"fs_read": true, "fetch": true, "mcp": true}

// corpusKey names one mining job.
type corpusKey struct {
	OwnerUserId   string
	GoalSignature string
	Level         Level
}

// loadCorpus reads one owner's recorded runs for one goal signature and
// returns them as sequences of pure steps.
//
// Four rules, each of which fails silently if it is missed.
//
// THE READ RUNS UNDER THE OWNER'S ACTOR, never a cluster-owner variant. The
// composite tier would happily serve every owner's runs to a sweep, and the
// template mined from two people's recordings is correct about neither.
//
// A DISLIKED RECORDING CONTRIBUTES NOTHING (D23). Exclusion happens here, in
// the loader, so that nothing downstream can forget it; a filter applied later
// is one a new caller skips.
//
// A RUN WITH NO goalSignature IS SKIPPED, not defaulted. Grouping unsignatured
// runs together mines across unrelated goals, and the procedure that comes out
// is correct about nothing.
//
// Consumed IS COMPUTED HERE. Whether a later step referenced an earlier
// result is a property of the whole run, which the module cannot see.
func (i *Integration) loadCorpus(ctx context.Context, k corpusKey) ([][]proc.Step, []string, error) {
	owner := strings.TrimSpace(k.OwnerUserId)
	if owner == "" {
		return nil, nil, nil
	}
	actorCtx := ownerActor(ctx, owner)

	runs, err := i.store.query(actorCtx, "query "+call("workRunsForOwner", map[string]any{"limit": corpusRunLimit}))
	if err != nil {
		return nil, nil, err
	}

	var (
		out    [][]proc.Step
		runIds []string
	)
	for _, run := range runs {
		runId := str(run, "id")
		if runId == "" {
			continue
		}
		sig := str(run, "goalSignature")
		if sig == "" {
			// Nobody measured this run's goal. Mining it beside a signed one
			// would put two unrelated goals in one template.
			continue
		}
		if k.GoalSignature != "" && sig != k.GoalSignature {
			continue
		}
		if str(run, "status") != "succeeded" {
			continue
		}
		disliked, err := i.runIsDisliked(actorCtx, runId)
		if err != nil {
			return nil, nil, err
		}
		if disliked {
			continue
		}
		steps, err := i.loadSteps(actorCtx, runId, sig, k.Level)
		if err != nil {
			return nil, nil, err
		}
		if len(steps) > 0 {
			out = append(out, steps)
			runIds = append(runIds, runId)
		}
	}
	return out, runIds, nil
}

// corpusRunLimit bounds one mining pass. It is a VALUE with a stated default
// rather than an unbounded read: a sweep that reads every run an owner has
// ever had grows without limit, and the compression rule needs a recent
// corpus rather than an exhaustive one.
const corpusRunLimit = 200

// loadSteps reads one run's steps and lowers them, keeping only the level's
// vocabulary.
func (i *Integration) loadSteps(ctx context.Context, runId, goalSignature string, level Level) ([]proc.Step, error) {
	rows, err := i.store.query(ctx, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(a, b int) bool { return intOf(rows[a], "seq") < intOf(rows[b], "seq") })

	steps := make([]proc.Step, 0, len(rows))
	for _, r := range rows {
		stepType := str(r, "stepType")
		if !levelAccepts(level, stepType) {
			continue
		}
		result := obj(r, "result")
		steps = append(steps, proc.Step{
			RunId:        runId,
			Key:          str(r, "key"),
			Seq:          intOf(r, "seq"),
			StepType:     stepType,
			Call:         proc.Call{Construct: str(obj(r, "call"), "construct"), Name: str(obj(r, "call"), "name")},
			Input:        obj(r, "input"),
			ResultDigest: str(r, "resultFingerprint"),
			EffectDigest: footprintDigest(obj(r, "actualFootprint")),
			ResultValue:  resultValue(result),

			GoalSignature: goalSignature,
		})
	}
	markConsumed(steps)
	return steps, nil
}

func levelAccepts(level Level, stepType string) bool {
	switch level {
	case LevelAutomation:
		return stepType == "automation"
	default:
		return appActionTypes[stepType]
	}
}

// markConsumed sets Consumed on every step whose result a LATER step's
// arguments referenced. It is a whole-run question, which is why it cannot
// live in the pure module.
//
// The comparison is over the flattened leaf VALUES of the result against the
// flattened leaf values of every later step's input. A digest-only result --
// which is what a large result degrades to -- is compared as its digest, so a
// step whose result was too big to keep is still recognised as consumed when a
// later argument carries that digest.
func markConsumed(steps []proc.Step) {
	for i := range steps {
		if !pureReadTypes[steps[i].StepType] {
			continue
		}
		values := leafValues(steps[i].ResultValue)
		if steps[i].ResultDigest != "" {
			values = append(values, steps[i].ResultDigest)
		}
		if len(values) == 0 {
			continue
		}
		for j := i + 1; j < len(steps) && !steps[i].Consumed; j++ {
			later := leafValues(steps[j].Input)
			for _, v := range values {
				if v == "" {
					continue
				}
				for _, w := range later {
					if strings.Contains(w, v) {
						steps[i].Consumed = true
						break
					}
				}
			}
		}
	}
}

func leafValues(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case bool:
		return []string{boolString(t)}
	case float64:
		return []string{trimFloat(t)}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, leafValues(e)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, leafValues(t[k])...)
		}
		return out
	default:
		return nil
	}
}

// resultValue unwraps the trimmed result to the part a later argument could
// have come from. The row's `result` is {status, result, error, metadata,
// contentId}; the inner `result` is the payload, and a data-flow reference
// into `status` would be an explanation nobody meant.
func resultValue(result map[string]any) any {
	if result == nil {
		return nil
	}
	if inner, ok := result["result"]; ok {
		return inner
	}
	return nil
}

// footprintDigest folds an observed footprint into a comparable string. Two
// actions with the same tool and arguments but different effects are not the
// same action, and this is what makes that visible to Symbolize.
func footprintDigest(fp map[string]any) string {
	if len(fp) == 0 {
		return ""
	}
	return strings.Join(leafValues(fp), "\x1f")
}

// intOf narrows a decoded payload number. The one field it reads is `seq`, a
// step's POSITION, which is an ordering -- so an out-of-range value saturates
// rather than becoming zero: a step that claimed position 0 would sort to the
// front of a run it belongs at the end of, and the procedure mined from it
// would have its steps in the wrong order. core/num names that answer.
func intOf(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	default:
		return 0
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// trimFloat writes a decoded JSON number the way canonicalization does, so a
// value that arrived as 1 in a result and 1 in an argument compares equal.
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
