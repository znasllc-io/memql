package work

import (
	"encoding/base64"
	"encoding/json"
	"sort"

	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Prefetched capability contracts are completed reads even without a model's
// tool round trip. Credit facts, not search terms, timestamps, shortlist order,
// or busy/idle polling. The durable per-run watermark and continuation cap still
// apply, so rediscovering the same facts cannot buy another work window.
func progressReceiptKeys(row map[string]any) []string {
	if key := progressReceiptKey(row); key != "" {
		return []string{key}
	}
	data := rowMap(row, "data")
	if rowString(row, "kind") != "note" || data["capabilityEvidence"] != true || rowString(data, "messagesEncoding") != "json-base64-v1" {
		return nil
	}
	encoded := rowString(data, "messages")
	if len(encoded) > 64*1024 {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	var messages []common.ChatMessage
	if json.Unmarshal(raw, &messages) != nil || len(messages) != 2 {
		return nil
	}
	first, result := messages[0], messages[1]
	if first.Role != "assistant" || len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "workCapabilities" || first.ToolCalls[0].ID == "" ||
		result.Role != "tool" || result.Name != "workCapabilities" || result.ToolCallId != first.ToolCalls[0].ID {
		return nil
	}
	var facts struct {
		Capabilities []struct{ Name, Kind, App string }
		Fleet        struct {
			Status     string
			Modalities map[string]struct{ Advertised *int }
		}
	}
	if json.Unmarshal([]byte(result.Content), &facts) != nil || len(facts.Capabilities) > 16 || len(facts.Fleet.Modalities) > 16 {
		return nil
	}
	var keys []string
	for _, contract := range facts.Capabilities {
		if contract.Name == "" || contract.Kind == "" {
			return nil
		}
		keys = append(keys, workstate.ArtifactHash(map[string]any{"capabilityContract": contract.Name, "kind": contract.Kind, "app": contract.App}))
	}
	if facts.Fleet.Status == "observed" {
		for modality, counts := range facts.Fleet.Modalities {
			if modality == "" || counts.Advertised == nil || *counts.Advertised < 0 {
				return nil
			}
			keys = append(keys, workstate.ArtifactHash(map[string]any{"fleetModality": modality, "advertised": *counts.Advertised}))
		}
	}
	sort.Strings(keys)
	return keys
}
