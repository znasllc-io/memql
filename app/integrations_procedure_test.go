package app

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// TestTheProcedureIntegrationShipsWithItsCompileGate is gaps G1 and G7 of epic
// memql#5408, and both are the same shape: a feature that is complete, tested
// and INERT in the shipped binary.
//
//   - G1: no binary imported integrations/procedure, so its builtins reached
//     no executor on any node. The registration is asserted in THIS test
//     binary, which links app/ exactly as every node binary does. (It holds
//     twice over: plugins_core.go blank-imports the package, and this
//     wiring's own file imports it by name -- so removing the blank import
//     alone does not unregister it, and removing both does not compile.)
//   - G7: the plug-in looked for Gate 1 on its engine, which has no such
//     method, so no learned procedure was ever compiled or recorded as
//     re-runnable. The gate is asserted INSTALLED on the registered instance
//     by the wiring integrationsCore calls on every node type.
//
// A capability that refuses, or a procedure that is "not re-runnable", is a
// well-formed answer -- which is why neither gap failed a single test until
// the wiring itself was asserted.
func TestTheProcedureIntegrationShipsWithItsCompileGate(t *testing.T) {
	var reg *memql.PluginRegistration
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == "procedure" {
			p := p
			reg = &p
		}
	}
	if reg == nil {
		t.Fatal("the procedure plug-in is not registered in the app binary: plugins_core.go must blank-import integrations/procedure (gap G1)")
	}

	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	app := &App{engine: engine, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Materialized the way materializePlugins does it: the registered
	// factory, the app's own plugin context, the engine's registry.
	if err := reg.ValidateContract(); err != nil {
		t.Fatal(err)
	}
	provider, err := reg.Factory(app.pluginContext())
	if err != nil || provider == nil {
		t.Fatalf("the procedure factory did not build an integration: %v", err)
	}
	if err := engine.RegisterIntegration(provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}

	integ := app.lookupProcedureIntegration()
	if integ == nil {
		t.Fatal("the registered procedure integration is not the expected type")
	}
	if integ.CompileGateInstalled() {
		t.Fatal("the gate was installed before the wiring ran, so this test would pass having wired nothing")
	}
	app.wireProcedureIntegration()
	if !integ.CompileGateInstalled() {
		t.Fatal("wireProcedureIntegration did not install Gate 1 on the registered instance (gap G7)")
	}

	// And the wiring is on the boot path every node type takes.
	src, err := os.ReadFile("integrations.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "a.wireProcedureIntegration()") {
		t.Fatal("integrationsCore does not call wireProcedureIntegration, so no node installs Gate 1")
	}
}
