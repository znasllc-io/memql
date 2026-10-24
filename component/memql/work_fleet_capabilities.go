package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
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

// Prefetched discovery is a real read even when the model did not request a
// tool round trip. Keep its bounded public receipt for review on any replica,
// separately from resumable agent history so it cannot replace a continuation.
func (e *MemQLEngine) recordWorkCapabilityEvidence(ctx context.Context, search string, matches []map[string]any, fleet map[string]any, total int) error {
	run, ok := common.RunFromContext(ctx)
	if !ok || !run.IsRun() {
		return nil
	}
	actor, _ := auth.AccessFromContext(ctx)
	if actor == nil || BareShortId(actor.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("capability evidence requires owned work")
	}
	contracts := make([]map[string]any, 0, len(matches))
	for _, match := range matches {
		contracts = append(contracts, map[string]any{"name": match["name"], "kind": match["kind"], "app": match["app"], "description": boundedMemoryText(fmt.Sprint(match["description"]), 500)})
	}
	value, err := json.Marshal(map[string]any{"capabilities": contracts, "fleet": fleet, "matchedCapabilityCount": total, "truncated": total > len(matches), "note": "A current authorized operation discovery and Fleet advertisement snapshot. Contracts establish available operations, not successful effects. Advertisements do not reserve capacity. The shortlist does not establish absence."})
	if err != nil {
		return err
	}
	args, _ := json.Marshal(map[string]any{"search": search})
	callID := "capability-discovery-" + workContextHash(search, value)
	messages, err := json.Marshal([]common.ChatMessage{
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: callID, Name: "workCapabilities", Arguments: string(args)}}},
		{Role: "tool", Name: "workCapabilities", ToolCallId: callID, Content: string(value)},
	})
	if err != nil {
		return err
	}
	call, err := parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": callID + "-" + BareShortId(run.RunId), "runId": run.RunId, "stepKey": run.StepKey, "kind": "note", "content": "Current capability discovery",
		"data": map[string]any{"capabilityEvidence": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(messages)},
	})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ContextWithTrainingEvidenceWrite(ctx)), "mutation "+call)
	return err
}
