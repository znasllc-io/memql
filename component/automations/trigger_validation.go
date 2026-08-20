package automations

// trigger_validation.go -- the load-time gate that refuses an automation
// wired to nothing (memql#3614 items 3 + memql#3615).
//
// An automation that silently never runs is indistinguishable from one that
// works until the day you need it. Two shapes produced exactly that:
//
//   - No reachable trigger at all. A typo'd kwarg (`@trigger(evnt=...)`), a
//     bare `@trigger()`, or no `@trigger` line whatsoever all compiled to
//     `topic="" schedule="" enabled=true`. `Scheduler.run` then filed the
//     automation in `s.automations` and registered NEITHER a cron entry nor a
//     subscription -- counted in `automationCount`, absent from both
//     `scheduledCount` and `eventTriggeredCount`. The AUTHORED scheduler has
//     always refused this (`authored_scheduler.go`, "neither an event trigger
//     nor a schedule"); the core one did not. Two spellings of one construct,
//     two outcomes. They agree now, and the refusal moved to LOAD so the
//     strict-boot gate (memql#2830) is what surfaces it.
//
//   - An unparseable cron. `scheduleAutomation` returns the parse error and
//     `Scheduler.run` does `logError(...); continue` -- boot succeeds, the
//     automation is loaded and enabled, and never fires. Strict boot never saw
//     it because the failure happened at scheduler registration rather than at
//     construct load. Parsing here, with the SAME parser the scheduler will
//     use, is what puts it in front of the strict-boot gate.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// automationCronParser is the parser the SCHEDULER uses, byte for byte:
// `cron.New(cron.WithSeconds())` installs exactly this field set
// (scheduler.go, authored_scheduler.go). Validating against anything else
// would be a second dialect, which is the whole defect.
var automationCronParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// plainSecondsField matches a seconds field that names one fixed second, the
// only spelling that cannot fire more than once a minute.
var plainSecondsField = regexp.MustCompile(`^\d+$`)

// validateTriggerWiring refuses an automation that no trigger can ever reach.
//
// Returns an error for the two structural failures; emits a WARN for the
// sub-minute cron, which is legal and occasionally intended.
func (l *Loader) validateTriggerWiring(automation *Automation) error {
	if automation == nil {
		return nil
	}

	scheduled := automation.IsScheduled()
	evented := automation.IsEventTriggered()

	if !scheduled && !evented {
		return fmt.Errorf("automation %q has neither an event trigger nor a schedule, so nothing can ever run it. "+
			"Add one of:\n"+
			"  @trigger(event=\"node.created\", concept=\"v1:<ns>:<concept>\", partition=\"*\")   graph CDC\n"+
			"  @trigger(event=\"<topic>\")                                                  a published topic\n"+
			"  @trigger(schedule=\"0 */10 * * * *\")                                        cron (6 fields)\n"+
			"A misspelled kwarg reads as no trigger at all -- `event=` and `schedule=` are the only two that wire anything",
			automation.Name)
	}

	if scheduled {
		if err := validateCronExpression(automation.Schedule); err != nil {
			return fmt.Errorf("automation %q: %w", automation.Name, err)
		}
		if l.logger != nil && cronFiresSubMinute(automation.Schedule) {
			l.logger.Warn("automation cron fires more than once per minute -- confirm this is the intent",
				"component", ComponentName,
				"automation", automation.Name,
				"schedule", automation.Schedule,
				"note", "MemQL crons carry a LEADING SECONDS field, so `*/5 * * * * *` is every 5 SECONDS; every 5 minutes is `0 */5 * * * *`")
		}
	}

	return nil
}

// validateCronExpression parses expr with the scheduler's own parser.
//
// The 5-field crontab spelling gets its own error, because it is the single
// most likely thing an author types and the one whose failure is least
// legible: `0 2 * * *` is crontab.guru's "daily at 2am" and every operator
// knows it. We name the 6-field equivalent rather than silently promoting it
// -- promotion would leave the corpus carrying two field-count conventions at
// once, so `*/5 * * * *` would mean every 5 minutes while `*/5 * * * * *`
// meant every 5 seconds, and a reader would have to count fields to tell
// which. Refusing with the exact replacement string states the promotion
// where it belongs: to the author, before it ships.
func validateCronExpression(expr string) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return fmt.Errorf("empty cron schedule")
	}

	_, err := automationCronParser.Parse(trimmed)
	if err == nil {
		return nil
	}

	if promoted, ok := promoteFiveFieldCron(trimmed); ok {
		if _, perr := automationCronParser.Parse(promoted); perr == nil {
			return fmt.Errorf("invalid cron schedule %q: %w. "+
				"That is the 5-field crontab spelling; MemQL crons carry a LEADING SECONDS field "+
				"(second minute hour day-of-month month day-of-week). Write %q for the same schedule. "+
				"The leading 0 is not added for you on purpose -- a corpus that accepted both widths would "+
				"make `*/5 * * * *` (every 5 minutes) and `*/5 * * * * *` (every 5 seconds) indistinguishable "+
				"without counting fields",
				trimmed, err, promoted)
		}
	}

	return fmt.Errorf("invalid cron schedule %q: %w. "+
		"MemQL crons are SIX fields (second minute hour day-of-month month day-of-week), e.g. "+
		"\"0 */10 * * * *\" = every 10 minutes, \"0 0 2 * * *\" = daily at 02:00. "+
		"Descriptors (@hourly, @daily, @weekly, @monthly, @yearly, @every <duration>) also parse",
		trimmed, err)
}

// promoteFiveFieldCron returns expr with a leading `0` seconds field when expr
// is exactly five whitespace-separated fields. The result is only ever used to
// BUILD AN ERROR MESSAGE -- nothing in the loader schedules a promoted string.
func promoteFiveFieldCron(expr string) (string, bool) {
	if strings.HasPrefix(expr, "@") {
		return "", false
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", false
	}
	return "0 " + strings.Join(fields, " "), true
}

// cronFiresSubMinute reports whether expr can fire more than once in a minute.
//
// This is the other half of the 6-vs-5 field ambiguity, and the half that
// cannot be refused: `*/5 * * * * *` is a perfectly valid 6-field expression
// that reads as "every 5 minutes" to anyone with crontab habits and fires
// every 5 seconds -- 60x more often than intended. Legal, occasionally
// deliberate, so it warns rather than refuses.
func cronFiresSubMinute(expr string) bool {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return false
	}

	if strings.HasPrefix(trimmed, "@") {
		// @every <duration> is the only descriptor with sub-minute reach.
		rest, ok := strings.CutPrefix(trimmed, "@every ")
		if !ok {
			return false
		}
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return false
		}
		return d > 0 && d < time.Minute
	}

	fields := strings.Fields(trimmed)
	if len(fields) < 6 {
		// Fewer than six fields never parses (validateCronExpression already
		// refused it), so there is nothing to warn about.
		return false
	}
	seconds := fields[0]
	if !plainSecondsField.MatchString(seconds) {
		return true
	}
	// A fixed second is once a minute regardless of its value; the parse
	// already bounded it to 0-59, so the conversion is belt-and-braces.
	if _, err := strconv.Atoi(seconds); err != nil {
		return true
	}
	return false
}
