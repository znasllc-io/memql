package library

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// gate_pool_wiring_test.go -- a library integration with no gate pool says which
// wiring it lacks (memql#6022, F9 fix round 4): at construction for the plug-in,
// at the first gate for NewIntegration.

func TestALibraryIntegrationWithNoGatePoolNamesTheWiringItLacks(t *testing.T) {
	i := NewIntegration(nil)
	for name, gate := range map[string]func(context.Context, string) (func(), error){"document": i.versionGate, "file": i.fileVersionGate} {
		release, err := gate(context.Background(), "some-id")
		if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) {
			t.Errorf("the %s version gate of an unwired library answered %v, want ErrWriteGatePoolMissing", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "library.NewIntegration") || !strings.Contains(err.Error(), "PluginContext.GateDB") {
			t.Errorf("the %s version gate does not say which wiring is missing: %v", name, err)
		}
	}
	unopened := NewIntegration(nil, func() *sql.DB { return nil })
	if _, err := unopened.versionGate(context.Background(), "some-id"); !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Errorf("a resolver with no pool answered %v", err)
	}
}

func TestTheLibraryPluginRefusesAContextWithNoGatePool(t *testing.T) {
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "library" {
			continue
		}
		provider, err := p.Factory(memql.PluginContext{})
		if err == nil || provider != nil {
			t.Fatalf("the library plug-in materialized without a gate pool: %v", provider)
		}
		if !strings.Contains(err.Error(), "GateDB") {
			t.Errorf("the refusal does not name PluginContext.GateDB: %v", err)
		}
		return
	}
	t.Fatal("the library plug-in is not registered")
}
