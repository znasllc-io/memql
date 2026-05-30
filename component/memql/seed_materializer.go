package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/provenance"
)

// seedMaterializerActor is the synthetic actor every materializer
// mutation runs as. Mutations require an actor (for createdBy
// stamping); the materializer runs from a Go startup goroutine with
// no auth context, so we mint one here. The `system:` prefix flips
// isSystemActor() true downstream so any validators that gate writes
// on "must be a system actor" let the seed-materializer through.
const seedMaterializerActor = "system:seedMaterializer"

// systemActorContext wraps ctx with a TokenInfo for the seed
// materializer's synthetic system actor. Every Execute call the
// materializer makes goes through this so mutations see a non-empty
// actor and don't fail with "no actor found in context".
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: seedMaterializerActor,
		Claims: map[string]any{
			"sub":  seedMaterializerActor,
			"role": "system",
		},
	})
}

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
//     graph.node.created.v1:identity:user. When a new user lands,
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

	mu          sync.Mutex
	started     bool
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

	allSeeds := m.registry.All()
	perUser := m.registry.PerUser()
	globalCount := len(allSeeds) - len(perUser)

	if logger != nil {
		logger.Info("seed materializer: startup sweep starting",
			"totalSeeds", len(allSeeds),
			"globalSeeds", globalCount,
			"perUserSeeds", len(perUser),
			"perUserNames", seedNames(perUser))
	}

	// Skill tier validation (memql#157): enforce skill.tier >=
	// max(tier across domainIds[]) before any row lands. A violation
	// brings boot down -- the catalog is self-validated and a mismatch
	// here means the cluster would serve a Tier-A skill that bundles
	// a Tier-C domain, which propagates wrong disclaimer / advisory
	// posture downstream.
	skillWarnings, skillErr := validateSkillTiers(m.registry)
	if skillErr != nil {
		if logger != nil {
			logger.Error("seed materializer: skill tier validation refused boot",
				"error", skillErr)
		}
		return skillErr
	}
	if len(skillWarnings) > 0 && logger != nil {
		for _, w := range skillWarnings {
			logger.Warn("seed materializer: skill catalog warning", "detail", w)
		}
	}

	// Global seeds first -- they don't depend on user state.
	for _, def := range allSeeds {
		if def.Scope != "global" {
			continue
		}
		if err := m.materializeGlobal(ctx, def); err != nil {
			if logger != nil {
				logger.Error("seed materializer: global materialization failed",
					"seed", def.Name, "concept", def.UseConcept, "error", err)
			}
			continue
		}
		if logger != nil {
			logger.Info("seed materializer: global row written",
				"seed", def.Name, "concept", def.UseConcept)
		}
	}

	// Per-user seeds: walk the existing user list and materialize each.
	if len(perUser) > 0 {
		users, err := m.listUserIds(ctx)
		if err != nil {
			if logger != nil {
				logger.Error("seed materializer: failed to list users for startup sweep",
					"error", err)
			}
		} else {
			if logger != nil {
				logger.Info("seed materializer: per-user sweep",
					"userCount", len(users),
					"perUserSeedCount", len(perUser),
					"rowsToWrite", len(users)*len(perUser))
			}
			rowsWritten := 0
			for _, userId := range users {
				for _, def := range perUser {
					if err := m.materializePerUser(ctx, def, userId); err != nil {
						if logger != nil {
							logger.Error("seed materializer: per-user materialization failed",
								"seed", def.Name, "userId", userId, "error", err)
						}
						continue
					}
					rowsWritten++
				}
			}
			if logger != nil {
				logger.Info("seed materializer: per-user sweep complete",
					"rowsWritten", rowsWritten,
					"expected", len(users)*len(perUser))
			}
		}
	}

	// Runtime hook: re-run perUser materialization on every new user.
	if bus := m.engine.EventBus(); bus != nil && len(perUser) > 0 {
		m.unsubscribe = bus.Subscribe(
			"graph.node.created.v1:identity:user",
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

	// Materialization complete -- now refresh the AgentRegistry from
	// the rows we just wrote (plus any user-created agents already
	// in the DB). The materialized rows are the canonical source of
	// truth; the registry is the in-memory cache the agent("name")
	// builtin reads from. Pass the system-actor ctx so the
	// underlying queryAllAgents call doesn't fail on the empty
	// actor.
	if agentReg := m.engine.Agents(); agentReg != nil {
		if _, err := agentReg.LoadFromRows(systemActorContext(ctx), m.engine, logger); err != nil && logger != nil {
			logger.Warn("seed materializer: AgentRegistry load failed",
				"error", err)
		}
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
// flow through to the concept's mutationCreate<ConceptName>. The
// per-seed provenance is stamped on ctx here so the engine's row-
// construction layer can persist {kind: "seed", name: <seedName>}
// as the row's intrinsic provenance field.
func (m *SeedMaterializer) materializeGlobal(ctx context.Context, def *SeedDefinition) error {
	idVal, ok := def.Body.fields["id"]
	if !ok || idVal.kind != seedString {
		return fmt.Errorf("global seed %q must declare a string `id` field", def.Name)
	}
	args := buildArgsFromBody(def.Body, def.UseConcept, idVal.str, "")
	ctx = provenance.ContextWithProvenance(ctx, provenance.Seed(def.Name))
	return m.invokeCreateMutation(ctx, def.UseConcept, args)
}

// materializePerUser writes one row for a (perUser seed, user)
// pair. Dedup keys off (concept, payload.ownerUserId, provenance.name)
// rather than a deterministic id string: the canonical id format
// already carries the concept in the middle position, and the seed
// name is captured in row provenance, so the row's shortId can be
// opaque. On first sweep for a (seed, user) we mint a UUID; on
// subsequent sweeps we look up the existing row and reuse its id so
// the time-series append-only insert lands as a new version of the
// same logical row instead of a parallel one.
func (m *SeedMaterializer) materializePerUser(ctx context.Context, def *SeedDefinition, userId string) error {
	if userId == "" {
		return fmt.Errorf("empty userId for per-user seed %q", def.Name)
	}
	rowId, err := m.lookupOrMintPerUserId(ctx, def, userId)
	if err != nil {
		return fmt.Errorf("dedup lookup: %w", err)
	}
	args := buildArgsFromBody(def.Body, def.UseConcept, rowId, userId)
	ctx = provenance.ContextWithProvenance(ctx, provenance.Seed(def.Name))
	return m.invokeCreateMutation(ctx, def.UseConcept, args)
}

// lookupOrMintPerUserId returns the existing perUser-seed row id for
// (def, userId) if one is already present, or a deterministic
// `<seedName>-<userShortId>` id otherwise. The deterministic-id path
// matches what the type-level docstring promises (see "For
// @scope("perUser") the materializer computes the id as
// `<seedName>-<userId>`" above) and is the load-bearing invariant
// for cluster boot: every node's startup sweep computes the SAME id
// for the same (seed, user) pair, so the racing inserts collapse to
// versions of one logical row via memql's (id, createdAt) primary
// key. The prior implementation here minted a fresh UUID via
// id.NewShortId() instead, which broke cluster boot -- 5 nodes
// racing produced 5 distinct rows for the same logical assistant.
// Tracked at #273.
//
// The lookup-first path is preserved so re-runs after the first
// successful materialization (subsequent startup sweeps, user-create
// re-fires) keep returning the row's original id even if that id
// pre-dates the deterministic-id contract (single-node deployments
// that wrote opaque UUIDs before this fix). Lookup failure falls
// through to the deterministic id rather than yet another UUID --
// any subsequent retry will then converge on that same value.
func (m *SeedMaterializer) lookupOrMintPerUserId(ctx context.Context, def *SeedDefinition, userId string) (string, error) {
	query := buildPerUserDedupQuery(def, userId)
	result, err := m.engine.Execute(systemActorContext(ctx), query)
	if err != nil {
		if m.engine.Logger != nil {
			m.engine.Logger.Warn("seed materializer: dedup lookup failed; falling back to deterministic id",
				"seed", def.Name, "userId", userId, "error", err)
		}
		return deterministicPerUserSeedId(def, userId), nil
	}
	if result != nil && result.Bundle != nil {
		for _, node := range result.Bundle.Nodes {
			if node != nil && node.Id != "" {
				return node.Id, nil
			}
		}
	}
	return deterministicPerUserSeedId(def, userId), nil
}

// buildPerUserDedupQuery builds the dedup-lookup query that finds an
// already-materialized perUser-seed row for (def, userId).
//
// The concept filter uses the canonical bare-RHS `concept==<id>` form
// (the lexer special-cases the colon-bearing concept literal there --
// quoting it would break that grammar). Every OTHER comparison RHS is
// a payload/provenance value that may carry colons -- the userId is
// the canonical `v1:identity:user:<uuid>` shape -- so it goes through
// dslStringLiteral, which JSON-quotes the value. That's the same
// canonical pattern the automations step-store uses for colon-bearing
// id comparisons (see component/automations/steps/steps.go's
// jsonString). Quoting is what keeps a colon-bearing id from being
// parsed as a bare expression that trips the expression parser with
// "unexpected token after expression: \":\"" (issue #420): the embedded
// `:` is only legal unquoted inside the concept-filter RHS, not in an
// arbitrary comparison value.
//
// Pure (no engine / no IO) so the regression test can assert the built
// query parses without standing up an engine.
func buildPerUserDedupQuery(def *SeedDefinition, userId string) string {
	conceptId := fmt.Sprintf("v1:%s:%s", def.UseNamespace, def.UseConcept)
	return fmt.Sprintf(
		`concept==%s; payload.ownerUserId==%s; provenance.name==%s`,
		conceptId, dslStringLiteral(userId), dslStringLiteral(def.Name),
	)
}

// dslStringLiteral renders a value as a DSL string literal safe to drop
// into any comparison RHS. JSON encoding gives correct escaping of
// quotes, backslashes, and control characters, and -- crucially for
// #420 -- wraps colon-bearing ids in double quotes so the embedded `:`
// is part of the string literal rather than a stray token the
// expression parser chokes on.
func dslStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// deterministicPerUserSeedId computes `<seedName>-<userShortId>` --
// the id every concurrent materializer run lands on for the same
// (seed, user) pair. The user's canonical prefix
// (`v1:identity:user:`) is stripped so the resulting shortId stays
// readable; the bare userId tail already carries enough entropy to
// uniquely identify the user inside the seed's namespace.
//
// Pure (no engine / no IO) so it's safe to call from the racing-
// inserts test that this issue's fix adds.
func deterministicPerUserSeedId(def *SeedDefinition, userId string) string {
	bare := strings.TrimPrefix(userId, "v1:identity:user:")
	return def.Name + "-" + bare
}

// invokeCreateMutation builds the canonical `mutationCreate<Concept>`
// invocation string and dispatches it through the engine.
//
// MemQL mutation calls accept object-literal args with BARE identifier
// keys (`{key: value, ...}`), not JSON-style quoted keys. Earlier
// versions of this code passed a raw json.Marshal output and the
// mutation parser silently rejected the call -- the materializer
// looked like it ran but no rows landed. renderArgsObject below emits
// the bare-key form recursively (nested objects, string arrays, etc.).
//
// The ctx gets wrapped in systemActorContext before Execute so the
// mutation's createdBy stamp resolves to the synthetic
// system:seedMaterializer actor instead of failing with "no actor
// found in context".
func (m *SeedMaterializer) invokeCreateMutation(ctx context.Context, conceptName string, args map[string]any) error {
	mutationName := "mutationCreate" + ucFirst(conceptName)
	rendered, err := renderArgsObject(args)
	if err != nil {
		return fmt.Errorf("render args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, rendered)
	if _, err := m.engine.Execute(systemActorContext(ctx), query); err != nil {
		return fmt.Errorf("%s: %w", mutationName, err)
	}
	return nil
}

// renderArgsObject emits a MemQL object literal with bare identifier
// keys: `{name: "Sofia", capabilities: {avatar: true, tools: ["uiClick"]}}`.
// Recurses into nested maps and arrays so the seed body's nested
// blocks (capabilities, providerConfig, triggerBehavior) round-trip
// cleanly through the mutation arg parser.
//
// Keys are emitted in sorted order so failure-mode logs are stable
// across runs (helpful when diffing a failed materialization).
func renderArgsObject(args map[string]any) (string, error) {
	var b strings.Builder
	b.WriteByte('{')

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		if err := renderArgsValue(&b, args[k]); err != nil {
			return "", fmt.Errorf("field %q: %w", k, err)
		}
	}
	b.WriteByte('}')
	return b.String(), nil
}

// renderArgsValue emits one value in MemQL-compatible literal syntax.
// Scalars (string, int, float, bool) use JSON literal form -- MemQL
// accepts the same shapes. Maps recurse via renderArgsObject. Arrays
// emit `[v1, v2, ...]` with each element re-rendered.
func renderArgsValue(b *strings.Builder, v any) error {
	switch val := v.(type) {
	case nil:
		b.WriteString("null")
		return nil
	case map[string]any:
		s, err := renderArgsObject(val)
		if err != nil {
			return err
		}
		b.WriteString(s)
		return nil
	case []any:
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := renderArgsValue(b, item); err != nil {
				return fmt.Errorf("array[%d]: %w", i, err)
			}
		}
		b.WriteByte(']')
		return nil
	}
	// Scalars: lean on encoding/json for safe quoting + numeric
	// formatting. MemQL accepts the same literal shapes for
	// strings ("..."), numbers, and bools.
	scalar, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal scalar: %w", err)
	}
	b.Write(scalar)
	return nil
}

// seedNames extracts the Name field from a slice of seed defs for
// log enumeration.
func seedNames(defs []*SeedDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		if d != nil {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

// listUserIds enumerates existing v1:identity:user rows via the
// canonical `queryActiveUsers` query (dsl/identity/queries.memql).
// Using the named query (instead of a raw `node(concept==...)` call)
// goes through the same path every other reader uses, with proper
// shape resolution, global-scope handling, and trait filtering --
// we only materialize per-user agents for active users, not
// soft-deleted ones.
//
// systemActorContext is required because queryActiveUsers's
// underlying executor stamps an actor on the request envelope; an
// empty actor produces a "no actor found in context" failure even
// for read paths that touch global concepts.
func (m *SeedMaterializer) listUserIds(ctx context.Context) ([]string, error) {
	result, err := m.engine.Execute(systemActorContext(ctx), `queryActiveUsers({})`)
	if err != nil {
		return nil, fmt.Errorf("queryActiveUsers: %w", err)
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
