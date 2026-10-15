package memql

// The `fleetPullRecommended` ACT (epic memql#5146, design D2).
//
// ===========================================================================
// ONE ACT, NOT A LIST OF BUTTONS
// ===========================================================================
// A person who has just paired a Mac Studio should not have to decide which
// four models to pull, in which order, and then press four buttons and watch
// four progress bars. The catalog knows which models suit a machine of this
// class and the engine knows the machine's class; the whole of what is left for
// a person to say is "yes, do that".
//
// So this act opens one pull record PER MODEL, in the recommended order,
// through the SAME path the per-model act uses. It does not invent a second
// pull mechanism: `createModelPull` writes the row, the claiming replica runs
// it, `workerModelPullStaleSweep` catches the abandoned ones. What this adds is
// the SET and the ORDER, which is exactly the part the catalog can answer and a
// person cannot.
//
// ===========================================================================
// WHAT IT REFUSES, AND WHY EACH REFUSAL IS BEFORE ANY WRITE
// ===========================================================================
// Every refusal here happens before the first row is written, so a partly-
// executed set is not a state this act can leave behind. That matters more than
// it looks: a person watching four bars, two of which will never move, cannot
// tell a queue from a failure.

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/core/num"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// RecommendedPullResultConcept is the canonical id of the act's answer.
const RecommendedPullResultConcept = "v1:worker:recommendedPullResult"

// ModelProfileConcept is the canonical id of the curated catalog.
const ModelProfileConcept = "v1:models:modelProfile"

// evaluateFleetPullRecommendedExpression serves the `fleetPullRecommended`
// builtin: resolve the machine under the caller, compute its recommended set,
// and open one pull per pullable entry in order.
func (e *MemQLEngine) evaluateFleetPullRecommendedExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	acting := strings.TrimSpace(actingUserFromContext(ctx))
	if acting == "" {
		return nil, modelPullRefusal{
			Code:    "not_your_machine",
			Message: "no machine of yours has that registration id",
		}
	}

	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	// The AUTHORIZED read, exactly as the per-model act does: the concept is
	// owner-tiered, so a machine that is not the caller's is not in the answer
	// and this function does not have to be trusted to check.
	machine, err := e.modelPullMachineFor(ctx, registrationId)
	if err != nil {
		return nil, err
	}

	unlock, err := e.lockModelDownloads(ctx, registrationId)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := e.modelSetupRow(ctx, registrationId)
	if err != nil {
		return nil, err
	}
	hardware, platformOs, installed := setupHardware(current)
	class := MachineClass(hardware)
	if class == ClassUnknown {
		return nil, modelPullRefusal{
			Code: "hardware_not_reported",
			Message: machineLabel(machine) + " has not reported what hardware it has, so the cluster " +
				"cannot say which models suit it. An older cockpit does not report one; upgrading it is " +
				"the fix, and nothing is wrong with the machine in the meantime.",
		}
	}
	if class == ClassUnsupported {
		return nil, modelPullRefusal{
			Code: "under_the_floor",
			Message: machineLabel(machine) + " is under the floor for local models -- Apple Silicon with " +
				"16 GB, or a discrete GPU with 8 GB. Nothing in the catalog would run usefully here, so " +
				"there is nothing to pull.",
		}
	}

	catalog, err := e.catalogProfiles(ContextWithFreshRead(ctx))
	if err != nil {
		return nil, err
	}
	set := RecommendedSet(class, hardware, platformOs, catalog)

	// The pullable half, in the set's own order. A blocked entry is REPORTED
	// rather than attempted: opening a pull for a model whose runtime the
	// machine does not have produces a failure on somebody's laptop for a
	// reason the page already knew.
	var plans []modelPullPlan
	plannedModels := make(map[string]bool)
	var blocked []map[string]any
	now := time.Now()
	for _, r := range set {
		if _, exists := installed["model:"+r.Profile.ModelId]; exists {
			continue
		}
		if !r.Pullable() {
			blocked = append(blocked, map[string]any{
				"modelId": r.Profile.ModelId,
				"level":   r.Level,
				"reason":  r.Blocked,
			})
			continue
		}
		plan, err := planModelPull(machine, acting, r.Profile.ModelId, now)
		if err != nil {
			// A machine-level refusal (offline, revoked) is the same for every
			// model in the set, so it is the answer for the whole act rather
			// than a per-model note -- and it is raised BEFORE any row is
			// written, which is what keeps a half-run set out of existence.
			return nil, err
		}
		// A model can serve several recommendation levels. Keep those levels
		// in the recommendation, but dispatch its download only once.
		if plannedModels[plan.Model] {
			continue
		}
		plannedModels[plan.Model] = true
		if err := e.validateModelDownload(ctx, registrationId, plan.Model); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	if hardware.DiskFreeBytes < modelDiskReserve {
		return nil, fmt.Errorf("The recommended downloads exceed free storage")
	}
	remaining := hardware.DiskFreeBytes - modelDiskReserve
	counted := map[string]bool{}
	for _, p := range catalog {
		if plannedModels[p.ModelId] && !counted[p.ModelId] {
			if p.SizeBytes <= 0 || uint64(p.SizeBytes) > remaining {
				return nil, fmt.Errorf("The recommended downloads exceed free storage")
			}
			remaining -= uint64(p.SizeBytes)
			counted[p.ModelId] = true
		}
	}
	if len(plans) == 0 {
		return nil, modelPullRefusal{
			Code: "nothing_to_pull",
			Message: "Nothing in the catalog can be pulled to " + machineLabel(machine) + " right now. " +
				blockedSummary(blocked),
		}
	}

	// Written under the CALLER'S actor so each row belongs to the person who
	// pressed the button, with internal origin because createModelPull is
	// @serverOnly -- the caller may own the row and still must not name the
	// replica that will act on it.
	writeCtx := auth.ContextWithInternalOrigin(ctx)
	opened := make([]any, 0, len(plans))
	for _, plan := range plans {
		if _, err := e.Execute(writeCtx, renderCreateModelPullCall(plan)); err != nil {
			// A failure PART WAY THROUGH is reported with what was opened
			// rather than rolled back. The rows already written name real
			// pulls that real replicas will run; deleting them would stop
			// work that is already under way, and pretending they do not
			// exist would leave a person watching bars the act denies opening.
			return nil, fmt.Errorf(
				"fleetPullRecommended: opened %d of %d pulls, then failed on %s: %w",
				len(opened), len(plans), plan.Model, err)
		}
		opened = append(opened, map[string]any{
			"pullId":       plan.PullId,
			"model":        plan.Model,
			"targetNodeId": plan.TargetNodeId,
		})
	}

	blockedAny := make([]any, 0, len(blocked))
	for _, b := range blocked {
		blockedAny = append(blockedAny, b)
	}
	return singleVirtualRow(RecommendedPullResultConcept, machine.RegistrationId, map[string]any{
		"workerId":     machine.RegistrationId,
		"machine":      machineLabel(machine),
		"machineClass": class,
		"pulls":        opened,
		// KEPT, not dropped. What the act did NOT do is half of what a person
		// needs to read, and a set that quietly pulled three of four models
		// with no note is indistinguishable from a catalog with three entries.
		"blocked":     blockedAny,
		"requestedAt": now.UTC().Format(time.RFC3339),
	})
}

// blockedSummary words the "nothing to pull" case from the reasons themselves.
//
// It names the FIRST reason rather than all of them: a person with an empty set
// has one thing to fix first, and a list of four sentences saying the machine
// is too small for four different models is one fact repeated.
func blockedSummary(blocked []map[string]any) string {
	if len(blocked) == 0 {
		return "The catalog recommends nothing for a machine of this class."
	}
	first, _ := blocked[0]["reason"].(string)
	if strings.TrimSpace(first) == "" {
		return "The catalog recommends nothing for a machine of this class."
	}
	return first
}

// machineHardwareFor reads one of the caller's registrations for its inventory
// and its reported platform.
//
// It re-reads rather than taking the fields off modelPullMachine, and the
// re-read is deliberate: modelPullMachine is the PULL's view of a machine --
// who owns it, which replica holds it, whether it is revoked -- and widening it
// with fields only the recommendation needs would make every pull carry them.
func (e *MemQLEngine) machineHardwareFor(ctx context.Context, registrationId string) (MachineHardware, string, error) {
	acting := strings.TrimSpace(actingUserFromContext(ctx))
	call := "workersForUser(ownerUserId: " + langparser.QuoteString(acting) + ")"
	res, err := e.Execute(ctx, call)
	if err != nil {
		return MachineHardware{}, "", fmt.Errorf("fleetPullRecommended: read your machines: %w", err)
	}
	for _, row := range modelPullRows(res.OutputPayload()) {
		rowId := mapString(row, "id")
		if rowId != registrationId && trimConceptPrefix(rowId) != registrationId {
			continue
		}
		platformOs := ""
		if platform, ok := row["platformInfo"].(map[string]any); ok {
			platformOs = mapString(platform, "os")
		}
		return HardwareFromRow(row["hardware"]), platformOs, nil
	}
	return MachineHardware{}, "", modelPullRefusal{
		Code:    "not_your_machine",
		Message: "no machine of yours has that registration id",
	}
}

// catalogProfiles reads the curated catalog.
//
// The catalog is `@rowAuthz(public, requiresIdentity)` -- any signed-in person
// reads it -- so this runs under the caller's own actor and needs no borrowed
// authority. That is the right shape: the recommendation is a fact about the
// caller's machine and the public catalog, and nothing here needs to see
// anything the caller could not.
func (e *MemQLEngine) catalogProfiles(ctx context.Context) ([]CatalogProfile, error) {
	res, err := e.Execute(ctx, "modelProfiles()")
	if err != nil {
		return nil, fmt.Errorf("fleetPullRecommended: read the model catalog: %w", err)
	}
	return catalogProfilesFromRows(modelPullRows(res.OutputPayload())), nil
}

// catalogProfilesFromRows is the projection, split out from the read so it can
// be tested as a function over values rather than through a fake engine that
// would record a query string and exercise no decision.
func catalogProfilesFromRows(rows []map[string]any) []CatalogProfile {
	out := make([]CatalogProfile, 0, len(rows))
	for _, row := range rows {
		modelId := mapString(row, "modelId")
		if modelId == "" {
			continue
		}
		// An INACTIVE or UNAVAILABLE entry is not a recommendation. Both are
		// soft states the catalog keeps deliberately -- the row stays so the
		// next operator can read why an id disappeared -- but neither is
		// something to pull, and an entry an operator removed coming back as a
		// recommendation would undo their decision on the next page load.
		//
		// A MISSING `active` key reads as TRUE, because the concept defaults it
		// that way and a row written before the flag existed has no key.
		// Reading absence as false would empty the catalog rather than fail --
		// the shape of outage nobody diagnoses, because every machine reports
		// "nothing recommended" and every machine is fine.
		if !boolField(row, "active", true) || boolField(row, "unavailable", false) {
			continue
		}
		out = append(out, CatalogProfile{
			ModelId:         modelId,
			Category:        mapString(row, "category"),
			Runtime:         mapString(row, "runtime"),
			Family:          mapString(row, "family"),
			Quant:           mapString(row, "quant"),
			MinMachineClass: mapString(row, "minMachineClass"),
			RecommendedFor:  stringListField(row, "recommendedFor"),
			OfferedOn:       stringListField(row, "offeredOn"),
			Flags:           stringListField(row, "flags"),
			Params:          int64Field(row, "params"),
			// narrowing: SATURATE, and the bare `int(...)` this replaces is
			// what CodeQL's go/incorrect-integer-conversion caught. `int` is
			// 32 bits on a 32-bit build, so a context window past MaxInt32
			// wrapped -- to a NEGATIVE, which sorts a corrupt catalog row
			// first and reads as a model with no context at all.
			//
			// TestEveryPayloadNarrowingCarriesAnAnswer does not cover this:
			// it sweeps the float64/int64 ARMS of a type switch, and this is
			// a conversion at a call site. The gate is not wrong, it is
			// scoped -- worth knowing before trusting it as the whole answer
			// on numeric narrowing.
			ContextWindow:   num.ClampInt64(int64Field(row, "contextWindow")),
			SizeBytes:       int64Field(row, "sizeBytes"),
			MemoryNeedBytes: int64Field(row, "memoryNeedBytes"),
			Source:          mapString(row, "source"),
			ReviewedAt:      mapString(row, "reviewedAt"),
			Notes:           mapString(row, "notes"),
		})
	}
	return out
}

func boolField(row map[string]any, key string, missing bool) bool {
	v, ok := row[key]
	if !ok || v == nil {
		return missing
	}
	b, ok := v.(bool)
	if !ok {
		return missing
	}
	return b
}

func stringListField(row map[string]any, key string) []string {
	list, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// narrowing: SATURATE -- every field read through here is an ORDERING.
//
// `params`, `contextWindow` and `sizeBytes` are compared and sorted, never
// summed, so a value past int64 belongs at the end of the order rather than
// wherever `int64(x)` lands -- which above the range is implementation-defined
// and answers with the integer indefinite value, a large NEGATIVE that would
// sort a corrupt catalog row FIRST.
func int64Field(row map[string]any, key string) int64 {
	switch n := row[key].(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return num.ClampFloat64ToInt64(n)
	}
	return 0
}
