package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
	"google.golang.org/protobuf/types/known/structpb"
)

// This file is the REAL-ENGINE regression guard for #1454. The bug -- the
// voice tool-scope + persona reads using the RETIRED `?.` optional-chain query
// syntax the parser rejects (and reading the empty flat payload.tools instead
// of capabilities.skillIds) -- survived THREE prior fixes (#1444, #1452, #1387)
// because every test fed a MOCK engine / canned bundle: the query string was
// never run through the live parser, so its parse failure was invisible.
//
// These tests boot the ACTUAL embedded-DSL engine (the same Init path
// engine_load_smoke_test.go's TestEngineInitLoadsFullDSL exercises) and run the
// query strings the handler builds through the real parser. They FAIL against
// the pre-fix `?.` code and pass with the agentById fix.

// newRealDSLEngine boots a real MemQLEngine with the full embedded DSL loaded
// (no DB). The parser + named-query resolution are fully live; only DB-backed
// Execute / ResolveSkills need a database (Part B supplies one when reachable).
func newRealDSLEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	require.NotNil(t, registry)
	eng, err := memqlengine.New(nil)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}

// realParseResolver is a voiceParticipantResolver whose Execute runs the query
// string through the REAL engine's parser (mirroring engine.Execute's first
// step) before returning a result. A query that fails to parse -- the retired
// `?.` syntax does -- surfaces as an Execute error, exactly as it does in
// production, driving the handler down its fail-open / empty-persona path. A
// query that parses returns the seeded agent bundle. ResolveSkills is backed by
// the real catalog mapping so the resolved tool surface is asserted against the
// genuine skill->tool expansion.
//
// The CRUX -- the parse of the handler's query string -- goes through the real
// engine. This is the exact coverage the mock-based tests lacked.
type realParseResolver struct {
	engine      *memqlengine.MemQLEngine
	agentBundle *memqlv1.GraphBundle
	skillTools  map[string][]string // skillId -> resolved tool slugs (real catalog)
	parsedOK    []string            // queries that parsed cleanly
	parseFailed []string            // queries the real parser rejected
}

func (r *realParseResolver) CanonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error) {
	return r.engine.CanonicalizeIdValue(ctx, value, conceptType)
}

func (r *realParseResolver) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	// Run the handler's query through the REAL parser, exactly as
	// engine.Execute does before it touches the DB. The retired `?.` syntax
	// dies here with "unexpected token after expression: \"?.\"".
	if _, err := r.engine.Parse(query); err != nil {
		r.parseFailed = append(r.parseFailed, query)
		return nil, err
	}
	r.parsedOK = append(r.parsedOK, query)
	return &memqlengine.ExecuteResult{Bundle: r.agentBundle}, nil
}

func (r *realParseResolver) ResolveSkills(_ context.Context, skillIds []string) (memqlengine.SkillBundle, error) {
	seen := map[string]struct{}{}
	var tools []string
	for _, id := range skillIds {
		for _, tslug := range r.skillTools[id] {
			if _, dup := seen[tslug]; dup {
				continue
			}
			seen[tslug] = struct{}{}
			tools = append(tools, tslug)
		}
	}
	return memqlengine.SkillBundle{ToolSlugs: tools}, nil
}

// assistantSkillIds is the canonical per-user Assistant skill set (#1443 +
// operator suite), the exact list a 0.9.48 staging assistant carries.
var assistantSkillIds = []string{
	"todos", "notes", "calendar",
	"copresent-takeover", "copresent-guide", "copresent-ui", "copresent-canvas",
	"workbench-baseline", "operator-computer-use", "delegation-baseline",
}

// catalogSkillTools mirrors the embedded skill catalog's toolSlugs for the
// assistant skills (dsl/todos/skills.memql, dsl/notes/skills.memql,
// dsl/agents/skills/calendar.memql, etc.). copresent-takeover/guide carry the
// `copresent-takeover` slug, which ExpandCapabilitySlugs fans out to the 16
// operator primitives (uiClick/uiType/...). This is the mapping the real
// ResolveSkills returns off the seeded catalog; Part B (PG) verifies it against
// the live catalog.
var catalogSkillTools = map[string][]string{
	"todos":                 {"todosList", "todosCreate", "todosComplete", "todosUpdate"},
	"notes":                 {"notesList", "notesCreate", "notesUpdate", "notesSearch"},
	"calendar":              {"calendarList", "calendarFind", "calendarCreate", "calendarUpdate", "calendarDelete"},
	"copresent-takeover":    {"copresent-takeover"}, // expands to operator primitives via ExpandCapabilitySlugs
	"copresent-guide":       {"copresent-guide"},
	"copresent-ui":          {"similarTo"},
	"copresent-canvas":      {"canvasPublish"},
	"workbench-baseline":    {"workbench-use"}, // expands to workbench tools
	"operator-computer-use": {"workerComputer", "workerStatus", "requestComputerUseScope", "canvasPublish"},
	"delegation-baseline":   {"dispatchAgent"},
}

func assistantAgentBundle(agentId string) *memqlv1.GraphBundle {
	caps := map[string]any{"skillIds": toAny(assistantSkillIds)}
	payload, _ := structpb.NewStruct(map[string]any{
		"name":         "Sofia",
		"role":         "assistant",
		"roleSlug":     "assistant",
		"personality":  "warm and concise",
		"audioControl": "always_on",
		"capabilities": caps,
	})
	return &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: agentId, Concept: "v1:agents:agent", Payload: payload},
	}}
}

// TestVoiceAgentToolScopeRealEngine_ParsesAndResolves is the mandatory
// real-engine reproduction for #1454. It drives the ACTUAL handler functions
// (resolveAgentToolSlugsVia, resolveAgentSessionRecordVia) with a resolver that
// runs their query strings through the REAL embedded-DSL parser.
//
//   - Pre-fix (the `from(v1:agents:agent) ?.id==%s select …` raw queries): the
//     real parser rejects `?.` with "unexpected token after expression:
//     \"?.\"", every read errors, tool scope fails OPEN (found=false -> the
//     unscoped 517-tool registry) and persona comes back empty. The assertions
//     below FAIL.
//   - Post-fix (agentById): the named query parses, the agent's
//     capabilities.skillIds resolve to a scoped tool surface that CONTAINS
//     todosCreate/notesCreate/calendarList + the operator primitives, totalling
//     ~30 tools (neither 517 nor 0), and persona name="Sofia"/role populate.
func TestVoiceAgentToolScopeRealEngine_ParsesAndResolves(t *testing.T) {
	const realId = "v1:agents:agent:assistant-1c65cb0c"
	eng := newRealDSLEngine(t)
	ctx := context.Background()

	// --- tool scope ----------------------------------------------------------
	scopeResolver := &realParseResolver{
		engine:      eng,
		agentBundle: assistantAgentBundle(realId),
		skillTools:  catalogSkillTools,
	}
	rawSlugs, agentRole, found := resolveAgentToolSlugsVia(ctx, scopeResolver, realId, nil)
	require.True(t, found,
		"the agent-row read must PARSE and resolve (found=true). found=false means the query "+
			"parse-failed -- the #1454 `?.` bug -- and the session fails open to the 517-tool registry")
	require.Empty(t, scopeResolver.parseFailed,
		"the handler's agent query must parse cleanly through the real engine; "+
			"a parse failure here is the retired `?.` syntax regression")
	require.Equal(t, "assistant", agentRole,
		"the GA agent's role must be read from the row so the ListTools role gate runs against "+
			"the SCOPED agent's role, not the empty caller role -- without this, @allowedRoles(\"assistant\") "+
			"GA-only tools (produceArtifact, the operator/uiClick surface) are scoped in but role-filtered out")

	set, scoped := scopeSetFromSlugs(rawSlugs, found)
	require.True(t, scoped)

	// The scoped surface must carry the personal-productivity tools + operator
	// primitives -- a genuinely scoped set, not the unscoped registry.
	for _, want := range []string{
		"todosCreate", "notesCreate", "calendarList",
		"uiClick", "uiType", // copresent-takeover operator fan-out
	} {
		assert.Contains(t, set, want, "scoped tool set must contain %q", want)
	}
	// Neither the unscoped 517-registry nor an empty set: a real, bounded scope.
	assert.Greater(t, len(set), 14, "scoped count must be a real surface, not ~0/empty")
	assert.Less(t, len(set), 60, "scoped count must be a bounded per-agent surface, not the 517-tool registry")
	t.Logf("#1454 real-engine scoped tool count = %d (NOT 517, NOT 0): %v", len(set), sortedSetKeys(set))

	// --- persona -------------------------------------------------------------
	personaResolver := &realParseResolver{
		engine:      eng,
		agentBundle: assistantAgentBundle(realId),
		skillTools:  catalogSkillTools,
	}
	rec := resolveAgentSessionRecordVia(ctx, personaResolver, realId)
	require.Empty(t, personaResolver.parseFailed,
		"the persona query must parse cleanly through the real engine (#1454)")
	assert.Equal(t, "Sofia", rec.persona.name, "persona name must populate (voice falls back to nova when empty)")
	assert.NotEmpty(t, rec.persona.role, "persona role must populate")
}

// TestVoiceAgentRetiredSyntaxIsRejectedByRealParser is the focused
// reproduction: the EXACT pre-fix query strings, run through the real parser,
// must be rejected with the `?.` error from the live staging log -- while the
// post-fix agentById string parses. This is the assertion the prior
// mock-based tests could never make (they never invoked the parser).
func TestVoiceAgentRetiredSyntaxIsRejectedByRealParser(t *testing.T) {
	eng := newRealDSLEngine(t)
	const id = `"v1:agents:agent:assistant-1c65cb0c"`

	retired := []string{
		`from(v1:agents:agent) ?.id==` + id + ` select id, payload.tools`,
		`from(v1:agents:agent) ?.id==` + id + ` select id, payload.name, payload.role, payload.description, payload.personality, payload.audioControl, payload.videoControl`,
		`from(v1:agents:agent) ?.id==` + id + ` select id, payload.avatarVendor, payload.avatarPersonaId, payload.gender`,
		`from(v1:agents:agent) ?.id==` + id + ` select id, payload.audioControl`,
	}
	for _, q := range retired {
		_, err := eng.Parse(q)
		require.Error(t, err, "retired `?.` query must be rejected by the real parser: %s", q)
		assert.Contains(t, err.Error(), "?.",
			"the parse failure must be the optional-chain token the staging log showed")
	}

	// The fix's query parses cleanly.
	_, err := eng.Parse(queryAgentByIdQuery("v1:agents:agent:assistant-1c65cb0c"))
	require.NoError(t, err, "agentById -- the #1454 fix -- must parse through the real engine")
}

// spaceContextParseResolver is a voiceParticipantResolver that runs each query
// through the REAL parser (like realParseResolver) and returns a per-query
// bundle keyed by which named query the handler issued. It lets the #1470
// space-context resolution be exercised against the genuine querySpaceMeta /
// spaceParticipants strings -- proving they parse cleanly AND that the
// space-name/goal + participant-name extraction wires through end-to-end.
type spaceContextParseResolver struct {
	engine          *memqlengine.MemQLEngine
	spaceBundle     *memqlv1.GraphBundle
	participantsB   *memqlv1.GraphBundle
	parsedOK        []string
	parseFailed     []string
	sawSpaceMeta    bool
	sawParticipants bool
}

func (r *spaceContextParseResolver) CanonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error) {
	return r.engine.CanonicalizeIdValue(ctx, value, conceptType)
}

func (r *spaceContextParseResolver) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	// querySpaceMeta moved to the CoPresent pack in B2 (#2038) alongside the
	// `space` concept, so the pure (no-pack) engine under test can no longer
	// parse it -- the carrier engine that runs this resolver in production
	// (bff / cognition, both carrier-built) loads the pack and resolves it.
	// Recognize the pack-owned query here and return the canned space row
	// WITHOUT routing it through the core parser; the pack's own DSL load
	// tests prove querySpaceMeta parses. The CORE spaceParticipants
	// string is still validated through the real parser below (the live #1470
	// retired-syntax guard).
	if strings.Contains(query, "querySpaceMeta") {
		r.sawSpaceMeta = true
		return &memqlengine.ExecuteResult{Bundle: r.spaceBundle}, nil
	}
	if _, err := r.engine.Parse(query); err != nil {
		r.parseFailed = append(r.parseFailed, query)
		return nil, err
	}
	r.parsedOK = append(r.parsedOK, query)
	if strings.Contains(query, "spaceParticipants") {
		r.sawParticipants = true
		return &memqlengine.ExecuteResult{Bundle: r.participantsB}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func (r *spaceContextParseResolver) ResolveSkills(_ context.Context, _ []string) (memqlengine.SkillBundle, error) {
	return memqlengine.SkillBundle{}, nil
}

// TestVoiceSpaceContextRealEngine_ParsesAndResolves is the #1470 real-engine
// guard: it drives the actual resolveVoiceSpaceContextVia through the REAL
// embedded-DSL parser, proving the querySpaceMeta + spaceParticipants
// strings the handler builds parse cleanly (no retired syntax) and that the
// space name + goal statement + human participant names extract end-to-end.
//
// Cross-node note (#1470): the ack is built on the bff/cognition node and
// consumed by the voice-agent on a different node -- the space context must
// ride the ack proto, never implicit session state. This test exercises the
// server side of that hop (the resolution that fills the ack fields).
func TestVoiceSpaceContextRealEngine_ParsesAndResolves(t *testing.T) {
	const partitionId = "v1:cognition:space:demo-1c65cb0c"
	eng := newRealDSLEngine(t)
	ctx := context.Background()

	spacePayload, _ := structpb.NewStruct(map[string]any{
		"name": "Q3 Roadmap",
		"goal": map[string]any{"statement": "Plan the Q3 product roadmap"},
	})
	p1, _ := structpb.NewStruct(map[string]any{
		"participantType": "human",
		"displayName":     "Jose",
	})
	p2, _ := structpb.NewStruct(map[string]any{
		"participantType": "human",
		"userId":          "u-2", // no displayName -> falls back to userId
	})

	resolver := &spaceContextParseResolver{
		engine: eng,
		spaceBundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			{Id: partitionId, Concept: "v1:cognition:space", Payload: spacePayload},
		}},
		participantsB: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			{Id: "v1:cognition:participant:a", Concept: "v1:cognition:participant", Payload: p1},
			{Id: "v1:cognition:participant:b", Concept: "v1:cognition:participant", Payload: p2},
		}},
	}

	out := resolveVoiceSpaceContextVia(ctx, resolver, partitionId)

	require.Empty(t, resolver.parseFailed,
		"spaceParticipants must parse cleanly through the real engine (#1470); "+
			"querySpaceMeta is pack-owned after B2 (#2038) and validated by the pack's load tests")
	require.True(t, resolver.sawSpaceMeta, "the resolver must issue querySpaceMeta for the space row")
	require.True(t, resolver.sawParticipants, "the resolver must issue spaceParticipants for the humans")
	assert.Equal(t, "Q3 Roadmap", out.name, "space name must resolve from the space row")
	assert.Equal(t, "Plan the Q3 product roadmap", out.purpose, "space purpose must resolve from payload.goal.statement")
	assert.Equal(t, []string{"Jose", "u-2"}, out.participantNames,
		"human participant names must resolve (displayName, falling back to userId)")
}

func sortedSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// --- Part B: full PG-backed path (skips when no Postgres reachable) ----------

// openVoiceScopeTestDB opens the dev Postgres (or $MEMQL_DATABASE_DSN);
// skips the test when none is reachable, so CI (no PG) is unaffected while a
// local/integration run exercises the genuine Execute + ResolveSkills path.
func openVoiceScopeTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("no Postgres reachable for the #1454 full-path test: %v", err)
	}
	return db
}

// TestVoiceAgentToolScopeRealEngine_FullPathWithDB runs the COMPLETE #1454 path
// against a real engine + Postgres: it seeds an Assistant agent row +
// skill-catalog rows, then drives resolveAgentToolSlugsVia / the persona read
// through the live parser+executor+ResolveSkills (no canned bundles at all).
// Skips when no PG is reachable. This is the belt-and-suspenders that reports
// the EXACT live tool count.
func TestVoiceAgentToolScopeRealEngine_FullPathWithDB(t *testing.T) {
	db := openVoiceScopeTestDB(t)
	defer db.Close()
	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:#1454"))

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))

	// Seed the skill catalog (idempotent best-effort) so ResolveSkills has rows.
	seedSkillCatalogRows(ctx, t, db)

	// Seed an Assistant agent row.
	agentId := "v1:agents:agent:assistant-1454test"
	caps, _ := json.Marshal(map[string]any{"skillIds": assistantSkillIds})
	payload, _ := json.Marshal(map[string]any{
		"name":         "Sofia",
		"role":         "assistant",
		"roleSlug":     "assistant",
		"personality":  "warm and concise",
		"audioControl": "always_on",
		"capabilities": json.RawMessage(caps),
	})
	agent := &concept.MemoryNode{
		ID:         agentId,
		CreatedAt:  time.Now(),
		CreatedBy:  "test:#1454",
		Concept:    "v1:agents:agent",
		Type:       "agent",
		Schema:     json.RawMessage(`{}`),
		Payload:    payload,
		Metadata:   json.RawMessage(`{}`),
		Provenance: json.RawMessage(`{"source":"test"}`),
	}
	_, err = db.NewInsert().Model(agent).Exec(ctx)
	require.NoError(t, err, "seed assistant agent row")
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", agentId).Exec(context.Background())
	})

	// Drive the REAL handler path: agentById -> capabilities.skillIds ->
	// ResolveSkills, through the live engine.
	rawSlugs, _, found := resolveAgentToolSlugsVia(ctx, eng, agentId, eng.Logger)
	require.True(t, found, "full-path agent read must resolve (no `?.` parse failure)")
	set, scoped := scopeSetFromSlugs(rawSlugs, found)
	require.True(t, scoped)
	for _, want := range []string{"todosCreate", "notesCreate", "calendarList"} {
		assert.Contains(t, set, want, "live-catalog scoped set must contain %q", want)
	}
	assert.Greater(t, len(set), 0)
	assert.Less(t, len(set), 200, "must be a bounded per-agent surface, not the full registry")
	t.Logf("#1454 FULL-PATH (PG) scoped tool count = %d: %v", len(set), sortedSetKeys(set))

	rec := resolveAgentSessionRecordVia(ctx, eng, agentId)
	assert.Equal(t, "Sofia", rec.persona.name)
	assert.NotEmpty(t, rec.persona.role)
}

func seedSkillCatalogRows(ctx context.Context, t *testing.T, db *bun.DB) {
	t.Helper()
	for slug, tools := range catalogSkillTools {
		p, _ := json.Marshal(map[string]any{"slug": slug, "toolSlugs": tools})
		node := &concept.MemoryNode{
			ID:         "v1:agents:skill:" + slug,
			CreatedAt:  time.Now(),
			CreatedBy:  "test:#1454",
			Concept:    "v1:agents:skill",
			Type:       "skill",
			Schema:     json.RawMessage(`{}`),
			Payload:    p,
			Metadata:   json.RawMessage(`{}`),
			Provenance: json.RawMessage(`{"source":"test"}`),
		}
		// Best-effort upsert: the real catalog may already carry these rows
		// (the engine seeds them at Init), in which case the live row wins and
		// our insert conflict is harmless.
		_, _ = db.NewInsert().Model(node).On("CONFLICT DO NOTHING").Exec(ctx)
	}
}
