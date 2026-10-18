package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFleetCapabilityDiscoveryDoesNotBorrowOfflineModality(t *testing.T) {
	models := []FleetModel{{ModelId: "same-name", ImageGen: true, Vision: true, Machines: []FleetMachine{
		{RegistrationId: "asleep", Capabilities: FleetModelCapabilities{ImageGen: true}},
		{RegistrationId: "awake", Online: true, Capabilities: FleetModelCapabilities{Vision: true}},
		{RegistrationId: "busy", Online: true, ActiveCount: 1, MaxConcurrent: 1, Capabilities: FleetModelCapabilities{ImageGen: true}},
	}}}
	counts := fleetCapabilityCounts(models)
	require.Equal(t, map[string]any{"advertised": 2, "online": 1, "idle": 0}, counts["imageGen"])
	require.Equal(t, map[string]any{"advertised": 1, "online": 1, "idle": 1}, counts["vision"])
	models[0].Machines[0].Online = true
	require.Equal(t, map[string]any{"advertised": 2, "online": 2, "idle": 1}, fleetCapabilityCounts(models)["imageGen"])
}

type capabilityCatalog struct {
	owners []string
	err    error
}

func (c *capabilityCatalog) Catalog(_ context.Context, owner string) ([]FleetModel, error) {
	c.owners = append(c.owners, owner)
	return nil, c.err
}
func TestFleetCapabilityDiscoveryIsCallerScopedAndUnknownOnReadFailure(t *testing.T) {
	catalog := &capabilityCatalog{}
	registry := newProviderRegistry()
	registry.SetFleetCatalog(catalog)
	e := &MemQLEngine{providers: registry}
	require.Equal(t, "unavailable", e.workFleetCapabilities(context.Background())["status"])
	require.Empty(t, catalog.owners, "must never fall back to shared anonymous discovery")
	require.Equal(t, "observed", e.workFleetCapabilities(userCtx("alice"))["status"])
	require.Equal(t, []string{"alice"}, catalog.owners)
	catalog.err = errors.New("catalog unavailable")
	facts := e.workFleetCapabilities(userCtx("alice"))
	require.Equal(t, "unavailable", facts["status"])
	require.NotContains(t, facts, "modalities", "read failure is not zero capabilities")
}
