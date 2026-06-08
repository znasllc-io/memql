// budget.go
//
// Global automation-execution budget (memql#1142). This is the automation-
// side analogue of the LLM kill-switch (memql#1145): a HARD ceiling that
// STOPS a storm, not a log line that merely observes one.
//
// Before this, the only thing standing between a misfiring automation and an
// unbounded re-fire loop was: (a) storm detection that was LOG-ONLY (>20/min
// per automation -> a WARN, nothing blocked), and (b) a concurrency limiter
// that caps how many run AT ONCE but not how many run PER MINUTE. A failing
// automation (e.g. an emit-failed-card automation that re-fires on its own
// failure, or a plan-level loop that re-creates a plan each cycle) could
// therefore execute hundreds of times a minute, and each execution can drive
// fresh LLM calls / plan churn.
//
// The budget is a process-global, cross-executor ceiling: both the event and
// schedule executors (built separately in scheduler.go) share ONE budget
// instance, so the cap is truly global. Two dimensions, both windowed:
//   - a TOTAL executions/window ceiling across every automation, and
//   - a per-automation executions/window ceiling,
//
// whichever trips first skips the execution with a clear status. Defaults are
// generous for real traffic (a healthy cluster runs well under them) and
// lethal to a storm. Even when the budget is generous, the LLM kill-switch
// (memql#1145) is the hard money stop behind it -- defense in depth.
package automations

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Generous-for-real-traffic, lethal-to-a-storm defaults. A healthy
	// cluster's whole automation fleet stays well under 600/min, and no
	// single automation legitimately fires 120 times/min sustained; a storm
	// blows straight through both.
	defaultAutomationBudgetGlobalMax  = 600
	defaultAutomationBudgetPerAutoMax = 120
	defaultAutomationBudgetWindowSecs = 60
)

// windowCount is a fixed-window counter (count since windowStart). Cheaper
// than a sliding window of timestamps and adequate for a storm backstop: a
// storm sends thousands/min, so fixed-window edge effects are irrelevant.
type windowCount struct {
	count       int
	windowStart time.Time
}

// automationBudget is the process-global execution ceiling shared by every
// Executor. Safe for concurrent use.
type automationBudget struct {
	enabled    bool
	globalMax  int // 0 = unlimited
	perAutoMax int // 0 = unlimited
	window     time.Duration

	mu        sync.Mutex
	global    windowCount
	perAuto   map[string]*windowCount
	lastAlert time.Time // throttles the loud ERROR to once per window
	now       func() time.Time
}

// sharedAutomationBudget is the one budget instance both executors reference.
var sharedAutomationBudget = newAutomationBudgetFromEnv()

func newAutomationBudgetFromEnv() *automationBudget {
	b := &automationBudget{
		enabled:    envBoolDefault("MEMQL_AUTOMATION_BUDGET_ENABLED", true),
		globalMax:  envIntDefault("MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_WINDOW", defaultAutomationBudgetGlobalMax),
		perAutoMax: envIntDefault("MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_AUTOMATION", defaultAutomationBudgetPerAutoMax),
		window:     time.Duration(envIntDefault("MEMQL_AUTOMATION_BUDGET_WINDOW_SECONDS", defaultAutomationBudgetWindowSecs)) * time.Second,
		perAuto:    map[string]*windowCount{},
		now:        time.Now,
	}
	if b.window <= 0 {
		b.window = defaultAutomationBudgetWindowSecs * time.Second
	}
	return b
}

// admit records one execution attempt for automationName and reports whether
// it is allowed. When it returns allowed=false the execution must be skipped.
// reason is non-empty ONLY on the first block within the current window (the
// alert moment) so the caller can log loudly exactly once per window without
// flooding; subsequent blocks return allowed=false with an empty reason.
func (b *automationBudget) admit(automationName string) (allowed bool, reason string) {
	if b == nil || !b.enabled {
		return true, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()

	// Roll the global window.
	if now.Sub(b.global.windowStart) > b.window {
		b.global = windowCount{windowStart: now}
	}
	// Roll the per-automation window.
	pc := b.perAuto[automationName]
	if pc == nil || now.Sub(pc.windowStart) > b.window {
		pc = &windowCount{windowStart: now}
		b.perAuto[automationName] = pc
	}

	// Check ceilings BEFORE incrementing so exactly `max` executions are
	// admitted per window and the (max+1)th is the first to be skipped.
	over := ""
	if b.globalMax > 0 && b.global.count >= b.globalMax {
		over = "global"
	} else if b.perAutoMax > 0 && pc.count >= b.perAutoMax {
		over = "per-automation"
	}
	if over != "" {
		// Throttle the alert reason to once per window.
		if b.lastAlert.IsZero() || now.Sub(b.lastAlert) >= b.window {
			b.lastAlert = now
			reason = over
		}
		return false, reason
	}

	b.global.count++
	pc.count++
	return true, ""
}

// reset clears all counters. For tests and a future admin lever.
func (b *automationBudget) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.global = windowCount{}
	b.perAuto = map[string]*windowCount{}
	b.lastAlert = time.Time{}
}

// --- small env helpers (local; the memql package has its own copies) --------

func envBoolDefault(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envIntDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}
