package memql

// authoring_sandbox_automation.go -- the automation-compile seam for the
// Gate 1 sandbox (issue #956, follow-up 2/3).
//
// Automations compile through component/automations, which imports
// component/memql (the engine, logic runner, scheduler). That import edge
// makes a direct memql -> automations dependency a cycle, so the sandbox
// reaches the automation compiler through a registered hook instead: the
// automations package calls SetSandboxAutomationCompiler from an init()
// (it already depends on memql), and the sandbox invokes whatever was
// registered. A binary that does not link the automations package leaves
// the hook nil, and the sandbox reports the `automation` kind as skipped
// rather than silently passing it.

import (
	"sync"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// SandboxAutomationResult carries the facts the sandbox needs from a
// compiled candidate automation: its name (for the name-mismatch check) and
// the concept its graph-CDC trigger keys off (for the event-trigger
// concept-existence check, #956 follow-up 3/3). TriggerConcept is empty when
// the automation is schedule-driven or its trigger does not name a single
// concept (system.* / cognition.* events, wildcards).
type SandboxAutomationResult struct {
	Name           string
	TriggerConcept string
}

// SandboxAutomationCompiler compiles a single candidate automation from its
// .memql source, READ-ONLY against the supplied concept registry, and
// returns the compiled facts (name + trigger concept) or a parse/bind error.
// It must not mutate engine or registry state.
type SandboxAutomationCompiler func(source, origin string, registry memoryNodes.Registry) (SandboxAutomationResult, error)

var (
	sandboxAutomationCompilerMu sync.RWMutex
	sandboxAutomationCompilerFn SandboxAutomationCompiler
)

// SetSandboxAutomationCompiler registers the automation-compile hook the
// Gate 1 sandbox uses for the `automation` construct kind. The
// component/automations package calls this from init(). Passing nil
// unregisters the hook (the sandbox then skips the automation kind).
func SetSandboxAutomationCompiler(fn SandboxAutomationCompiler) {
	sandboxAutomationCompilerMu.Lock()
	defer sandboxAutomationCompilerMu.Unlock()
	sandboxAutomationCompilerFn = fn
}

// sandboxAutomationCompiler returns the registered hook, or nil when no
// automation compiler is linked into this binary.
func sandboxAutomationCompiler() SandboxAutomationCompiler {
	sandboxAutomationCompilerMu.RLock()
	defer sandboxAutomationCompilerMu.RUnlock()
	return sandboxAutomationCompilerFn
}
