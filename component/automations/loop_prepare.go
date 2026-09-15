package automations

// loop_prepare.go -- @loop and @mode at load (epic memql#5380; D-G and D-H of
// the loop protection plan).
//
// The parser refused what the source alone decides (component/language/
// parser, loop_mode.go): an until that is not a one-parameter lambda, a
// maxDepth or a max that is not a whole number, a key left out. What is left
// needs the loaded automation or the node's configuration, and is decided
// here, for every automation that becomes executable: the loader calls
// prepareLoopAndMode beside prepareExpressions, and the executor's
// ensurePrepared calls it for an automation built in Go.
//
//   - @loop: until parses to a lambda of one parameter (loop_until_form); the
//     automation is event-triggered (loop_not_event_triggered); maxDepth is
//     from 1 to the depth cap (loop_max_depth_range); and the @filter holds
//     until's negation as a top-level conjunct (loop_until_not_in_filter),
//     the refusal printing the @filter that would.
//   - @mode: exactly one of the four modes (mode_flags); no max on single or
//     restart, which admit no second run to bound (mode_max_not_allowed); and
//     no max below 0, which only an automation built in Go can carry, since
//     the parser refuses a written one below 1 (mode_max_range).
//
// Each refusal names the automation and the fix, on one line -- the strict
// loader lists one problem per line -- and ends with its rule id in brackets,
// which a load report reads through RuleCode. The ids are the parser's
// constants, so the two halves spell them once.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// maxChainDepthEnv names the depth cap's env value.
const maxChainDepthEnv = "MEMQL_AUTOMATION_MAX_CHAIN_DEPTH"

// defaultMaxChainDepth is the depth cap when the env value is unset: a chain
// of 16 runs is stopped before its 17th.
const defaultMaxChainDepth = 16

// maxChainDepth is the depth cap: the deepest chain of automation runs one
// root cause may drive, and the most a @loop's maxDepth may name. It is
// MEMQL_AUTOMATION_MAX_CHAIN_DEPTH, read on each call. A value that is not a
// depth -- not a whole number, or below 1, which would refuse every run a
// person's write triggers -- reads as the default, as a non-positive budget
// window does.
func maxChainDepth() int {
	if n := envIntDefault(maxChainDepthEnv, defaultMaxChainDepth); n >= 1 {
		return n
	}
	return defaultMaxChainDepth
}

// loopModeRefusal is a load refusal of an @loop or @mode. RuleCode reports its
// rule id to a load report (baseloader.CodedRefusal).
type loopModeRefusal struct {
	code, message string
}

func (r *loopModeRefusal) Error() string    { return r.message + " [" + r.code + "]" }
func (r *loopModeRefusal) RuleCode() string { return r.code }

func refuseLoopMode(code, format string, args ...any) error {
	return &loopModeRefusal{code: code, message: fmt.Sprintf(format, args...)}
}

// prepareLoopAndMode validates an automation's @loop and @mode and parses the
// loop's until once, onto Loop.UntilLambda. It runs after the trigger filter
// is parsed (prepareExpressions), which the until is judged against.
func prepareLoopAndMode(a *Automation) error {
	if a == nil {
		return nil
	}
	if a.Loop != nil {
		if err := prepareLoop(a); err != nil {
			return err
		}
	}
	if a.Mode != nil {
		return checkMode(a)
	}
	return nil
}

func prepareLoop(a *Automation) error {
	loop := a.Loop
	lam, err := languageParser.ParseV1Lambda(loop.Until)
	switch {
	case strings.TrimSpace(loop.Until) == "":
		return refuseLoopMode(languageParser.RuleLoopUntilForm,
			"automation %q: @loop names no until -- write until=row => <the state that ends the loop>, as in %s",
			a.Name, languageParser.LoopExample)
	case err != nil:
		return refuseLoopMode(languageParser.RuleLoopUntilForm,
			"automation %q: @loop's until `%s` is not a lambda of one parameter over the triggering row (%s) -- write until=row => <the state that ends the loop>, as in %s",
			a.Name, oneLine(loop.Until), oneLine(err.Error()), languageParser.LoopExample)
	case len(lam.Params) != 1:
		return refuseLoopMode(languageParser.RuleLoopUntilForm,
			"automation %q: @loop's until takes a lambda of one parameter (the triggering row), got %d -- write until=row => <the state that ends the loop>, as in %s",
			a.Name, len(lam.Params), languageParser.LoopExample)
	}
	loop.UntilLambda = lam

	if !a.IsEventTriggered() {
		return refuseLoopMode(languageParser.RuleLoopNotEventTriggered,
			"automation %q: @loop bounds a cycle of writes, and only an event-triggered automation can be in one -- give it a @trigger(event=...), or remove the @loop",
			a.Name)
	}

	if limit := maxChainDepth(); loop.MaxDepth < 1 || loop.MaxDepth > limit {
		return refuseLoopMode(languageParser.RuleLoopMaxDepthRange,
			"automation %q: @loop maxDepth=%d is outside 1 to %d, the depth cap (%s) -- name how many runs of this automation one causal chain may hold, as in %s",
			a.Name, loop.MaxDepth, limit, maxChainDepthEnv, languageParser.LoopExample)
	}

	var filter *ast.LambdaExpr
	if a.Trigger != nil {
		filter = a.Trigger.FilterLambda
	}
	switch {
	case filter == nil:
		return refuseLoopMode(languageParser.RuleLoopUntilNotInFilter,
			"automation %q: it has no @filter, so nothing excludes the rows @loop's until (%s) holds on and a converged row would fire it again -- add %s",
			a.Name, loop.Until, suggestedLoopFilter(nil, lam))
	case !untilInFilter(filter, lam):
		return refuseLoopMode(languageParser.RuleLoopUntilNotInFilter,
			"automation %q: its @filter does not exclude the rows @loop's until (%s) holds on, so a converged row would fire it again -- hold the negation as a top-level conjunct: %s",
			a.Name, loop.Until, suggestedLoopFilter(filter, lam))
	}
	return nil
}

// suggestedLoopFilter is the @filter that covers until: the existing filter's
// body `&&` until's negation, written in the filter's parameter -- or, with no
// filter, the negation alone in until's.
func suggestedLoopFilter(filter, until *ast.LambdaExpr) string {
	if filter == nil || len(filter.Params) != 1 {
		body := untilFilterConjunct(until.Body)
		return "@filter(" + ast.FormatExpr(&ast.LambdaExpr{Params: until.Params, Body: body}) + ")"
	}
	param := filter.Params[0]
	negation := untilFilterConjunct(renameParam(until.Body, until.Params[0], param))
	body := &ast.BinaryExpr{Op: "&&", Left: filter.Body, Right: negation}
	return "@filter(" + ast.FormatExpr(&ast.LambdaExpr{Params: []string{param}, Body: body}) + ")"
}

// modeKinds are the four modes, in the order a refusal lists them.
var modeKinds = []string{ModeSingle, ModeQueued, ModeRestart, ModeParallel}

func checkMode(a *Automation) error {
	m := a.Mode
	var named []string
	for _, k := range strings.Split(m.Kind, ",") {
		if k = strings.TrimSpace(k); k != "" {
			named = append(named, k)
		}
	}
	oneOf := "write exactly one of " + strings.Join(modeKinds[:len(modeKinds)-1], ", ") + " or " + modeKinds[len(modeKinds)-1]
	switch {
	case len(named) == 0:
		return refuseLoopMode(languageParser.RuleModeFlags,
			"automation %q: @mode names no mode -- %s, as in @mode(queued, max=10)", a.Name, oneOf)
	case len(named) > 1:
		return refuseLoopMode(languageParser.RuleModeFlags,
			"automation %q: @mode names %d modes, %s -- a fire can be governed one way; %s",
			a.Name, len(named), strings.Join(named, " and "), oneOf)
	case !isModeKind(named[0]):
		return refuseLoopMode(languageParser.RuleModeFlags,
			"automation %q: @mode names %q, which is not a mode -- %s", a.Name, named[0], oneOf)
	}
	kind := named[0]
	if (kind == ModeSingle || kind == ModeRestart) && m.Max != 0 {
		return refuseLoopMode(languageParser.RuleModeMaxNotAllowed,
			"automation %q: @mode(%s, max=%d) -- %s admits no second run for max to bound; max belongs to queued or parallel, so drop it or pick one of them",
			a.Name, kind, m.Max, kind)
	}
	if m.Max < 0 {
		return refuseLoopMode(languageParser.RuleModeMaxRange,
			"automation %q: @mode max=%d -- max is a whole number of at least 1, as in @mode(queued, max=10), or absent for the default",
			a.Name, m.Max)
	}
	return nil
}

func isModeKind(k string) bool {
	for _, m := range modeKinds {
		if k == m {
			return true
		}
	}
	return false
}

// oneLine folds a multi-line message onto one line: the strict loader lists
// one problem per line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
