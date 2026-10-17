package memql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func fleetSharingResource(args map[string]any) (string, error) {
	resource := strings.TrimSpace(stringArg(args, "resource"))
	if resource == "" {
		resource = "models"
	}
	if resource != "models" && resource != "workbench" {
		return "", modelPullRefusal{Code: "invalid_sharing_resource", Message: "choose model inference or fleet workbenches; host commands, apps and computer use cannot be shared"}
	}
	return resource, nil
}

func (m modelPullMachine) forSharingResource(resource string) modelPullMachine {
	if resource == "workbench" {
		m.SharingMode, m.SharedUserIds, m.SharedGroupIds = ParseMachineSharing(m.WorkbenchSharing)
		m.SharingPaused = false
		if paused, exists := m.WorkbenchSharing["paused"]; exists {
			m.SharingPaused = paused != false
		}
	}
	return m
}

func (e *MemQLEngine) evaluateFleetSetOwnerFeaturesExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if err := requireFleetSharingAccess(ctx); err != nil {
		return nil, err
	}
	registrationID := strings.TrimSpace(stringArg(args, "registrationId"))
	expected, ok := args["expectedUpdatedAt"].(string)
	if !ok {
		return nil, modelPullRefusal{Code: "features_changed", Message: "reload this machine's settings before changing its features"}
	}
	write := map[string]any{"registrationId": registrationID}
	for _, field := range []string{"headless", "apps", "computerUse"} {
		enabled, ok := args[field].(bool)
		if !ok {
			return nil, modelPullRefusal{Code: "invalid_feature_choice", Message: "each machine feature must be enabled or disabled"}
		}
		write[field] = enabled
	}
	call, err := langparser.RenderCall("setWorkerOwnerFeatures", write)
	if err != nil {
		return nil, err
	}
	// A fresh read and the engine's transactional row-version fence close the
	// cross-replica read/write gap. Heartbeats may race this write; retry those,
	// but never overwrite another owner's settings edit with an old draft.
	for attempt := 0; attempt < 3; attempt++ {
		machine, err := e.modelPullMachineFor(ctx, registrationID)
		if err != nil {
			return nil, err
		}
		if machine.RevokedAt != "" {
			return nil, modelPullRefusal{Code: "machine_revoked", Message: "this machine has been removed from the fleet"}
		}
		if mapString(machine.OwnerFeatures, "updatedAt") != expected {
			return nil, modelPullRefusal{Code: "features_changed", Message: "these feature settings changed elsewhere; review the current settings and try again"}
		}
		if machine.Version.IsZero() {
			return nil, fmt.Errorf("machine settings require a registration version")
		}
		fenced := ContextWithRowVersionFence(auth.ContextWithInternalOrigin(ctx), WorkerRegistrationConcept, registrationID, machine.Version)
		if _, err = e.Execute(fenced, call); errors.Is(err, ErrRowVersionChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return singleVirtualRow("v1:worker:ownerFeaturesResult", registrationID, map[string]any{"machineId": registrationID, "headless": write["headless"], "apps": write["apps"], "computerUse": write["computerUse"], "sentence": "Machine features saved."})
	}
	return nil, ErrRowVersionChanged
}
