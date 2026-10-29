package memql

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestModelSetupCapacityAndCapabilities(t *testing.T) {
	h := MachineHardware{MemoryBytes: 64 << 30, Gpu: MachineGpu{Backend: GpuBackendMetal}, Runtimes: []MachineRuntime{{Name: "ollama"}}}
	chat := CatalogProfile{ModelId: "chat", Category: "text", Runtime: "ollama", MemoryNeedBytes: 30 << 30, Flags: []string{"structured", "tools", "vision"}}
	embed := CatalogProfile{ModelId: "embed", Category: "embeddings", Runtime: "ollama", MemoryNeedBytes: 4 << 30}
	labels := map[string]any{"model:chat": "structured=1,tools=1,vision=1", "model:embed": "embeddings=1"}
	catalog := []CatalogProfile{chat, embed}
	used, err := validateModelChoices(map[string]string{"conversation": "chat", "structured": "chat", "vision": "chat", "embeddings": "embed"}, catalog, h, "darwin", labels)
	require.NoError(t, err)
	require.Equal(t, uint64(34<<30), used)
	heavy := chat
	heavy.ModelId = "heavy"
	heavy.MemoryNeedBytes = 25 << 30
	labels["model:heavy"] = "tools=1"
	_, err = validateModelChoices(map[string]string{"conversation": "chat", "tools": "heavy"}, append(catalog, heavy), h, "darwin", labels)
	require.ErrorContains(t, err, "exceeds")
	_, err = validateModelChoices(map[string]string{"vision": "embed"}, catalog, h, "darwin", labels)
	require.ErrorContains(t, err, "not installed with support")
	delete(labels, "model:chat")
	_, err = validateModelChoices(map[string]string{"conversation": "chat"}, catalog, h, "darwin", labels)
	require.ErrorContains(t, err, "not installed")
}

func TestModelSetupCombinesMultimodalProfilesWithoutDuplicatingCapacity(t *testing.T) {
	profiles := []CatalogProfile{
		{ModelId: "omni", Category: "audioIn", Runtime: "ollama", MemoryNeedBytes: 8 << 30, Flags: []string{"audioIn"}, OfferedOn: []string{"macos", "linux"}},
		{ModelId: "omni", Category: "text", Runtime: "ollama", MemoryNeedBytes: 10 << 30, Flags: []string{"vision", "tools"}, OfferedOn: []string{"macos"}},
	}
	merged := mergeSetupProfiles(profiles)
	require.Len(t, merged, 1)
	require.ElementsMatch(t, []string{"conversation", "tools", "vision", "audioIn"}, profilePurposes(merged[0]))
	require.Equal(t, int64(10<<30), merged[0].MemoryNeedBytes)
	require.Equal(t, []string{"macos"}, merged[0].OfferedOn)
	require.Equal(t, []string{"audioIn"}, profiles[0].Flags, "normalization must not mutate the shared catalog")
	h := MachineHardware{MemoryBytes: 64 << 30, Gpu: MachineGpu{Backend: GpuBackendMetal}, Runtimes: []MachineRuntime{{Name: "ollama"}}}
	used, err := validateModelChoices(map[string]string{"conversation": "omni", "audioIn": "omni"}, merged, h, "darwin", map[string]any{"model:omni": "audioin=1"})
	// A runtime advertising only transcription cannot claim conversation just
	// because the model card lists it. Verify the catalog union independently.
	require.Error(t, err)
	used, err = validateModelChoices(map[string]string{"conversation": "omni", "tools": "omni", "audioIn": "omni"}, merged, h, "darwin", map[string]any{"model:omni": "tools=1,audioin=1"})
	require.NoError(t, err)
	require.Equal(t, uint64(10<<30), used)
	profiles[1].Runtime = "mlx"
	require.Zero(t, mergeSetupProfiles(profiles)[0].MemoryNeedBytes, "conflicting runtime profiles require verification")
}

func TestModelSetupFitUnknownDiscreteAndLargeServers(t *testing.T) {
	p := CatalogProfile{ModelId: "large", Runtime: "ollama", MemoryNeedBytes: 200 << 30}
	h := MachineHardware{MemoryBytes: 512 << 30, Gpu: MachineGpu{Backend: GpuBackendNone}, Runtimes: []MachineRuntime{{Name: "ollama"}}}
	require.Empty(t, modelFit(p, h, "linux"))
	h.Gpu = MachineGpu{Backend: GpuBackendCuda, VramBytes: 24 << 30}
	require.Contains(t, modelFit(p, h, "linux"), "more model memory")
	p.MemoryNeedBytes = 0
	require.Contains(t, modelFit(p, h, "linux"), "not verified")
	require.Contains(t, modelFit(p, MachineHardware{}, "linux"), "Waiting")
	p.Runtime = "cloud"
	require.Contains(t, modelFit(p, h, "linux"), "Cloud")
}

func TestModelSetupRoutingIsExplicitAndHandlesCompoundNeeds(t *testing.T) {
	s := ModelSetupFromRow(map[string]any{"revision": "saved", "choices": map[string]any{"conversation": "small", "tools": "large"}})
	require.True(t, s.Allows("large", ModelPurpose(true, true, false, false, false, false, false), 8192))
	require.False(t, s.Allows("small", "tools", 8192))
	require.False(t, s.Allows("large", "tools", 32769))
	require.False(t, s.Allows("large", "vision", 8192))
	require.True(t, (ModelSetup{}).Allows("any", "tools", 262144))
	require.False(t, (ModelSetup{Revision: "saved"}).Allows("any", "tools", 8192))
	require.False(t, advertisedPurpose(map[string]any{"model:flux": "imagegen=1"}, "flux", "conversation"))
}

func TestModelSetupRestrictsProviderAvailabilityOnTheServingMachine(t *testing.T) {
	m := FleetModelAdvertisedOnEveryMachineForTest(FleetModel{ModelId: "chat", Tools: true, ContextWindow: 131072, Machines: []FleetMachine{{Online: true, ModelSetup: ModelSetup{Revision: "one", Choices: map[string]string{"conversation": "chat", "tools": "other"}}}}})
	ok, _ := m.eligibleFor(FleetNeeds{Tools: true})
	require.False(t, ok)
	ok, _ = m.eligibleFor(FleetNeeds{})
	require.True(t, ok)
	m.Machines = append(m.Machines, FleetMachine{Online: false, ModelSetup: ModelSetup{Revision: "two", Choices: map[string]string{"tools": "chat"}}})
	m = FleetModelAdvertisedOnEveryMachineForTest(m)
	ok, _ = m.eligibleFor(FleetNeeds{Tools: true})
	require.False(t, ok, "an offline sibling's selection cannot make the online machine eligible")
	m.Machines[1].Online = true
	ok, _ = m.eligibleFor(FleetNeeds{Tools: true})
	require.True(t, ok)
	ok, _ = m.eligibleFor(FleetNeeds{Tools: true, MinContextWindow: 65536})
	require.False(t, ok)
}
