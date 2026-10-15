package memql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// ModelSetup is owner-selected routing, persisted on the registration so both
// the provider selector and the stream-holding replica read the same decision.
// Absence preserves automatic routing. An explicit empty setup serves nothing.
type ModelSetup struct {
	Revision string
	Choices  map[string]string
}

var ModelPurposes = []string{"conversation", "structured", "tools", "vision", "embeddings", "audioIn", "audioOut", "imageGen"}

const ModelSetupContextLimit = 32768

func ModelSetupFromRow(value any) ModelSetup {
	row, _ := value.(map[string]any)
	s := ModelSetup{Revision: mapString(row, "revision"), Choices: map[string]string{}}
	if choices, ok := row["choices"].(map[string]any); ok {
		for k, v := range choices {
			if model, ok := v.(string); ok {
				s.Choices[k] = model
			}
		}
	}
	return s
}

// Primary purpose is deterministic for compound calls. All requested model
// capabilities are still checked by the existing routing gates.
func ModelPurpose(structured, tools, vision, embeddings, audioIn, audioOut, imageGen bool) string {
	switch {
	case embeddings:
		return "embeddings"
	case audioIn:
		return "audioIn"
	case audioOut:
		return "audioOut"
	case imageGen:
		return "imageGen"
	case vision:
		return "vision"
	case tools:
		return "tools"
	case structured:
		return "structured"
	default:
		return "conversation"
	}
}

func (s ModelSetup) Allows(model, purpose string, contextWindow int) bool {
	return s.Revision == "" || (s.Choices[purpose] == model && contextWindow <= ModelSetupContextLimit)
}

func profilePurposes(p CatalogProfile) []string {
	var out []string
	if slices.Contains([]string{"text", "reasoning", "omni", "vision"}, p.Category) {
		out = append(out, "conversation")
	}
	for _, purpose := range ModelPurposes[1:] {
		if slices.Contains(p.Flags, purpose) || (purpose == "embeddings" && p.Category == "embeddings") {
			out = append(out, purpose)
		}
	}
	return out
}

// Fit uses bytes rather than the old class ladder, including machines larger
// than its top rung. Unknown residency is never presented as a confirmed fit.
func modelFit(p CatalogProfile, h MachineHardware, platform string) string {
	if p.Runtime == "cloud" {
		return "Cloud model · available through a provider"
	}
	if !h.Present() || UsableBytes(h) == 0 {
		return "Waiting for hardware details"
	}
	if len(p.OfferedOn) > 0 && !slices.Contains(p.OfferedOn, NormalizePlatform(platform)) {
		return "Not supported on this operating system"
	}
	if !h.HasRuntime(p.Runtime) {
		return "Needs " + p.Runtime
	}
	if p.MemoryNeedBytes <= 0 {
		return "Memory estimate not verified"
	}
	if uint64(p.MemoryNeedBytes) > UsableBytes(h) {
		return "Needs more model memory"
	}
	return ""
}

func advertisedPurpose(labels map[string]any, model, purpose string) bool {
	value, ok := labels["model:"+model].(string)
	if !ok {
		return false
	}
	attrs := map[string]string{}
	for _, part := range strings.Split(value, ",") {
		k, v, found := strings.Cut(part, "=")
		if found {
			attrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	yes := func(k string) bool { return attrs[k] == "1" || attrs[k] == "true" }
	if purpose == "conversation" {
		return !yes("embeddings") && !yes("imagegen") && !yes("audioin") && !yes("audioout")
	}
	return yes(strings.ToLower(purpose))
}

// Unique models consume the planning budget once, even when they fill several
// purposes. This is a conservative residency estimate, not a live RAM reading.
func validateModelChoices(choices map[string]string, catalog []CatalogProfile, h MachineHardware, platform string, labels map[string]any) (uint64, error) {
	profiles := map[string]CatalogProfile{}
	for _, p := range catalog {
		profiles[p.ModelId] = p
	}
	seen := map[string]bool{}
	var used uint64
	for purpose, model := range choices {
		if !slices.Contains(ModelPurposes, purpose) {
			return 0, fmt.Errorf("unknown model purpose %q", purpose)
		}
		if model == "" {
			continue
		}
		p, found := profiles[model]
		if !found {
			return 0, fmt.Errorf("%s needs a verified catalog profile before it can be selected", model)
		}
		if why := modelFit(p, h, platform); why != "" {
			return 0, fmt.Errorf("%s: %s", model, why)
		}
		if !slices.Contains(profilePurposes(p), purpose) || !advertisedPurpose(labels, model, purpose) {
			return 0, fmt.Errorf("%s is not installed with support for %s", model, purpose)
		}
		if seen[model] {
			continue
		}
		seen[model] = true
		need := uint64(p.MemoryNeedBytes)
		if need > UsableBytes(h)-used {
			return 0, fmt.Errorf("This setup exceeds the machine's model memory. Choose a smaller model or reuse one across purposes.")
		}
		used += need
	}
	return used, nil
}

func (e *MemQLEngine) modelSetupRow(ctx context.Context, registration string) (map[string]any, error) {
	acting := actingUserFromContext(ctx)
	if strings.TrimSpace(acting) == "" {
		return nil, fmt.Errorf("no machine of yours has that registration id")
	}
	res, err := e.Execute(ContextWithFreshRead(ctx), "workersForUser(ownerUserId: "+langparser.QuoteString(acting)+")")
	if err != nil {
		return nil, err
	}
	for _, row := range modelPullRows(res.OutputPayload()) {
		if trimConceptPrefix(mapString(row, "id")) == trimConceptPrefix(registration) && sameRowAuthzOwner(mapString(row, "ownerUserId"), acting) {
			return row, nil
		}
	}
	return nil, fmt.Errorf("no machine of yours has that registration id")
}

func setupHardware(row map[string]any) (MachineHardware, string, map[string]any) {
	platform, _ := row["platformInfo"].(map[string]any)
	labels, _ := row["labels"].(map[string]any)
	h := HardwareFromRow(row["hardware"])
	for key := range labels {
		if runtime, ok := strings.CutPrefix(key, "runtime:"); ok && !h.HasRuntime(runtime) {
			h.Runtimes = append(h.Runtimes, MachineRuntime{Name: runtime})
		}
	}
	return h, mapString(platform, "os"), labels
}

func (e *MemQLEngine) evaluateFleetModelSetup(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := stringArg(args, "registrationId")
	row, err := e.modelSetupRow(ctx, registration)
	if err != nil {
		return nil, err
	}
	catalog, err := e.catalogProfiles(ContextWithFreshRead(ctx))
	if err != nil {
		return nil, err
	}
	hw, platform, labels := setupHardware(row)
	setup := ModelSetupFromRow(row["modelSetup"])
	entries := make([]any, 0, len(catalog))
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].ModelId < catalog[j].ModelId })
	for _, p := range catalog {
		_, installed := labels["model:"+p.ModelId]
		blocked := modelFit(p, hw, platform)
		installBlocked := blocked
		if !installed && installBlocked == "" {
			if p.SizeBytes <= 0 {
				installBlocked = "Download size not verified"
			} else if hw.DiskFreeBytes < uint64(p.SizeBytes)+modelDiskReserve {
				installBlocked = "Not enough free storage"
			}
		}
		entries = append(entries, map[string]any{"modelId": p.ModelId, "category": p.Category, "runtime": p.Runtime, "purposes": profilePurposes(p), "memoryBytes": p.MemoryNeedBytes, "sizeBytes": p.SizeBytes, "contextWindow": p.ContextWindow, "installed": installed, "blocked": blocked, "installBlocked": installBlocked, "recommended": len(p.RecommendedFor) > 0 && blocked == "", "source": p.Source, "reviewedAt": p.ReviewedAt, "notes": p.Notes})
	}
	// These installation-wide records are owner-writable. This projection
	// exposes only the public model evidence and enabled flag after own-machine
	// admission; the research owner's identity never leaves this read scope.
	researchCtx := auth.ContextWithAccess(ContextWithFreshRead(ctx), auth.MaintenanceActor("weeklyModelCatalogResearch"))
	researchSummary, researchAt := "", ""
	reviews, err := e.Execute(researchCtx, "latestModelCatalogReview()")
	if err != nil {
		return nil, err
	}
	for _, review := range modelPullRows(reviews.OutputPayload()) {
		researchSummary, researchAt = mapString(review, "summary"), mapString(review, "reviewedAt")
		known := map[string]bool{}
		for _, p := range catalog {
			known[p.ModelId] = true
		}
		findings, _ := review["findings"].([]any)
		for _, value := range findings {
			finding, ok := value.(map[string]any)
			if !ok {
				continue
			}
			model := mapString(finding, "modelId")
			if model == "" || known[model] {
				continue
			}
			sources := stringListField(finding, "sources")
			source := ""
			if len(sources) > 0 {
				source = sources[0]
			}
			entries = append(entries, map[string]any{"modelId": model, "category": mapString(finding, "category"), "runtime": mapString(finding, "runtime"), "purposes": []string{}, "memoryBytes": 0, "sizeBytes": 0, "installed": false, "blocked": "Research candidate · needs verification", "installBlocked": "Needs verification before installation", "recommended": false, "source": source, "notes": mapString(finding, "summary")})
		}
	}
	researchEnabled := false
	settings, err := e.Execute(researchCtx, "modelCatalogResearchSettings()")
	if err != nil {
		return nil, err
	}
	for _, setting := range modelPullRows(settings.OutputPayload()) {
		researchEnabled = boolField(setting, "enabled", false)
	}
	bound, err := e.setupBoundEmbedder(ctx, labels)
	if err != nil {
		return nil, err
	}
	suggested := suggestModelChoices(catalog, hw, platform, labels)
	if bound != "" {
		suggested["embeddings"] = bound
	}
	used, validation := validateModelChoices(setup.Choices, catalog, hw, platform, labels)
	warning := ""
	if validation != nil {
		warning = validation.Error()
	}
	return singleVirtualRow("v1:worker:modelSetupView", registration, map[string]any{"revision": setup.Revision, "choices": setup.Choices, "catalog": entries, "usableBytes": UsableBytes(hw), "diskFreeBytes": hw.DiskFreeBytes, "usedBytes": used, "contextLimit": ModelSetupContextLimit, "warning": warning, "researchSummary": researchSummary, "researchAt": researchAt, "researchEnabled": researchEnabled, "suggestedChoices": suggested, "boundEmbeddingModel": bound})
}

// Preserve an installed model that currently owns the corpus vector space.
// This is a fresh database read, not a replica-local binding cache.
func (e *MemQLEngine) setupBoundEmbedder(ctx context.Context, labels map[string]any) (string, error) {
	binding, err := e.ReadEmbedderBinding(ctx)
	if errors.Is(err, ErrNoEmbedderBound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	model, local := strings.CutPrefix(binding.ProviderRef, "fleet:")
	if _, installed := labels["model:"+model]; local && installed {
		return model, nil
	}
	return "", nil
}

func (e *MemQLEngine) evaluateFleetSetModelSetup(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := stringArg(args, "registrationId")
	// Own-machine admission precedes the unscoped version witness.
	if _, err := e.modelSetupRow(ctx, registration); err != nil {
		return nil, err
	}
	ctx, err := e.fenceWriteTarget(ctx, WorkerRegistrationConcept, registration)
	if err != nil {
		return nil, err
	}
	row, err := e.modelSetupRow(ctx, registration)
	if err != nil {
		return nil, err
	}
	if mapString(row, "revokedAt") != "" {
		return nil, fmt.Errorf("Pair this machine again before changing its models")
	}
	previous := ModelSetupFromRow(row["modelSetup"])
	if previous.Revision != stringArg(args, "revision") {
		return nil, fmt.Errorf("The model setup changed. Reload it before saving.")
	}
	choices, ok := args["choices"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("model choices must be an object")
	}
	setup := ModelSetupFromRow(map[string]any{"choices": choices})
	if len(setup.Choices) != len(choices) {
		return nil, fmt.Errorf("model choices must contain model names")
	}
	catalog, err := e.catalogProfiles(ContextWithFreshRead(ctx))
	if err != nil {
		return nil, err
	}
	hw, platform, labels := setupHardware(row)
	if _, err := validateModelChoices(setup.Choices, catalog, hw, platform, labels); err != nil {
		return nil, err
	}
	bound, err := e.setupBoundEmbedder(ctx, labels)
	if err != nil {
		return nil, err
	}
	if bound != "" && setup.Choices["embeddings"] != bound {
		return nil, fmt.Errorf("Memory search uses %s. Keep that model until the corpus has been reindexed.", bound)
	}
	revision := id.NewShortId()
	call, err := langparser.RenderCall("setWorkerModelSetup", map[string]any{"registrationId": registration, "setup": map[string]any{"revision": revision, "choices": choices}})
	if err != nil {
		return nil, err
	}
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), call); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:worker:modelSetupResult", registration, map[string]any{"revision": revision})
}

const modelDiskReserve uint64 = 5 << 30

func (e *MemQLEngine) validateModelDownload(ctx context.Context, registration, model string) error {
	row, err := e.modelSetupRow(ctx, registration)
	if err != nil {
		return err
	}
	hw, platform, labels := setupHardware(row)
	res, err := e.Execute(ContextWithFreshRead(ctx), "modelPullsForWorker(workerId: "+langparser.QuoteString(registration)+")")
	if err != nil {
		return err
	}
	for _, pull := range modelPullRows(res.OutputPayload()) {
		if state := mapString(pull, "status"); state == "requested" || state == "running" {
			return fmt.Errorf("Wait for this machine's current download to finish")
		}
		ended, _ := time.Parse(time.RFC3339Nano, mapString(pull, "endedAt"))
		reported, _ := time.Parse(time.RFC3339Nano, hw.ReportedAt)
		if !ended.IsZero() && reported.Before(ended) {
			return fmt.Errorf("Waiting for refreshed storage details after the last download")
		}
	}
	if _, installed := labels["model:"+model]; installed {
		return fmt.Errorf("%s is already installed", model)
	}
	catalog, err := e.catalogProfiles(ContextWithFreshRead(ctx))
	if err != nil {
		return err
	}
	for _, p := range catalog {
		if p.ModelId != model {
			continue
		}
		if why := modelFit(p, hw, platform); why != "" {
			return fmt.Errorf("%s: %s", model, why)
		}
		if p.SizeBytes <= 0 || hw.DiskFreeBytes < uint64(p.SizeBytes)+modelDiskReserve {
			return fmt.Errorf("A verified download size and enough free storage are required")
		}
		return nil
	}
	return fmt.Errorf("Add a verified catalog profile before installing %s", model)
}

// A database session lock serializes download admission across BFF replicas.
// The durable pull row reserves the machine until refreshed storage is known.
func (e *MemQLEngine) lockModelDownloads(ctx context.Context, registration string) (func(), error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := e.database().DB.Conn(bounded)
	if err != nil {
		return nil, err
	}
	key := trimConceptPrefix(registration)
	if _, err := conn.ExecContext(bounded, "SELECT pg_advisory_lock(1297173581, hashtext($1))", key); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, err
	}
	return func() {
		release, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := conn.ExecContext(release, "SELECT pg_advisory_unlock(1297173581, hashtext($1))", key); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

// The suggested setup stays inside the same combined budget as a manual save.
// Prefer a suitable model already selected, then the curated strongest fit.
func suggestModelChoices(catalog []CatalogProfile, h MachineHardware, platform string, labels map[string]any) map[string]string {
	ranked := append([]CatalogProfile(nil), catalog...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if (len(ranked[i].RecommendedFor) > 0) != (len(ranked[j].RecommendedFor) > 0) {
			return len(ranked[i].RecommendedFor) > 0
		}
		if ranked[i].Params != ranked[j].Params {
			return ranked[i].Params > ranked[j].Params
		}
		return ranked[i].ModelId < ranked[j].ModelId
	})
	choices := map[string]string{}
	for _, purpose := range ModelPurposes {
		candidates := append([]CatalogProfile(nil), ranked...)
		sort.SliceStable(candidates, func(i, j int) bool {
			chosen := func(model string) bool {
				for _, v := range choices {
					if v == model {
						return true
					}
				}
				return false
			}
			return chosen(candidates[i].ModelId) && !chosen(candidates[j].ModelId)
		})
		for _, p := range candidates {
			choices[purpose] = p.ModelId
			if _, err := validateModelChoices(choices, catalog, h, platform, labels); err == nil {
				break
			}
			delete(choices, purpose)
		}
	}
	return choices
}
