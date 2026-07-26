package conformance

// harness_test.go -- the shared rig for the MCP conformance suite (memql#1715).
//
// One *Env carries a real, fully-loaded engine (the same New + LoadUnifiedConcepts
// + Init bootstrap the cluster runs) plus a real MCP Server head over it, so the
// suite exercises the genuine connector surface (tools/list + tools/call,
// including the run_query / run_mutation meta-tools) rather than poking engine
// internals.
//
// Postgres gating mirrors the sibling *_db_test.go pattern: probe the DSN; if no
// DB is reachable the DB-backed dimensions are reported SKIP (and the suite stays
// green for a laptop `go test ./...`). When a DB IS reachable the suite migrates
// it on connect via the production NewMemoryNodesDatabase + Start path -- so a
// blank CI Postgres comes up fully schema'd with no external migrate step.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/mcp"
	"github.com/znasllc-io/memql/component/memql"
)

// defaultDSN matches the readmerge / pii sibling DB tests so a developer with
// the dev cluster up gets the DB-backed dimensions for free.
const defaultDSN = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"

// ownerUID is the cluster-owner the suite acts as. Owner role clears per-row
// authz for the owned/admin constructs the lifecycle dimensions exercise.
const ownerUID = "user-conformance-owner"

// Env is the per-run conformance rig: a loaded engine, an MCP head over it, an
// owner-scoped context, and (when reachable) a migrated Postgres handle.
type Env struct {
	Eng      *memql.MemQLEngine
	Srv      *mcp.Server
	Ctx      context.Context
	DB       *bun.DB
	Registry memoryNodes.Registry
	HasDB    bool
}

// newEnv builds the rig once per suite run. The DSL load + engine Init is
// DB-free; the Postgres connect/migrate is best-effort and sets HasDB.
func newEnv(t *testing.T) *Env {
	t.Helper()

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts; DSL tree did not load")
	}

	db, hasDB := tryDB(t)

	var bunDB *bun.DB
	if hasDB {
		bunDB = db
	}

	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	// Quiet the provider loader (one WARN per provider when no secrets are
	// seeded -- expected here, not what the suite checks).
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree failed: %v", err)
	}

	// Wire the multi-step LogicRunner the same way app bootstrap does, so the
	// event-trigger dimension (#1706) can drive a multi-step event logic through
	// the genuine step registry. An in-memory event bus lets the logic's
	// publishEvent step run (no subscribers needed) instead of erroring.
	eng.SetEventBus(events.NewBus())
	eng.SetLogicRunner(automations.NewLogicRunner(eng, automationSteps.NewRegistry(), eng.Logger))

	srv := mcp.NewServer(
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		"memql-conformance", "test", eng,
		mcp.Config{ActingRole: string(auth.RoleOwner), ActingUser: ownerUID, Tier: mcp.DefaultTier},
	)

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: ownerUID, Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: ownerUID})

	return &Env{Eng: eng, Srv: srv, Ctx: ctx, DB: bunDB, Registry: registry, HasDB: hasDB}
}

// tryDB probes the configured DSN. When reachable it boots the production
// MemoryNodesDatabase (migrate-on-start) so a blank database is fully migrated,
// waits for readiness, and asserts the critical schema actually applied. When
// not reachable it returns (nil,false) and the DB-backed dimensions are skipped.
func tryDB(t *testing.T) (*bun.DB, bool) {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = defaultDSN
	}

	// Cheap reachability probe first so the no-DB path never blocks on the
	// migrate lifecycle.
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		t.Logf("conformance: no Postgres reachable at %s (%v) -- DB-backed dimensions will SKIP", safeDSN(dsn), err)
		return nil, false
	}
	_ = probe.Close()

	// Make sure NewMemoryNodesDatabase (which reads the env) sees the DSN we
	// just proved reachable, then migrate via the production lifecycle.
	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("conformance: NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(90 * time.Second):
		t.Fatalf("conformance: database did not become ready within 90s (migrations stuck?)")
	}
	if err := mnd.AssertCriticalSchema(context.Background()); err != nil {
		t.Fatalf("conformance: migrations did not apply cleanly: %v", err)
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("conformance: database ready but BunDB() is nil")
	}
	return bunDB, true
}

func safeDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.Index(dsn, "://"); j >= 0 && j+3 < i {
			return dsn[:j+3] + "***" + dsn[i:]
		}
	}
	return dsn
}

// ---------------------------------------------------------------------------
// MCP JSON-RPC helpers -- drive the real Server.Dispatch surface.
// ---------------------------------------------------------------------------

// rpc dispatches one JSON-RPC request through the real MCP head and returns the
// decoded `result` object. A protocol-level error fails the test.
func (e *Env) rpc(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("rpc marshal: %v", err)
	}
	out, ok := e.Srv.Dispatch(e.Ctx, raw)
	if !ok {
		t.Fatalf("rpc %s: Dispatch returned no response", method)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("rpc %s: decode response: %v\nraw=%s", method, err, out)
	}
	if resp.Error != nil {
		t.Fatalf("rpc %s: protocol error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result
}

// listTools enumerates the connector tool surface (name -> inputSchema) via
// tools/list -- the same surface an MCP client discovers.
func (e *Env) listTools(t *testing.T) map[string]map[string]any {
	t.Helper()
	res := e.rpc(t, "tools/list", nil)
	raw, _ := res["tools"].([]any)
	out := make(map[string]map[string]any, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		schema, _ := m["inputSchema"].(map[string]any)
		out[name] = schema
	}
	return out
}

// toolCall invokes a tool via tools/call and returns (decodedContent, isError).
// The MCP result wraps the construct output as a single text content block; the
// text is JSON, so it is decoded back into a Go value for assertions.
func (e *Env) toolCall(t *testing.T, name string, args map[string]any) (any, bool) {
	t.Helper()
	res := e.rpc(t, "tools/call", map[string]any{"name": name, "arguments": args})
	isErr, _ := res["isError"].(bool)
	text := firstText(res)
	if text == "" {
		return nil, isErr
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		// Not JSON (e.g. a plain error string) -- return the raw text.
		return text, isErr
	}
	return decoded, isErr
}

// firstText pulls the first text content block out of an MCP tools/call result.
func firstText(res map[string]any) string {
	content, _ := res["content"].([]any)
	for _, c := range content {
		m, _ := c.(map[string]any)
		if m == nil {
			continue
		}
		if txt, ok := m["text"].(string); ok {
			return txt
		}
	}
	return ""
}

// runMutation invokes a named mutation through the MCP run_mutation meta-tool
// (the connector dispatch path). Fails on an MCP-level error result.
func (e *Env) runMutation(t *testing.T, name string, args map[string]any) any {
	t.Helper()
	out, isErr := e.toolCall(t, "run_mutation", map[string]any{"name": name, "args": args})
	if isErr {
		t.Fatalf("run_mutation %s failed: %v", name, out)
	}
	return out
}

// runQuery invokes a named query through the MCP run_query meta-tool and returns
// the decoded result (canonical array per #1710).
func (e *Env) runQuery(t *testing.T, name string, args map[string]any) any {
	t.Helper()
	out, isErr := e.toolCall(t, "run_query", map[string]any{"name": name, "args": args})
	if isErr {
		t.Fatalf("run_query %s failed: %v", name, out)
	}
	return out
}

// runQueryServerSide executes a named query straight against the engine with
// INTERNAL origin, bypassing the MCP meta-tool.
//
// It exists for @serverOnly constructs (memql#2800 / #2883), which run_query
// correctly refuses because MCP is a client surface. A conformance assertion
// that needs such a query's ROWS -- rather than its reachability -- has to
// reach it the way its real callers do.
//
// Use this only where the property under test is the query's RESULT. Where the
// property is "a client may reach this", assert the refusal instead.
func (e *Env) runQueryServerSide(t *testing.T, query string) int {
	t.Helper()
	res, err := e.Eng.Execute(auth.ContextWithInternalOrigin(e.Ctx), query)
	if err != nil {
		t.Fatalf("server-side %s failed: %v", query, err)
	}
	if res == nil || res.Bundle == nil {
		return 0
	}
	return len(res.Bundle.Nodes)
}

// latestPayload reads the most-recent version of (concept, id) directly off the
// append-only MemoryNodes table -- the ground-truth read for sibling-field
// preservation + PII-scrub assertions.
func (e *Env) latestPayload(t *testing.T, conceptName, id string) map[string]any {
	t.Helper()
	var node memoryNodes.MemoryNode
	err := e.DB.NewSelect().Model(&node).
		Where("concept = ?", conceptName).
		Where("id = ?", id).
		Order("createdAt DESC").
		Limit(1).
		Scan(e.Ctx)
	if err != nil {
		t.Fatalf("read-back of %s/%s failed: %v", conceptName, id, err)
	}
	var p map[string]any
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		t.Fatalf("decode payload of %s/%s: %v", conceptName, id, err)
	}
	return p
}

// uniqueSuffix is deterministic-but-unique within a process so re-runs against a
// persistent local DB don't collide.
func uniqueSuffix(name string) string {
	return fmt.Sprintf("%s-%d", name, os.Getpid())
}

// asArray coerces a decoded MCP result to a slice, enforcing the canonical
// array envelope (#1710): a query result is ALWAYS a JSON array.
func asArray(t *testing.T, v any) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a canonical array envelope (#1710), got %T: %#v", v, v)
	}
	return arr
}

// ---------------------------------------------------------------------------
// Check registry + pass-rate emission.
// ---------------------------------------------------------------------------

// check is one conformance dimension. NeedsDB marks the DB-backed dimensions
// that SKIP without Postgres.
type check struct {
	Issue   string
	Dim     string
	NeedsDB bool
	Run     func(t *testing.T, e *Env)
}

// emitPassRate logs the published pass-rate metric and, in CI, appends a
// step-summary table. Pass-rate is over the dimensions that actually RAN
// (skips are reported separately so a no-DB laptop run isn't penalised).
func emitPassRate(t *testing.T, pass, fail, skip int, rows []string) {
	ran := pass + fail
	rate := 100.0
	if ran > 0 {
		rate = float64(pass) / float64(ran) * 100
	}
	t.Logf("CONFORMANCE PASS-RATE: %d/%d ran passed (%.1f%%); %d skipped (no DB)", pass, ran, rate, skip)
	for _, r := range rows {
		t.Logf("  %s", r)
	}

	summaryPath := os.Getenv("GITHUB_STEP_SUMMARY")
	if summaryPath == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## MCP Conformance Suite (memql#1715)\n\n")
	fmt.Fprintf(&b, "**Pass-rate: %d/%d ran passed (%.1f%%)** -- %d skipped (no DB)\n\n", pass, ran, rate, skip)
	fmt.Fprintf(&b, "| Issue | Dimension | Result |\n|---|---|---|\n")
	for _, r := range rows {
		// rows are "RESULT #issue/dim"
		parts := strings.SplitN(r, " ", 2)
		if len(parts) != 2 {
			continue
		}
		result := parts[0]
		idDim := strings.SplitN(parts[1], "/", 2)
		issue, dim := parts[1], ""
		if len(idDim) == 2 {
			issue, dim = idDim[0], idDim[1]
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", issue, dim, result)
	}
	f, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Logf("conformance: could not open GITHUB_STEP_SUMMARY: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Logf("conformance: could not write GITHUB_STEP_SUMMARY: %v", err)
	}
}
