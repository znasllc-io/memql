//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestComputerTargetSelectionAcrossReplicas(t *testing.T) {
	first := machine("first", func(c *Candidate) {
		c.MachineId = "machine-a"
		c.DisplayName = "Desktop"
		c.ConnectedNodeId = "other-replica"
		c.Labels = map[string]string{"machineId": "forged-operator-value"}
		c.ActionContracts[permission.Contract] = permission.Version
	})
	second := machine("second", func(c *Candidate) {
		c.MachineId = "machine-b"
		c.DisplayName = "Laptop"
		c.ConnectedNodeId = "another-replica"
		c.Labels = map[string]string{"machineId": "machine-a"}
	})
	fleet := &fakeFleet{owner: ownerA, machines: []Candidate{first, second}}
	dispatcher := &Dispatcher{router: newTestRouter(fleet), permissions: &permissionTestAuthority{}}
	// No stream on this replica. Status and selection must use shared rows.
	integration := NewIntegration(dispatcher, workerservice.NewRegistry(nil, fleetNow), nil, nil)
	nodes, err := integration.handleStatus(asPerson(ownerA), nil, 0)
	require.NoError(t, err)
	var availability Availability
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &availability))
	require.Len(t, availability.Machines, 2)
	require.Equal(t, "Desktop", availability.Machines[0].Name)
	require.Equal(t, map[string]string{"machineId": "machine-a"}, availability.Machines[0].RequireLabels)
	require.True(t, availability.Machines[0].ComputerPermissionSupported)
	require.False(t, availability.Machines[1].ComputerPermissionSupported)
	require.True(t, availability.Machines[1].HeadlessOnline, "an older inference worker is not offline")

	operation := map[string]any{"tool": "workerHost", "action": "fs_list", "args": map[string]any{"path": "/home/person"}, "directory": "/home/person"}
	_, _, err = dispatcher.prepareComputerPermission(context.Background(), ownerA, "agent", "run", operation, nil)
	require.ErrorContains(t, err, "Choose which computer", "do not silently discard the older machine before asking")
	scope, existing, err := dispatcher.prepareComputerPermission(context.Background(), ownerA, "agent", "run", operation, availability.Machines[0].RequireLabels)
	require.NoError(t, err)
	require.Nil(t, existing)
	require.Equal(t, first.RegistrationId, scope.RegistrationID)
	require.Equal(t, first.MachineId, scope.MachineID, "operator labels cannot redirect the chosen machine")
	_, _, err = dispatcher.prepareComputerPermission(context.Background(), ownerA, "agent", "run", operation, availability.Machines[1].RequireLabels)
	require.ErrorContains(t, err, "updated MemQL Cockpit", "a selected older machine must not fall back to the updated machine")
	require.Empty(t, fleet.touched, "discovery and approval preparation must not reserve or execute a machine")

	fleet.machines[1].ConnectedNodeId = ""
	_, _, err = dispatcher.prepareComputerPermission(context.Background(), ownerA, "agent", "run", operation, nil)
	require.ErrorContains(t, err, "Choose which computer", "an offline alternative does not establish the browser's host")

	integration.engine = &memql.MemQLEngine{}
	integration.ScopeRequester = func(context.Context, string, string, map[string]any) (string, error) {
		t.Fatal("an unresolved target reached the approval writer")
		return "", nil
	}
	args := map[string]any{"agentId": "agent", "intent": "inspect", "requestedScope": "observe", "summary": "Inspect files"}
	_, err = integration.handleRequestScope(asPerson(ownerA), args, 0)
	require.ErrorContains(t, err, "Choose which computer")
	args["requireLabels"] = map[string]any{"machineId": "missing"}
	_, err = integration.handleRequestScope(asPerson(ownerA), args, 0)
	require.ErrorContains(t, err, "No eligible computer", "a nonexistent target must not create an unrestricted scope")
}
