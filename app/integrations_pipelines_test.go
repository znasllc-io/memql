package app

import (
	"io"
	"log/slog"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
)

// TestWirePipelinesGivesTheDriverTheClustersDomain holds the one line that
// makes MEMQL_DOMAIN reach a step. The driver hands every step its cluster's
// front-door domain from Deps.Domain, and a port nobody wires is green in every
// driver test and inert in the cluster: each step simply runs with no domain.
// So the wiring is asserted on the REGISTERED instance, the way the procedure
// plug-in's compile gate is (integrations_procedure_test.go).
func TestWirePipelinesGivesTheDriverTheClustersDomain(t *testing.T) {
	var reg *memql.PluginRegistration
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == pipelinerun.IntegrationName {
			p := p
			reg = &p
		}
	}
	if reg == nil {
		t.Fatal("the pipelines plug-in is not registered in the app binary")
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

	// Materialized the way materializePlugins does it: the registered factory,
	// the app's own plugin context, the engine's registry.
	provider, err := reg.Factory(app.pluginContext())
	if err != nil || provider == nil {
		t.Fatalf("the pipelines factory did not build an integration: %v", err)
	}
	if err := engine.RegisterIntegration(provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	integ := app.lookupPipelinesIntegration()
	if integ == nil {
		t.Fatal("the registered pipelines integration is not the expected type")
	}

	// The plug-in keeps its ports private; Configure hands the live Deps to the
	// function it is given, so that is the one place to read one from.
	domainPort := func() (port func() string) {
		integ.Configure(func(d *pipelinerun.Deps) { port = d.Domain })
		return port
	}
	if domainPort() != nil {
		t.Fatal("a Domain port was installed before the wiring ran, so this test would pass having wired nothing")
	}
	app.wirePipelines()
	port := domainPort()
	if port == nil {
		t.Fatal("wirePipelines installed no Domain port, so no step is told its cluster's domain")
	}

	// The domain is a value read at each call, trimmed, and absent when unset.
	t.Setenv("MEMQL_DOMAIN", "  example.test ")
	if got := port(); got != "example.test" {
		t.Errorf("Domain() = %q, want the cluster's MEMQL_DOMAIN, trimmed", got)
	}
	t.Setenv("MEMQL_DOMAIN", "other.test")
	if got := port(); got != "other.test" {
		t.Errorf("Domain() = %q after MEMQL_DOMAIN changed, want other.test: the port reads the environment at each call", got)
	}
	t.Setenv("MEMQL_DOMAIN", "")
	if got := port(); got != "" {
		t.Errorf("Domain() = %q with no MEMQL_DOMAIN, want none", got)
	}
}
