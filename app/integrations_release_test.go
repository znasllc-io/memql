package app

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/release"
)

func TestReleaseCandidatesWireTheNativeArtifactStore(t *testing.T) {
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{engine: engine, Logger: quietLogger()}
	var integration *release.Integration
	for _, registration := range memql.RegisteredPlugins() {
		if registration.Name != "release" {
			continue
		}
		provider, err := registration.Factory(app.pluginContext())
		if err != nil {
			t.Fatal(err)
		}
		integration = provider.(*release.Integration)
		if err := engine.RegisterIntegration(provider); err != nil {
			t.Fatal(err)
		}
	}
	if integration == nil {
		t.Fatal("release integration is not registered")
	}
	uploader := &pipelinesFakeUploader{}
	app.wireReleaseCandidates(uploader, "release-wiring-fixture")
	store := app.pipelinesLibraryStoreFor(uploader, "release-wiring-fixture").(release.CandidateLibrary)
	if err := integration.ConfigureCandidates(release.CandidateDependencies{Database: func() *sql.DB { return nil }, Library: store}); err == nil {
		t.Fatal("app did not bind retained-artifact authority before serving")
	}
	for _, source := range []string{"transport_bff.go", "transport_agent.go"} {
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "a.wireReleaseCandidates(uploader,") {
			t.Fatal("serving/automation node omitted release storage", source)
		}
	}
}
