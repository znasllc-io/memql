package seeder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/database"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/id"
)

var payloadPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// envVarPattern matches ${VAR_NAME} for environment variable substitution
var envVarPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// BunStore exposes the methods required by the concept seeder.
type BunStore interface {
	memoryNodes.Store
	BunDB() *bun.DB
}

// Runner coordinates applying concept seed files to the backing store.
type Runner struct {
	store    BunStore
	registry memoryNodes.Registry
	logger   *slog.Logger
}

// NewRunner builds a Runner using the provided store and concept registry.
func NewRunner(store BunStore, registry memoryNodes.Registry, logger *slog.Logger) (*Runner, error) {
	if store == nil {
		return nil, fmt.Errorf("concept seeder: store is required")
	}
	if registry == nil {
		registry = memoryNodes.DefaultRegistry()
	}

	return &Runner{
		store:    store,
		registry: registry,
		logger:   logger,
	}, nil
}

// Run loads every seed.memql file and applies missing records idempotently.
func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	bunDB := r.store.BunDB()
	if bunDB == nil {
		return fmt.Errorf("concept seeder: bun database handle is nil")
	}

	seeds, err := loadConceptSeeds()
	if err != nil {
		return err
	}

	if len(seeds) == 0 && r.logger != nil {
		r.logger.Debug("concept seeder: no seed files discovered")
		return nil
	}

	conceptIndex := make(map[string]*memoryNodes.Concept)
	if r.registry != nil {
		for _, concept := range r.registry.List() {
			if concept == nil {
				continue
			}
			name := strings.TrimSpace(concept.Name)
			if name == "" {
				continue
			}
			conceptIndex[name] = concept
		}
	}

	var errs []error
	for _, seed := range seeds {
		concept := conceptIndex[seed.ConceptName]
		if concept == nil {
			if r.logger != nil {
				r.logger.Info("concept seeder: seed skipped (concept not in active registry)",
					"concept", seed.ConceptName,
					"source", seed.SourcePath)
			}
			continue
		}

		if err := r.applyConceptSeed(ctx, bunDB, concept, seed); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *Runner) applyConceptSeed(ctx context.Context, bunDB *bun.DB, concept *memoryNodes.Concept, seed conceptSeed) error {
	if concept == nil {
		return fmt.Errorf("concept seeder: nil concept")
	}

	schemaId, err := concept.DefinitionSchemaId()
	if err != nil {
		return fmt.Errorf("concept seeder: resolve schema id for %s: %w", concept.Name, err)
	}

	defaultActor := strings.TrimSpace(seed.Actor)
	if defaultActor == "" {
		defaultActor = "system"
	}

	var errs []error
	for idx, record := range seed.Records {
		recordActor := strings.TrimSpace(record.Actor)
		if recordActor == "" {
			recordActor = defaultActor
		}
		if recordActor == "" {
			recordActor = "system"
		}

		matchers := record.Match
		if len(matchers) == 0 && strings.TrimSpace(record.ID) != "" {
			matchers = []seedMatch{{Field: "id", Value: record.ID}}
		}

		// Apply environment variable substitution to the seed payload
		seedPayload := substituteEnvVars(clonePayload(record.Payload))

		// Check if record exists and get its current payload
		existing, err := r.getExistingRecord(ctx, bunDB, concept, schemaId, matchers)
		if err != nil {
			errs = append(errs, fmt.Errorf("concept seeder: %s record %d existence check failed: %w", concept.Name, idx, err))
			continue
		}

		if existing.Exists {
			// Compare payloads - if they're the same, skip
			if payloadsEqual(seedPayload, existing.Payload) {
				if r.logger != nil {
					r.logger.Debug("concept seeder: record skipped (payload unchanged)",
						"concept", concept.Name,
						"record", record.ID,
						"source", seed.SourcePath)
				}
				continue
			}

			// Payloads differ - insert a new version (immutable pattern)
			if r.logger != nil {
				r.logger.Info("concept seeder: payload changed, inserting new version",
					"concept", concept.Name,
					"record", record.ID,
					"source", seed.SourcePath)
			}
		}

		// Global-scoped concepts live in the reserved SystemPartition so
		// every tenant sees the same topology/metadata. Without this
		// branch a seed like v1:cluster:nodeType/seed.memql lands in
		// `default:...` while the matching Create(ctx) calls from
		// automations land in `_system:...`, and the CLI's
		// queryClusterNodeTypes returns empty because the query path
		// filters to _system for global concepts.
		partition := id.DefaultPartition
		if concept.IsGlobal() {
			partition = id.SystemPartition
		}

		// Insert the record (either new or updated version). Stamp
		// system-bootstrap provenance so the row's intrinsic
		// reflects the legacy seed.memql sidecar path (distinct
		// from the new `seed` DSL primitive in dsl/agents/ etc.).
		seedCtx := provenance.ContextWithProvenance(ctx,
			provenance.System("conceptSeeder:"+concept.Name))
		if _, err := concept.Create(seedCtx, r.store, memoryNodes.CreateParams{
			Partition: partition,
			Actor:     recordActor,
			Payload:   seedPayload,
			ID:        record.ID,
		}); err != nil {
			errs = append(errs, fmt.Errorf("concept seeder: %s record %d insert failed: %w", concept.Name, idx, err))
			continue
		}

		if r.logger != nil {
			action := "inserted"
			if existing.Exists {
				action = "updated (new version)"
			}
			r.logger.Info(fmt.Sprintf("concept seeder: record %s", action),
				"concept", concept.Name,
				"record", record.ID,
				"source", seed.SourcePath)
		}
	}

	return errors.Join(errs...)
}

// existingRecord holds the result of checking for an existing record.
type existingRecord struct {
	Exists  bool
	Payload map[string]any
}

// getExistingRecord checks if a record exists and returns its latest payload.
func (r *Runner) getExistingRecord(ctx context.Context, bunDB *bun.DB, concept *memoryNodes.Concept, schemaId string, matchers []seedMatch) (*existingRecord, error) {
	if bunDB == nil {
		return nil, fmt.Errorf("concept seeder: bun database handle is nil")
	}
	if len(matchers) == 0 {
		return &existingRecord{Exists: false}, nil
	}

	// Query for the latest record (by createdAt DESC) to get the most recent version
	query := bunDB.NewSelect().
		TableExpr(fmt.Sprintf(`"%s" AS mn`, memoryNodes.MemoryNodesTable)).
		ColumnExpr("mn.payload").
		Where("mn.concept = ?", concept.Name).
		Where("mn.schema->>'$id' = ?", schemaId).
		OrderExpr(`mn."createdAt" DESC`).
		Limit(1)

	for _, matcher := range matchers {
		expr, args, err := buildMatchExpr(concept, matcher)
		if err != nil {
			return nil, err
		}
		query = query.Where(expr, args...)
	}

	var payloadJSON []byte
	if err := query.Scan(ctx, &payloadJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &existingRecord{Exists: false}, nil
		}
		return nil, err
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("concept seeder: unmarshal existing payload: %w", err)
	}

	return &existingRecord{
		Exists:  true,
		Payload: payload,
	}, nil
}

func buildMatchExpr(concept *memoryNodes.Concept, matcher seedMatch) (string, []any, error) {
	field := strings.TrimSpace(strings.ToLower(matcher.Field))
	if field == "" {
		return "", nil, fmt.Errorf("concept seeder: match field is required")
	}

	switch {
	case field == "id":
		raw := fmt.Sprintf("%v", matcher.Value)
		storageId := storageId(concept, raw)
		if storageId == "" {
			return "", nil, fmt.Errorf("concept seeder: match field id requires value")
		}
		return "mn.id = ?", []any{storageId}, nil
	case field == "concept":
		return "mn.concept = ?", []any{fmt.Sprintf("%v", matcher.Value)}, nil
	case field == "createdby":
		return `mn."createdBy" = ?`, []any{fmt.Sprintf("%v", matcher.Value)}, nil
	case field == "type":
		return "mn.type = ?", []any{fmt.Sprintf("%v", matcher.Value)}, nil
	case strings.HasPrefix(field, "payload."):
		pathStr := strings.TrimPrefix(strings.TrimSpace(matcher.Field), "payload.")
		segments := strings.Split(pathStr, ".")
		if len(segments) == 0 {
			return "", nil, fmt.Errorf("concept seeder: payload match requires path")
		}
		var normalized []string
		for _, segment := range segments {
			trimmed := strings.TrimSpace(segment)
			if trimmed == "" {
				return "", nil, fmt.Errorf("concept seeder: payload path segment is empty")
			}
			if !payloadPathSegmentPattern.MatchString(trimmed) {
				return "", nil, fmt.Errorf("concept seeder: payload path segment %q is invalid", trimmed)
			}
			normalized = append(normalized, trimmed)
		}
		jsonValue, err := json.Marshal(matcher.Value)
		if err != nil {
			return "", nil, fmt.Errorf("concept seeder: marshal match value: %w", err)
		}
		expr := fmt.Sprintf("mn.payload #> '{%s}' = (?::jsonb)", strings.Join(normalized, ","))
		return expr, []any{string(jsonValue)}, nil
	default:
		return "", nil, fmt.Errorf("concept seeder: unsupported match field %q", matcher.Field)
	}
}

func storageId(concept *memoryNodes.Concept, rawId string) string {
	if concept == nil {
		return ""
	}
	trimmed := strings.TrimSpace(rawId)
	if trimmed == "" {
		return ""
	}
	if id.HasPartition(trimmed) {
		return trimmed
	}
	// Global-scoped concepts store rows under SystemPartition; the
	// existence check must look there or it will always miss and the
	// seeder will insert a new "version" on every restart.
	partition := id.DefaultPartition
	if concept.IsGlobal() {
		partition = id.SystemPartition
	}
	if strings.HasPrefix(trimmed, concept.Name+":") {
		return partition + ":" + trimmed
	}
	return id.BuildNodeId(partition, concept.Name, trimmed)
}

type conceptSeed struct {
	ConceptName string
	SourcePath  string
	Actor       string
	Records     []seedRecord
}

type seedRecord struct {
	ID      string
	Actor   string
	Payload map[string]any
	Match   []seedMatch
}

type seedMatch struct {
	Field string
	Value any
}

func loadConceptSeeds() ([]conceptSeed, error) {
	var seeds []conceptSeed

	err := fs.WalkDir(database.Concepts, ".", func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		if !strings.EqualFold(d.Name(), "seed.memql") {
			return nil
		}

		dir := path.Dir(filePath)
		conceptName, err := memoryNodes.ConceptNameFromPath(dir)
		if err != nil {
			return fmt.Errorf("concept seeder: derive concept name for %s: %w", filePath, err)
		}

		data, err := fs.ReadFile(database.Concepts, filePath)
		if err != nil {
			return fmt.Errorf("concept seeder: read %s: %w", filePath, err)
		}

		parsed, err := ParseSeedMemQL(data)
		if err != nil {
			return fmt.Errorf("concept seeder: parse %s: %w", filePath, err)
		}

		if len(parsed.Records) == 0 {
			return nil
		}

		records := make([]seedRecord, 0, len(parsed.Records))
		for idx, rec := range parsed.Records {
			if len(rec.Payload) == 0 {
				return fmt.Errorf("concept seeder: %s records[%d] payload is required", filePath, idx)
			}

			id := strings.TrimSpace(rec.ID)
			matchers := make([]seedMatch, 0, len(rec.Match))
			for matchIdx, m := range rec.Match {
				field := strings.TrimSpace(m.Field)
				if field == "" {
					return fmt.Errorf("concept seeder: %s records[%d].match[%d] field is required", filePath, idx, matchIdx)
				}
				matchers = append(matchers, seedMatch{
					Field: field,
					Value: m.Value,
				})
			}

			if id == "" && len(matchers) == 0 {
				return fmt.Errorf("concept seeder: %s records[%d] must define an id or match filters", filePath, idx)
			}

			records = append(records, seedRecord{
				ID:      id,
				Actor:   strings.TrimSpace(rec.Actor),
				Payload: rec.Payload,
				Match:   matchers,
			})
		}

		seeds = append(seeds, conceptSeed{
			ConceptName: conceptName,
			SourcePath:  filePath,
			Actor:       parsed.Actor,
			Records:     records,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return seeds, nil
}

func clonePayload(source map[string]any) map[string]any {
	if len(source) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// substituteEnvVars recursively substitutes ${VAR} patterns with environment variable values.
func substituteEnvVars(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return payload
	}
	result := make(map[string]any, len(payload))
	for key, value := range payload {
		result[key] = substituteEnvVarsInValue(value)
	}
	return result
}

// substituteEnvVarsInValue substitutes ${VAR} patterns in any value type.
func substituteEnvVarsInValue(value any) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		return substituteEnvVarsInString(v)
	case map[string]any:
		return substituteEnvVars(v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = substituteEnvVarsInValue(item)
		}
		return result
	default:
		return value
	}
}

// substituteEnvVarsInString replaces ${VAR} patterns with os.Getenv(VAR).
func substituteEnvVarsInString(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1] // Strip ${ and }
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match // Keep original if env var not found
	})
}

// payloadsEqual compares two payloads for equality (deep comparison via JSON).
func payloadsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	aJSON, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bJSON, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aJSON) == string(bJSON)
}
