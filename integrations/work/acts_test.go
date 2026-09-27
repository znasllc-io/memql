package work

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

// acts_test.go -- the harness the epic memql#5414 act tests share.
//
// # Why a fake SQL connector rather than a field to swap
//
// Every act reads a run's version history through StepVersions, which is a
// hand-rolled read over bunDB and admitRow -- the sweep's shape. Swapping the
// read out behind a test hook would leave the fold, the admission and the
// scan untested by every test here; standing a database/sql connector behind
// the real *bun.DB keeps all three on the path and needs no database. The one
// thing it cannot check is the SQL itself, which versions_db_test.go runs
// against Postgres.

// stepStore answers the version-history read from canned step row-versions.
type stepStore struct {
	mu      sync.Mutex
	rows    []storedStepRow
	queries []string
}

type storedStepRow struct {
	id      string
	at      time.Time
	payload map[string]any
}

func (s *stepStore) Connect(context.Context) (driver.Conn, error) { return &stepConn{s: s}, nil }
func (s *stepStore) Driver() driver.Driver                        { return nil }

// add stores one row-version.
func (s *stepStore) add(id string, at time.Time, payload map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, storedStepRow{id: id, at: at, payload: payload})
}

func (s *stepStore) queried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

type stepConn struct{ s *stepStore }

func (c *stepConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stepConn: Prepare is not implemented; bun formats its arguments into the query")
}
func (c *stepConn) Close() error              { return nil }
func (c *stepConn) Begin() (driver.Tx, error) { return nil, errors.New("stepConn: no transactions") }

// QueryContext answers the rows whose runId the formatted query names. bun
// inlines its arguments, so the run id forms arrive quoted in the text.
func (c *stepConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.queries = append(c.s.queries, query)
	var out []storedStepRow
	for _, r := range c.s.rows {
		runId, _ := r.payload["runId"].(string)
		if runId != "" && strings.Contains(query, "'"+runId+"'") {
			out = append(out, r)
		}
	}
	return &stepRows{rows: out}, nil
}

type stepRows struct {
	rows []storedStepRow
	next int
}

func (r *stepRows) Columns() []string { return []string{"id", "createdAt", "payload"} }
func (r *stepRows) Close() error      { return nil }
func (r *stepRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.next]
	r.next++
	raw, err := json.Marshal(row.payload)
	if err != nil {
		return err
	}
	dest[0], dest[1], dest[2] = row.id, row.at, raw
	return nil
}

// newActsIntegration is newTestIntegration plus a version history.
func newActsIntegration(t *testing.T) (*Integration, *recordingEngine, *stepStore) {
	t.Helper()
	i, eng := newTestIntegration(t)
	store := &stepStore{}
	db := bun.NewDB(sql.OpenDB(store), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	i.bunDB = func() *bun.DB { return db }
	return i, eng, store
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	actOwner  = "u-alice"
	actRunId  = "v1:work:run:r-acts"
	actGoalId = "v1:work:goal:g-acts"
)

// actRunRow is a finished run of three statements, the shape workRunForOwner
// answers with.
func actRunRow(status string, order ...string) map[string]any {
	if len(order) == 0 {
		order = []string{"fetch", "draft", "publish"}
	}
	steps := make([]any, 0, len(order))
	for _, k := range order {
		steps = append(steps, k)
	}
	return map[string]any{
		"id":                  actRunId,
		"ownerUserId":         "v1:identity:user:" + actOwner,
		"goalId":              actGoalId,
		"goalSignature":       "sig-weekly-report",
		"automationName":      "weeklyReport",
		"templateFingerprint": "fp-weekly",
		"templateConstructId": "v1:authoring:construct:weekly",
		"templateVersion":     "3",
		"input":               map[string]any{"week": "2026-39"},
		"inputFingerprint":    "in-39",
		"variables":           map[string]any{"week": "2026-39", "region": "emea"},
		"executionAuthority":  map[string]any{"expiresAt": "2026-10-01T00:00:00Z"},
		"mode":                modeLive,
		"status":              status,
		"stepOrder":           steps,
	}
}

// stepVersionAt is the time a fixture version's receipt landed: later
// versions land later, and an intent a second before its receipt.
func stepVersionAt(seq, version int) time.Time {
	return testNow.Add(time.Duration(version*100+seq) * time.Minute)
}

// addVersion stores one version of one step as its intent and its receipt.
// basis is the stored basis (nil for the pristine case the journal omits).
// extra fields land on the receipt.
func addVersion(s *stepStore, runId, key string, seq, version int, status string, basis work.Head, extra map[string]any) {
	id := "v1:work:step:" + bareRunId(runId) + "-" + key
	intent := map[string]any{
		"runId":          runId,
		"ownerUserId":    "v1:identity:user:" + actOwner,
		"key":            key,
		"seq":            seq,
		"stepType":       "function",
		"kind":           "reasoning",
		"call":           map[string]any{"construct": "function", "name": key},
		"status":         "running",
		"attempt":        version,
		"version":        version,
		"idempotencyKey": runId + ":" + key + ":" + itoa(version),
		"startedAt":      stepVersionAt(seq, version).Add(-time.Second).Format(time.RFC3339),
	}
	if basis != nil {
		intent["basis"] = basis.Object()
	}
	s.add(id, stepVersionAt(seq, version).Add(-time.Second), intent)
	receipt := map[string]any{}
	for k, v := range intent {
		receipt[k] = v
	}
	receipt["status"] = status
	receipt["result"] = map[string]any{"status": status, "result": key + " version " + itoa(version)}
	receipt["resultFingerprint"] = "fp-" + key + "-" + itoa(version)
	receipt["finishedAt"] = stepVersionAt(seq, version).Format(time.RFC3339)
	receipt["durationMs"] = 1000 + version
	for k, v := range extra {
		receipt[k] = v
	}
	s.add(id, stepVersionAt(seq, version), receipt)
}

// addPristineRun stores version 1 of every step, done, as an ordinary run
// writes it.
func addPristineRun(s *stepStore, runId string, order ...string) {
	for seq, key := range order {
		addVersion(s, runId, key, seq, 1, "done", nil, nil)
	}
}

// argsOf is the single recorded call to name, decoded.
func argsOf(t *testing.T, eng *recordingEngine, name string) map[string]any {
	t.Helper()
	return eng.callTo(t, name).Args(t)
}

// mutationsIn is every write the handler made, by construct name.
func mutationsIn(eng *recordingEngine) []recordedCall {
	var out []recordedCall
	for _, c := range eng.recorded() {
		if strings.HasPrefix(strings.TrimSpace(c.Query), "mutation ") {
			out = append(out, c)
		}
	}
	return out
}

// assertEveryCallParses hands every call the handler composed to the real
// parser (render_test.go's reasoning: a recording fake accepts anything).
func assertEveryCallParses(t *testing.T, eng *recordingEngine) {
	t.Helper()
	for _, c := range eng.recorded() {
		parseCall(t, c.Query)
	}
}

// ownerAdmits admits only rows owned by owner, the owned tier's answer.
func ownerAdmits(owner string) func(context.Context, memorynodes.MemoryNode) bool {
	return func(_ context.Context, n memorynodes.MemoryNode) bool {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		got, _ := p["ownerUserId"].(string)
		return strings.TrimPrefix(got, "v1:identity:user:") == owner
	}
}

func timeMinutes(n int) time.Duration { return time.Duration(n) * time.Minute }
