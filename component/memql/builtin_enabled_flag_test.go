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
	total, disabled := 0, 0
	for name, fn := range registry.Snapshot() {
		if fn == nil || fn.Type != FunctionTypeBuiltin {
			continue
		}
		total++
		if !fn.Enabled {
			disabled++
			t.Errorf("embedded builtin %q registered disabled; the tree carries no functional @disabled", name)
		}
	}
	if total < 50 {
		t.Fatalf("expected the embedded tree to register its builtins (74 at time of writing, guarded at 50), got %d", total)
	}
	_ = disabled
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
