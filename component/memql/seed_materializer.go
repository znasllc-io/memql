package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/provenance"
)

// Seed-write resilience knobs (#1753). A full-stack release roll restarts every
// node-type's replicas at once; the combined connection demand momentarily
// exceeds the Postgres ceiling and seed materialization writes fail in a burst
// with SQLSTATE 53300 (remaining connection slots reserved). Rather than
// ERROR-drop the seed, treat 53300 as TRANSIENT and retry with a jittered,
// lengthening backoff so a momentary surge never loses a seed row. The window
// (~sum of base*attempt) comfortably outlasts the observed ~3s startup storm.
const (
	seedWriteMaxAttempts = 6
	seedWriteBaseBackoff = 300 * time.Millisecond
)

// devDefaultAvatarPersonaEnv names the dev-only knob that auto-assigns an
// avatar persona to the per-user Assistant when it is first materialized.
// When set (to a catalog persona NAME, e.g. "Ava"), materializePerUser stamps
// the assistant's avatarVendor + avatarPersonaId from the matching
// v1:agents:avatarPersona catalog row -- so a freshly-provisioned assistant
// renders its avatar with no manual stamping. Unset (production default) -> no
// behavior change. Deployment-agnostic: the persona id is resolved from the
// catalog at materialization time, never hardcoded (the minted ids vary per
// vendor account). `make dev-refresh` exports this so the avatar "just works"
// after login. See frontend#237.
const devDefaultAvatarPersonaEnv = "MEMQL_DEV_DEFAULT_AVATAR_PERSONA"

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
// (createAgent for v1:agents:agent, etc.) so the platform
// has a single canonical write path. The convention is:
//
//	use <namespace>.<conceptName>   ->  create<ConceptName>
//
// NOT mutationCreate<ConceptName>. Five comments in this file said that, and
// the code has always built `"create" + ucFirst(conceptName)` (see
// invokeCreateMutation). A seed author following the old wording would name
// their mutation mutationCreateFoo, and the materializer would look for
// createFoo and fail at runtime -- the same class as #2806, where docs named
// constructs that do not exist. Kind prefixes were abandoned in memql#2853;
// this convention never used one.
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

	// Skill catalog reconcile (#1459). The global pass above already
	// materializes every registered `seed skill` -- including the
	// carrier-RegisterTree'd product-pack skills on a carrier node,
	// since LoadUnifiedSeeds walks the overlay-inclusive dsl.Tree().
	// But a per-row materialize failure in the global pass is
	// logged-and-continued and never re-verified, which is how staging
	// ended up with the Assistant's capabilities.skillIds referencing
	// pack skills whose v1:agents:skill ROWS were absent --
	// ResolveSkills then silently dropped them and the realtime voice
	// model got zero UI primitives.
	// This pass re-asserts that every registered skill seed has a row,
	// creating only the MISSING ones (idempotent, create-only). Runs
	// BEFORE the assistant reconcile so the catalog rows the assistant's
	// skillIds point at exist first. Best-effort: a failure is logged,
	// boot continues.
	if _, err := m.reconcileSkillCatalog(ctx); err != nil && logger != nil {
		logger.Warn("seed materializer: skill catalog reconcile failed",
			"error", err)
	}

	// Assistant skillId reconcile (#1443). The per-user create above is
	// create-only (no-op on an existing assistant-<userId> row), so
	// existing assistant rows never pick up skillIds added to the seed
	// after they were minted -- the owner's staging assistant included.
	// Merge the canonical skillId set into every existing assistant row
	// here: idempotent, merge-only (never clobbers user-added skills or
	// user-editable fields like name/avatar). Runs after the per-user
	// sweep (so freshly-created rows are present) and after the global
	// pass (so assistantRole's lock/cap is already in the catalog).
	// Best-effort: a failure is logged, boot continues.
	if _, err := m.reconcileAssistantSkills(ctx); err != nil && logger != nil {
		logger.Warn("seed materializer: assistant skill reconcile failed",
			"error", err)
	}

	// Materialization complete -- now refresh the AgentRegistry from
	// the rows we just wrote (plus any user-created agents already
	// in the DB). The materialized rows are the canonical source of
	// truth; the registry is the in-memory cache the agent("name")
	// builtin reads from. Pass the system-actor ctx so the
	// underlying allAgents call doesn't fail on the empty
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
// flow through to the concept's create<ConceptName>. The
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
	m.applyDevDefaultAvatarPersona(ctx, def, args)
	ctx = provenance.ContextWithProvenance(ctx, provenance.Seed(def.Name))
	return m.invokeCreateMutation(ctx, def.UseConcept, args)
}

// applyDevDefaultAvatarPersona is the dev-only hook (#237) that stamps a
// default avatar persona onto the per-user Assistant at first materialization,
// so a freshly-provisioned assistant renders its avatar with no manual step.
// Gated on the MEMQL_DEV_DEFAULT_AVATAR_PERSONA env (a catalog persona NAME);
// unset -> no-op (production default). Only touches the `assistant` seed, and
// never overrides an avatar the body already carries. The persona id + vendor
// are resolved from the v1:agents:avatarPersona catalog at runtime so the value
// stays deployment-agnostic (minted ids vary per vendor account).
//
// Because createAgent is create-only (no-op on an existing row), this
// only takes effect on the first create; a later user choice (the #239 picker)
// is preserved across restarts. All cluster nodes read the same env value, so
// concurrent materializer runs inject identical fields -- no last-writer race.
func (m *SeedMaterializer) applyDevDefaultAvatarPersona(ctx context.Context, def *SeedDefinition, args map[string]any) {
	if def == nil || def.Name != "assistant" {
		return
	}
	personaName := strings.TrimSpace(os.Getenv(devDefaultAvatarPersonaEnv))
	if personaName == "" {
		return
	}
	// Respect an avatar the seed/body already declared.
	if existing, ok := args["avatarPersonaId"].(string); ok && strings.TrimSpace(existing) != "" {
		return
	}

	vendor, personaId, ok := m.resolveAvatarPersonaByName(ctx, personaName)
	if !ok {
		if m.engine != nil && m.engine.Component != nil && m.engine.Logger != nil {
			m.engine.Logger.Warn("seed materializer: dev default avatar persona not found in catalog; assistant stays avatar-less",
				"env", devDefaultAvatarPersonaEnv, "personaName", personaName)
		}
		return
	}
	args["avatarVendor"] = vendor
	args["avatarPersonaId"] = personaId
	if m.engine != nil && m.engine.Component != nil && m.engine.Logger != nil {
		m.engine.Logger.Info("seed materializer: stamped dev default avatar persona on assistant",
			"personaName", personaName, "vendor", vendor, "avatarPersonaId", personaId)
	}
}

// resolveAvatarPersonaByName looks up a v1:agents:avatarPersona catalog row by
// its display name and returns its (vendor, personaId). The catalog is
// materialized in the global pass before any perUser seed, so it is present by
// the time the assistant is materialized. Returns ok=false on lookup failure or
// no match.
func (m *SeedMaterializer) resolveAvatarPersonaByName(ctx context.Context, name string) (vendor, personaId string, ok bool) {
	if m.engine == nil {
		return "", "", false
	}
	result, err := m.engine.Execute(systemActorContext(ctx), `query avatarPersonas()`)
	if err != nil {
		if m.engine != nil && m.engine.Component != nil && m.engine.Logger != nil {
			m.engine.Logger.Warn("seed materializer: avatarPersonas failed resolving dev default avatar",
				"error", err)
		}
		return "", "", false
	}
	for _, row := range materializerResultRows(result) {
		if rowStringField(row, "name") != name {
			continue
		}
		v := rowStringField(row, "vendor")
		pid := rowStringField(row, "personaId")
		if pid == "" {
			continue
		}
		if v == "" {
			v = "anam"
		}
		return v, pid, true
	}
	return "", "", false
}

// materializerResultRows normalizes an Execute result into row maps. Shape-
// projected queries (avatarPersonas uses avatarPersonaFull) surface flat
// rows via OutputPayload; a raw bundle is walked as a fallback.
func materializerResultRows(result *ExecuteResult) []map[string]any {
	if result == nil {
		return nil
	}
	switch v := result.OutputPayload().(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if mrow, ok := item.(map[string]any); ok {
				out = append(out, mrow)
			}
		}
		return out
	}
	if result.Bundle != nil {
		out := make([]map[string]any, 0, len(result.Bundle.Nodes))
		for _, n := range result.Bundle.Nodes {
			if n == nil {
				continue
			}
			row := map[string]any{}
			if p := n.GetPayload(); p != nil {
				for k, val := range p.AsMap() {
					row[k] = val
				}
			}
			out = append(out, row)
		}
		return out
	}
	return nil
}

// rowStringField reads a string field off a row map, probing the bare key and
// a nested payload map (shape output may nest under payload).
func rowStringField(row map[string]any, field string) string {
	if v, ok := row[field].(string); ok {
		return v
	}
	if p, ok := row["payload"].(map[string]any); ok {
		if v, ok := p[field].(string); ok {
			return v
		}
	}
	return ""
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
	conceptId, err := m.canonicalSeedConceptID(def)
	if err != nil {
		// #1608: cannot resolve the canonical concept id (e.g. the
		// signature-form binding left UseNamespace empty AND the bare
		// name doesn't resolve). Surface LOUDLY rather than firing a
		// malformed "v1::<concept>" lookup that silently fails; the
		// deterministic-id fallback keeps seeding correct.
		if m.engine != nil && m.engine.Component != nil && m.engine.Logger != nil {
			m.engine.Logger.Error("seed materializer: cannot resolve canonical concept id for per-user seed dedup; using deterministic id (dedup guard skipped)",
				"seed", def.Name, "concept", def.UseConcept, "namespace", def.UseNamespace, "error", err.Error())
		}
		return deterministicPerUserSeedId(def, userId), nil
	}
	query := buildPerUserDedupQuery(def, userId, conceptId)
	result, err := m.engine.Execute(systemActorContext(ctx), query)
	if err != nil {
		if m.engine != nil && m.engine.Component != nil && m.engine.Logger != nil {
			// #1385: capture the EXACT built query string on the WARN so
			// a staging parse failure is self-diagnosing -- the next run
			// reveals the failing input (e.g. a divergent concept id or a
			// lexer-tripping bare colon RHS) instead of just the opaque
			// "unexpected token after expression" error. Benign fallback
			// (fallback id == canonical deterministic id), so logging the
			// query carries no security concern (no user secrets).
			m.engine.Logger.Warn("seed materializer: dedup lookup failed; falling back to deterministic id",
				"seed", def.Name, "userId", userId, "query", query, "error", err)
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
// EVERY colon-bearing RHS -- the concept id, the canonical
// `v1:identity:user:<uuid>` userId, and the provenance name -- goes
// through dslStringLiteral, which JSON-quotes the value. Quoting is
// what keeps a colon-bearing id from being parsed as a bare expression
// that trips the expression parser with
// "unexpected token after expression: \":\"" (issue #420 / #1385).
//
// The `concept==` RHS used to be left BARE here on the theory that the
// lexer special-cases the colon-bearing concept literal (and that
// quoting it "would break that grammar"). That theory is wrong on both
// counts (#1385):
//
//  1. The lexer (component/language/parser/lexer.go scanIdentifier,
//     the #1118 change) only folds a `:` into an identifier when the
//     NEXT char is a LETTER; a `:` followed by a non-letter (digit,
//     hyphen, EOF, ...) terminates the token. So a bare colon-bearing
//     RHS only parses by luck of "every concept-id segment starts with
//     a letter" -- a fragile invariant. `v1:agents:agent` happens to
//     satisfy it; a future concept id that doesn't would split into
//     stray `:` tokens and reproduce the exact staging error.
//  2. Quoting the concept RHS is SAFE: `concept=="v1:agents:agent"`
//     parses green via parseViaLangparser AND still resolves in
//     extractConceptFromExpression (executor_filter.go), which reads
//     node.Value.(string) -- identical for bare vs quoted RHS.
//
// Routing the concept RHS through the same dslStringLiteral helper as
// the #420 userId fix removes the fragile bare-RHS dependency entirely.
//
// Pure (no engine / no IO) so the regression test can assert the built
// query parses without standing up an engine.
// conceptId is the canonical concept id the caller resolved (e.g.
// "v1:agents:agent"). It is passed in -- rather than rebuilt from
// def.UseNamespace here -- because the signature-form seed binding leaves
// UseNamespace empty, and `fmt.Sprintf("v1:%s:%s", "", "agent")` produced
// the malformed "v1::agent" whose registry lookup always failed, silently
// disabling the dedup guard and spamming a fallback WARN every seed sweep
// (#1608). The caller (lookupOrMintPerUserId) now resolves the real
// canonical id via the concept registry.
func buildPerUserDedupQuery(def *SeedDefinition, userId, conceptId string) string {
	return fmt.Sprintf(
		`concept==%s; payload.ownerUserId==%s; provenance.name==%s`,
		dslStringLiteral(conceptId), dslStringLiteral(userId), dslStringLiteral(def.Name),
	)
}

// canonicalSeedConceptID resolves a per-user seed's bound concept to its
// canonical id (e.g. "v1:agents:agent"). When the parser captured the
// namespace (Form B `use <ns>.<concept>`) it is honoured directly; for
// the signature form, where UseNamespace is empty (#1608), the bare
// concept name is resolved through the registry by the same trailing-
// segment match the loader's ConceptResolver uses. Returns a loud error
// (never a malformed "v1::<concept>") so the caller can fall back to the
// deterministic id rather than firing a doomed registry lookup.
func (m *SeedMaterializer) canonicalSeedConceptID(def *SeedDefinition) (string, error) {
	if def == nil || def.UseConcept == "" {
		return "", fmt.Errorf("seed has no concept binding")
	}
	var reg memoryNodes.Registry
	if m != nil && m.engine != nil {
		reg = m.engine.Concepts()
	}
	// Explicit namespace (Form B): honour it, verifying against the
	// registry when one is available.
	if def.UseNamespace != "" {
		id := fmt.Sprintf("v1:%s:%s", def.UseNamespace, def.UseConcept)
		if reg == nil {
			return id, nil
		}
		if _, err := reg.Get(id); err == nil {
			return id, nil
		}
		// Fall through to bare-name resolution: the declared namespace
		// didn't resolve, so trust the concept name over a stale binding.
	}
	if reg == nil {
		return "", fmt.Errorf("no concept registry to resolve bare concept %q", def.UseConcept)
	}
	id, err := NewConceptResolver(reg).resolveBareConceptName(def.UseConcept)
	if err != nil {
		return "", fmt.Errorf("resolve bare concept %q: %w", def.UseConcept, err)
	}
	return id, nil
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

// invokeCreateMutation builds the canonical `create<Concept>` invocation
// string and dispatches it through the engine.
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
	mutationName := "create" + ucFirst(conceptName)
	rendered, err := renderArgsCallList(args)
	if err != nil {
		return fmt.Errorf("render args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, rendered)
	if err := m.executeSeedWriteWithRetry(ctx, query); err != nil {
		return fmt.Errorf("%s: %w", mutationName, err)
	}
	return nil
}

// executeSeedWriteWithRetry runs a seed mutation, treating a connection-slot
// storm (SQLSTATE 53300, the all-nodes full-stack-roll surge in #1753) as a
// TRANSIENT condition: it backs off with a jittered, lengthening delay and
// retries instead of dropping the seed, so a momentary surge never loses a row.
// Non-transient errors return immediately. The seed writes are idempotent at
// the row level (global seeds key off a stable id; per-user off (concept,
// owner, provenance)), so a retried write converges rather than duplicating.
func (m *SeedMaterializer) executeSeedWriteWithRetry(ctx context.Context, query string) error {
	var logger *slog.Logger
	if m.engine != nil {
		logger = m.engine.Logger
	}
	return retryOnDBSurge(ctx, logger, func() error {
		_, err := m.engine.Execute(systemActorContext(ctx), query)
		return err
	})
}

// retryOnDBSurge runs `do`, retrying only on a transient DB connection surge
// with a jittered, lengthening backoff. Extracted from the engine call so it is
// unit-testable with a scripted closure (the engine itself is a concrete type).
func retryOnDBSurge(ctx context.Context, logger *slog.Logger, do func() error) error {
	var err error
	for attempt := 1; attempt <= seedWriteMaxAttempts; attempt++ {
		if err = do(); err == nil || !isTransientDBSurge(err) {
			return err
		}
		if attempt == seedWriteMaxAttempts {
			break
		}
		// Jittered linear backoff: base*attempt + [0,base) jitter, so the
		// ~20-pod surge doesn't re-converge into a synchronized retry storm.
		backoff := time.Duration(attempt)*seedWriteBaseBackoff + time.Duration(rand.Int63n(int64(seedWriteBaseBackoff)))
		if logger != nil {
			logger.Warn("seed materializer: transient DB connection surge; backing off and retrying",
				"attempt", attempt, "maxAttempts", seedWriteMaxAttempts, "backoff", backoff.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

// isTransientDBSurge reports whether a DB error is the connection-slot
// exhaustion of a full-stack roll (SQLSTATE 53300) or a transient connection
// drop -- worth backing off and retrying rather than ERROR-dropping the seed.
// Conservative: a validation/logic error must NOT be retried.
func isTransientDBSurge(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "53300") || // too_many_connections / remaining connection slots reserved
		strings.Contains(s, "remaining connection slots are reserved") ||
		strings.Contains(s, "too many clients already") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "the database system is") // starting up / in recovery
}

// renderArgsObject emits a MemQL object literal with bare identifier
// keys: `{name: "Sofia", capabilities: {avatar: true, tools: ["uiClick"]}}`.
// Recurses into nested maps and arrays so the seed body's nested
// blocks (capabilities, providerConfig, triggerBehavior) round-trip
// cleanly through the mutation arg parser.
//
// Keys are emitted in sorted order so failure-mode logs are stable
// across runs (helpful when diffing a failed materialization).
// renderArgsCallList renders the named-args invocation body `k: v, ...`
// (Story 9 / #2335): renderArgsObject without the outer object braces, since
// the kind-prefixed call form `name(k: v, ...)` drops the legacy `{...}`
// wrapper (which the parser now rejects). Empty args render "" -> `name()`.
func renderArgsCallList(args map[string]any) (string, error) {
	obj, err := renderArgsObject(args)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(obj, "{"), "}"), nil
}

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
// canonical `activeUsers` query (dsl/identity/queries.memql).
// Using the named query (instead of a raw `node(concept==...)` call)
// goes through the same path every other reader uses, with proper
// shape resolution, global-scope handling, and trait filtering --
// we only materialize per-user agents for active users, not
// soft-deleted ones.
//
// systemActorContext is required because activeUsers's
// underlying executor stamps an actor on the request envelope; an
// empty actor produces a "no actor found in context" failure even
// for read paths that touch global concepts.
func (m *SeedMaterializer) listUserIds(ctx context.Context) ([]string, error) {
	// #2883: activeUsers is @serverOnly. systemActorContext supplies the actor
	// the executor needs; the origin stamp is the separate question of which
	// CHANNEL the call arrived on, and seed materialization is server-side Go.
	result, err := m.engine.Execute(auth.ContextWithInternalOrigin(systemActorContext(ctx)), `query activeUsers()`)
	if err != nil {
		return nil, fmt.Errorf("activeUsers: %w", err)
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
//     create<X> uses).
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
// mutation-name convention (create<Concept>).
func ucFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
