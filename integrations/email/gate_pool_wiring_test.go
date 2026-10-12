package email

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// gate_pool_wiring_test.go -- the email plug-in's gates, with no gate pool, say
// which wiring they lack (memql#6022, F9 fix round 4). The connection store used to
// answer every failure to take its gate -- a busy lock included -- with "email
// connection coordination is unavailable" and nothing else.

func TestEmailGatesWithNoPoolNameTheWiringTheyLack(t *testing.T) {
	store := &connectionStore{}
	var state map[string]any
	err := store.change(context.Background(), "some-key", &state, func(bool) error { return nil })
	if !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "PluginContext.GateDB") {
		t.Errorf("an unwired connection store answered %v, want ErrWriteGatePoolMissing naming PluginContext.GateDB", err)
	}
	release, err := (&storedACSOperations{connection: store}).lock(context.Background(), "some-operation")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "PluginContext.GateDB") {
		t.Errorf("an unwired send-receipt lock answered %v, want ErrWriteGatePoolMissing naming PluginContext.GateDB", err)
	}
}

func TestTheEmailPluginRefusesAContextWithNoGatePool(t *testing.T) {
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "email" {
			continue
		}
		provider, err := p.Factory(memql.PluginContext{})
		if err == nil || provider != nil {
			t.Fatalf("the email plug-in materialized without a gate pool: %v", provider)
		}
		if !strings.Contains(err.Error(), "GateDB") {
			t.Errorf("the refusal does not name PluginContext.GateDB: %v", err)
		}
		return
	}
	t.Fatal("the email plug-in is not registered")
}
