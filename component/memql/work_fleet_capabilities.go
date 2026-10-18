package memql

import (
	"context"
	"time"
)

// Capability discovery reports current advertisements, not a promise that a
// policy will select a model. Use the same caller-scoped catalog as dispatch;
// no anonymous/shared fallback and no hardware names from other people.
func (e *MemQLEngine) workFleetCapabilities(ctx context.Context) map[string]any {
	result := map[string]any{
		"checkedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"status":    "unavailable",
		"note":      "Current model advertisements from your machines and machines shared with you. Vision reads images; imageGen creates them. Counts are model/machine pairs, not distinct machines. Offline and busy are temporary states. This snapshot is not policy approval or a reservation: execution rechecks capability, sharing, policy, capacity and budget. Missing advertisements or an incomplete catalog do not prove a permanent platform limitation.",
	}
	owner := actingUserFromContext(ctx)
	if owner == "" || e.providers == nil || !e.providers.FleetCatalogInstalled() {
		return result
	}
	models, err := e.providers.FleetCatalog(ContextWithFreshRead(ctx), owner)
	if err != nil {
		return result
	}
	result["status"] = "observed"
	result["modalities"] = fleetCapabilityCounts(models)
	return result
}

func fleetCapabilityCounts(models []FleetModel) map[string]any {
	result := map[string]any{}
	for _, name := range []string{"vision", "imageGen", "audioIn", "audioOut", "embeddings", "tools", "structuredOutput"} {
		offered, online, idle := 0, 0, 0
		seen := map[string]bool{}
		for _, model := range models {
			for _, machine := range model.Machines {
				c := machine.Capabilities
				flags := map[string]bool{"vision": c.Vision, "imageGen": c.ImageGen, "audioIn": c.AudioIn, "audioOut": c.AudioOut, "embeddings": c.Embeddings, "tools": c.Tools, "structuredOutput": c.StructuredOutput}
				key := model.ModelId + "\x00" + machine.RegistrationId
				if !flags[name] || seen[key] {
					continue
				}
				seen[key] = true
				offered++
				if machine.Online {
					online++
					if !machine.Busy() {
						idle++
					}
				}
			}
		}
		result[name] = map[string]any{"advertised": offered, "online": online, "idle": idle}
	}
	return result
}
