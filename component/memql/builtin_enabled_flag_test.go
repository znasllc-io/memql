package memql

import (
	"io"
	"log/slog"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// #2608: the builtin Enabled flag was dead -- builtinDeclToFunction never set
// it, so all 74 builtins registered Enabled=false regardless of @enabled,
// hiding them from functions() discovery and making help() report disabled
// for functions that run fine. The lifecycle ruling applies: absent =
// enabled, @enabled = accepted no-op, @disabled = disabled for real.
func builtinFlagDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestLoadUnifiedBuiltins_LifecycleHonest(t *testing.T) {
	overlay := fstest.MapFS{"builtins.memql": {Data: []byte(`@executor("integration.probe.absent")
@args(profile="object")
@description("lifecycle probe, no annotation")
builtin probeAbsentLifecycle {
  name string @required
}

@enabled
@executor("integration.probe.enabled")
@args(profile="object")
@description("lifecycle probe, explicit enabled")
builtin probeExplicitEnabled {
  name string @required
}

@disabled
@executor("integration.probe.disabled")
@args(profile="object")
@description("lifecycle probe, explicit disabled")
builtin probeExplicitDisabled {
  name string @required
}
`)}}
	const domain = "builtinlifecyclehonest"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(builtinFlagDiscardLogger(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}

	for name, want := range map[string]bool{
		"probeAbsentLifecycle":  true,
		"probeExplicitEnabled":  true,
		"probeExplicitDisabled": false,
	} {
		fn, err := registry.Get(name)
		if err != nil {
			t.Fatalf("builtin %q did not register: %v", name, err)
		}
		if fn.Enabled != want {
			t.Errorf("builtin %q: Enabled = %v, want %v", name, fn.Enabled, want)
		}
	}
}

// The embedded tree's 74 builtins all carry @enabled or nothing -- with the
// honest flag every one must register enabled (the old accident registered
// every one disabled).
func TestLoadUnifiedBuiltins_EmbeddedTreeAllEnabled(t *testing.T) {
	registry := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(builtinFlagDiscardLogger(), registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	total := 0
	for name, fn := range registry.Snapshot() {
		if fn == nil || fn.Type != FunctionTypeBuiltin {
			continue
		}
		total++
		if !fn.Enabled {
			t.Errorf("embedded builtin %q registered disabled; the tree carries no functional @disabled", name)
		}
		if fn.FunctionKind != FunctionTypeBuiltin {
			t.Errorf("embedded builtin %q: FunctionKind = %q, want %q (functions() rows must carry a truthful kind)", name, fn.FunctionKind, FunctionTypeBuiltin)
		}
	}
	if total < 50 {
		t.Fatalf("expected the embedded tree to register its builtins (74 at time of writing, guarded at 50), got %d", total)
	}
}

// Builtins are deliberately EXCLUDED from function-backed MCP tool
// generation: the MCP connector surface is a curated @mcp opt-in (tools) --
// making the Enabled flag honest must not flood it with 74 raw builtins as
// an accident of the flag flip.
func TestRegisterFunctionTools_BuiltinsExcluded(t *testing.T) {
	functions := newFunctionRegistry()
	if err := functions.add(&Function{
		Name:        "probeEnabledBuiltin",
		Type:        FunctionTypeBuiltin,
		Description: "probe builtin",
		Executor:    "integration.probe.enabled",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}
	tools := newToolRegistry()
	registerFunctionTools(builtinFlagDiscardLogger(), functions, tools)
	if tools.Has("probeEnabledBuiltin") {
		t.Error("an enabled builtin generated an MCP function-tool; builtins must be excluded from the generated surface deliberately")
	}
}

// Review blocker on #2646: a bare top-level call is the primary way to
// invoke a builtin, and the meta-command short-circuit skipped the Enabled
// gate entirely -- a @disabled builtin executed as `help()` while rejecting
// when nested. A disabled builtin must not match as a meta-command; the
// bare call then flows through normal parsing into the same expansion gate.
func TestLookupBuiltinFunction_DisabledDoesNotMatch(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	if err := e.functions.add(&Function{
		Name:        "retiredHelper",
		Type:        FunctionTypeBuiltin,
		Description: "retired probe",
		Executor:    "integration.probe.retired",
		Enabled:     false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.functions.add(&Function{
		Name:        "liveHelper",
		Type:        FunctionTypeBuiltin,
		Description: "live probe",
		Executor:    "integration.probe.live",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := e.lookupBuiltinFunction("retiredHelper"); ok {
		t.Error("a @disabled builtin matched as a meta-command; the bare-call form bypasses the Enabled gate")
	}
	if _, ok := e.lookupBuiltinFunction("liveHelper"); !ok {
		t.Error("an enabled builtin failed to match as a meta-command")
	}
}

// Review finding on #2646: the most likely reason to @disabled a builtin is
// that its Go executor is gone -- and executor validation ran regardless of
// Enabled, so the retirement path bricked boot. A disabled builtin skips
// executor validation; an enabled one with an unknown executor still fails.
func TestInitBuiltinExecutorHandlers_DisabledSkipsValidation(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	if err := e.functions.add(&Function{
		Name:        "retiredLegacyBuiltin",
		Type:        FunctionTypeBuiltin,
		Description: "legacy probe",
		Executor:    "removedLegacyExecutor",
		Enabled:     false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.initBuiltinExecutorHandlers(); err != nil {
		t.Errorf("a @disabled builtin with a retired executor must not fail boot validation: %v", err)
	}

	if err := e.functions.add(&Function{
		Name:        "liveLegacyBuiltin",
		Type:        FunctionTypeBuiltin,
		Description: "live probe",
		Executor:    "removedLegacyExecutor",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.initBuiltinExecutorHandlers(); err == nil {
		t.Error("an ENABLED builtin with an unknown executor must still fail boot validation")
	}
}
