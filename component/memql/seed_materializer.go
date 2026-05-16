package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unicode"

	"github.com/visionarys-io/memql/component/events"
)

// SeedMaterializer turns parsed seed declarations into materialized
// concept rows. Two trigger paths:
//
//  1. Start() runs a startup sweep: walks every registered seed and
//     materializes its rows. Global seeds become one row in _system;
//     perUser seeds iterate v1:identity:user and produce one row per
//     user. Idempotency comes from the underlying insert path's
//     deterministic-id semantics -- memql is time-series, so repeat
//     inserts with the same id stamp a new version but reads still
//     see one logical row.
//
//  2. After the sweep, Start() subscribes to
//     graph.node.created.*.v1:identity:user. When a new user lands,
//     the materializer re-runs the perUser sweep just for that one
//     user. Global seeds are skipped (they don't fan out per user).
//
// The materializer delegates row writes to existing concept mutations
// (mutationCreateAgent for v1:agents:agent, etc.) so the platform
// has a single canonical write path. The convention is:
//
//	use <namespace>.<conceptName>   ->  mutationCreate<ConceptName>
//
// Each seed body field maps to a same-named mutation arg. The
// concept's id field name follows the same case-corrected pattern:
//   - agent          ->  agentId
//   - agentAuthorization ->  agentAuthorizationId
//   - partition      ->  partitionId
//
// For @scope("perUser") the materializer computes the id as
// `<seedName>-<userId>` and passes it as `<conceptName>Id` along with
// `ownerUserId=<userId>`. The body must NOT declare its own id (the
// loader's compileSeedDecl rejects that).
type SeedMaterializer struct {
	engine   *MemQLEngine
	registry *SeedRegistry

	mu         sync.Mutex
	started    bool
	unsubscribe func()
}

// NewSeedMaterializer wires a materializer to the engine + registry.
// Both must be non-nil; the materializer doesn't usefully exist
// without a write path or a list of seeds.
func NewSeedMaterializer(engine *MemQLEngine, registry *SeedRegistry) *SeedMaterializer {
	return &SeedMaterializer{engine: engine, registry: registry}
}

// Start runs the startup sweep + sets up the runtime user-create
// hook. Idempotent (calling twice is a no-op). The sweep is
// synchronous; it returns once every seed has been materialized
// (or skipped with a logged error).
func (m *SeedMaterializer) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	if m.engine == nil || m.registry == nil {
		return fmt.Errorf("seed materializer: engine + registry must be non-nil")
	}

	logger := m.engine.Logger

	// Global seeds first -- they don't depend on user state.
	for _, def := range m.registry.All() {
		if def.Scope != "global" {
			continue
		}
		if err := m.materializeGlobal(ctx, def); err != nil {
			if logger != nil {
				logger.Warn("seed materializer: global materialization failed",
					"seed", def.Name, "concept", def.UseConcept, "error", err)
			}
			continue
		}
	}

	// Per-user seeds: walk the existing user list and materialize each.
	perUser := m.registry.PerUser()
	if len(perUser) > 0 {
		users, err := m.listUserIds(ctx)
		if err != nil {
			if logger != nil {
				logger.Warn("seed materializer: failed to list users for startup sweep",
					"error", err)
			}
		} else {
			for _, userId := range users {
				for _, def := range perUser {
					if err := m.materializePerUser(ctx, def, userId); err != nil {
						if logger != nil {
							logger.Warn("seed materializer: per-user materialization failed",
								"seed", def.Name, "userId", userId, "error", err)
						}
						continue
					}
				}
			}
		}
	}

	// Runtime hook: re-run perUser materialization on every new user.
	if bus := m.engine.EventBus(); bus != nil && len(perUser) > 0 {
		m.unsubscribe = bus.Subscribe(
			"graph.node.created.*.v1:identity:user",
			func(ev events.Event) {
				userId := extractUserIdFromEvent(ev)
				if userId == "" {
					return
				}
				// Use a fresh context so a slow handler doesn't get
				// canceled by Start's ctx going away. The bus's
				// own lifecycle wraps this.
				bgCtx := context.Background()
				for _, def := range m.registry.PerUser() {
					if err := m.materializePerUser(bgCtx, def, userId); err != nil {
						if logger != nil {
							logger.Warn("seed materializer: per-user runtime materialization failed",
								"seed", def.Name, "userId", userId, "error", err)
						}
					}
				}
			},
		)
	}

	m.started = true
	if logger != nil {
		logger.Info("seed materializer: startup sweep complete",
			"perUserSeeds", len(perUser),
			"globalSeeds", len(m.registry.All())-len(perUser))
	}
	return nil
}

// Stop tears down the runtime event subscription. Safe to call
// without Start.
func (m *SeedMaterializer) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unsubscribe != nil {
		m.unsubscribe()
		m.unsubscribe = nil
	}
	m.started = false
	return nil
}

// materializeGlobal writes one row for a @scope("global") seed. The
// seed body's `id` field provides the row id; remaining body fields
// flow through to the concept's mutationCreate<ConceptName>.
func (m *SeedMaterializer) materializeGlobal(ctx context.Context, def *SeedDefinition) error {
	idVal, ok := def.Body.fields["id"]
	if !ok || idVal.kind != seedString {
		return fmt.Errorf("global seed %q must declare a string `id` field", def.Name)
	}
	args := buildArgsFromBody(def.Body, def.UseConcept, idVal.str, "")
	return m.invokeCreateMutation(ctx, def.UseConcept, args)
}

// materializePerUser writes one row for a (perUser seed, user)
// pair. The row id is `<seedName>-<userId>`; the user id is stamped
// into `ownerUserId` so owner-keyed lookups resolve immediately.
func (m *SeedMaterializer) materializePerUser(ctx context.Context, def *SeedDefinition, userId string) error {
	if userId == "" {
		return fmt.Errorf("empty userId for per-user seed %q", def.Name)
	}
	rowId := def.Name + "-" + userId
	args := buildArgsFromBody(def.Body, def.UseConcept, rowId, userId)
	return m.invokeCreateMutation(ctx, def.UseConcept, args)
}

// invokeCreateMutation builds the canonical `mutationCreate<Concept>`
// invocation string and dispatches it through the engine.
func (m *SeedMaterializer) invokeCreateMutation(ctx context.Context, conceptName string, args map[string]any) error {
	mutationName := "mutationCreate" + ucFirst(conceptName)
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, string(argsJSON))
	if _, err := m.engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("%s: %w", mutationName, err)
	}
	return nil
}

// listUserIds runs queryAllUsers (or its equivalent) to enumerate
// existing v1:identity:user rows. Returns just the ids -- the
// materializer doesn't need any other fields.
//
// Per memql/CLAUDE.md the user concept lives at v1:identity:user
// (global scope); a generic concept-level query gets us the ids
// without depending on a product-layer query name.
func (m *SeedMaterializer) listUserIds(ctx context.Context) ([]string, error) {
	// `node(concept=="v1:identity:user")` returns every user row
	// (engine auto-filters to latest version per id). The shape is
	// raw row payloads; we just need the row ids.
	result, err := m.engine.Execute(ctx, `node(concept=="v1:identity:user")`)
	if err != nil {
		return nil, err
	}
	return extractRowIds(result), nil
}

// -----------------------------------------------------------------
// Body -> args lowering
// -----------------------------------------------------------------

// buildArgsFromBody walks a seed body and emits a map suitable for
// the concept's create mutation. The mapping:
//   - Top-level scalar fields -> string/int/float/bool/[]string args
//     by the same name. `id` is REPLACED by the materializer-computed
//     value below.
//   - Top-level nested blocks  -> object args (recursively walked).
//   - The synthetic id is stamped at `<conceptName>Id` (the convention
//     mutationCreate<X> uses).
//   - When `ownerUserId` is supplied (perUser scope), it's added to
//     the args map regardless of whether the body already has one --
//     for perUser seeds the user context wins.
func buildArgsFromBody(body seedBlock, conceptName string, idVal, ownerUserId string) map[string]any {
	args := make(map[string]any, len(body.fields)+len(body.nested)+2)
	for k, v := range body.fields {
		if k == "id" {
			// `id` from the body is consumed into the synthetic
			// conceptId arg below; don't pass it through twice.
			continue
		}
		args[k] = seedValueToInterface(v)
	}
	for k, v := range body.nested {
		args[k] = nestedBlockToMap(v)
	}
	args[conceptName+"Id"] = idVal
	if ownerUserId != "" {
		args["ownerUserId"] = ownerUserId
	}
	return args
}

// nestedBlockToMap converts a nested seedBlock into a map[string]any
// the mutation arg parser accepts for `object`-typed args.
func nestedBlockToMap(block seedBlock) map[string]any {
	out := make(map[string]any, len(block.fields)+len(block.nested))
	for k, v := range block.fields {
		out[k] = seedValueToInterface(v)
	}
	for k, v := range block.nested {
		out[k] = nestedBlockToMap(v)
	}
	return out
}

func seedValueToInterface(v seedValue) any {
	switch v.kind {
	case seedString:
		return v.str
	case seedInt:
		return v.intV
	case seedFloat:
		return v.floatV
	case seedBool:
		return v.boolV
	case seedStringArray:
		// Return a []any of strings so encoding/json emits a JSON array.
		arr := make([]any, len(v.stringsV))
		for i, s := range v.stringsV {
			arr[i] = s
		}
		return arr
	}
	return nil
}

// -----------------------------------------------------------------
// Event payload extraction
// -----------------------------------------------------------------

// extractUserIdFromEvent pulls the user row id out of a
// graph.node.created event. The payload shape is the materialized
// row; the engine puts the row id at `payload.id` (or top-level
// `id` in older shapes -- defensive read in both spots).
func extractUserIdFromEvent(ev events.Event) string {
	if ev.Payload == nil {
		return ""
	}
	if id, ok := ev.Payload["id"].(string); ok && id != "" {
		return id
	}
	if p, ok := ev.Payload["payload"].(map[string]any); ok {
		if id, ok := p["id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// -----------------------------------------------------------------
// Row-id extraction from an Execute result
// -----------------------------------------------------------------

// extractRowIds walks a memql Execute result and returns the `id`
// field of every row. Handles the two shapes the engine returns:
// a top-level []any of row maps, or a {"nodes":[...]} wrapper.
func extractRowIds(result any) []string {
	if result == nil {
		return nil
	}
	var rows []any
	switch v := result.(type) {
	case []any:
		rows = v
	case map[string]any:
		if nodes, ok := v["nodes"].([]any); ok {
			rows = nodes
		} else {
			rows = []any{v}
		}
	default:
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["id"].(string); ok && id != "" {
			out = append(out, id)
			continue
		}
		if p, ok := m["payload"].(map[string]any); ok {
			if id, ok := p["id"].(string); ok && id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// ucFirst returns the input string with its first rune upper-cased.
// Used to convert concept names ("agent" -> "Agent") for the
// mutation-name convention (mutationCreate<Concept>).
func ucFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

