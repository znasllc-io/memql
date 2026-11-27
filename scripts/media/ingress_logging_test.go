package media

import (
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// Pinned Ingress 1.6.0 logs a stream key at WARN in pkg/whip/server.go.
// Its HandlerLogger also forwards opaque child diagnostics; filtering severity
// cannot establish credential redaction. Docker must retain no ingress output.
func TestLocalIngressUsesProviderLoggingSchemaAndNoRetainedOutput(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub(), "--ingress=true", "--wait-seconds=0")...)
	if code != 0 || !env.OK {
		t.Fatalf("local ingress render failed: exit=%d", code)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(f.stateDir, "ingress.yaml"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, obsolete := cfg["log_level"]; obsolete || !reflect.DeepEqual(cfg["logging"], map[string]any{"level": "error"}) {
		t.Fatal("pinned provider requires logging.level; unknown log_level silently leaves its default")
	}
	var compose struct {
		Services map[string]struct {
			Logging map[string]string `yaml:"logging"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(f.root, "deploy/media/compose.ingress.yaml"))), &compose); err != nil {
		t.Fatal(err)
	}
	for name, service := range compose.Services {
		if name == "ingress" {
			if !reflect.DeepEqual(service.Logging, map[string]string{"driver": "none"}) {
				t.Fatal("ingress provider output must have no Docker log retention")
			}
		} else if len(service.Logging) != 0 {
			t.Fatalf("ingress overlay changed the %s service's logging", name)
		}
	}
}
