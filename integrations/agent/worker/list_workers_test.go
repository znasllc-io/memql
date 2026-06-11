//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// TestHandleListWorkers_IncludesCapabilityDescriptor proves the
// agentworkerListWorkers payload carries the structured capability
// descriptor for workers that sent one at registration, and stays
// byte-compatible (no key at all) for workers that didn't
// (memql#1330).
func TestHandleListWorkers_IncludesCapabilityDescriptor(t *testing.T) {
	desc, err := workerservice.ParseCapabilityDescriptor(
		`{"platform":"darwin","displayServer":"quartz","guiAvailable":true,"actions":["screenshot","mouse_click"],"schemaVersion":1}`)
	if err != nil {
		t.Fatalf("parse descriptor: %v", err)
	}

	registry := workerservice.NewRegistry(nil, func() time.Time { return time.Unix(0, 0).UTC() })
	registry.Add(&workerservice.Worker{
		RegistrationId:       "reg-new",
		OwnerUserId:          "user-1",
		Name:                 "gui-build",
		Capabilities:         []string{workerservice.CapabilityHeadless, workerservice.CapabilityGUI},
		CapabilityDescriptor: desc,
	})
	registry.Add(&workerservice.Worker{
		RegistrationId: "reg-old",
		OwnerUserId:    "user-1",
		Name:           "legacy-build",
		Capabilities:   []string{workerservice.CapabilityHeadless},
	})

	integration := NewIntegration(nil, registry, nil, nil)
	nodes, err := integration.handleListWorkers(context.Background(), map[string]any{"ownerUserId": "user-1"}, 0)
	if err != nil {
		t.Fatalf("handleListWorkers: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 result node, got %d", len(nodes))
	}

	var entries []map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &entries); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 workers in payload, got %d", len(entries))
	}
	byId := map[string]map[string]any{}
	for _, e := range entries {
		byId[e["registrationId"].(string)] = e
	}

	withDesc, ok := byId["reg-new"]["capabilityDescriptor"].(map[string]any)
	if !ok {
		t.Fatalf("reg-new missing capabilityDescriptor: %v", byId["reg-new"])
	}
	if withDesc["displayServer"] != "quartz" || withDesc["guiAvailable"] != true {
		t.Fatalf("unexpected descriptor payload: %v", withDesc)
	}
	if withDesc["schemaVersion"] != float64(1) {
		t.Fatalf("unexpected schemaVersion: %v", withDesc["schemaVersion"])
	}
	actions, ok := withDesc["actions"].([]any)
	if !ok || len(actions) != 2 || actions[0] != "screenshot" {
		t.Fatalf("unexpected actions: %v", withDesc["actions"])
	}

	if _, present := byId["reg-old"]["capabilityDescriptor"]; present {
		t.Fatalf("legacy worker must not carry a capabilityDescriptor key")
	}
}
