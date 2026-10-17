package app

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
)

func TestSARIFPublicationWiresSharedStorageOnServingAndAutomationNodes(t *testing.T) {
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{engine: engine, Logger: quietLogger()}
	for _, registration := range memql.RegisteredPlugins() {
		if registration.Name != "pipelines" {
			continue
		}
		provider, err := registration.Factory(a.pluginContext())
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.RegisterIntegration(provider); err != nil {
			t.Fatal(err)
		}
	}
	i := a.lookupPipelinesIntegration()
	if i == nil {
		t.Fatal("pipelines integration absent")
	}
	a.wirePipelineSARIF(&pipelinesFakeUploader{}, "sarif-wiring-fixture")
	i.Configure(func(d *pipelinerun.Deps) {
		if d.SARIF == nil || d.SARIF.Engine != engine || d.SARIF.Library == nil || d.SARIF.Database == nil {
			t.Fatal("publication authority is not wired")
		}
	})
	for _, source := range []string{"transport_bff.go", "transport_agent.go"} {
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "a.wirePipelineSARIF(uploader,") {
			t.Fatal("node omitted SARIF publication storage", source)
		}
	}
}
