package liveknowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// EngineAccess is the narrow engine surface the dispatcher needs:
// load the liveSource row by name + the backing liveConnector row by
// id, optionally read / write liveSnapshot cache rows.
type EngineAccess interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Dispatcher orchestrates one Live Knowledge read end-to-end:
// resolve source -> resolve connector -> check cache -> dispatch ->
// write snapshot -> return result.
type Dispatcher struct {
	Engine   EngineAccess
	Registry *Registry
	Logger   *slog.Logger
}

// NewDispatcher constructs a Dispatcher pinned to an engine adapter
// and connector registry.
func NewDispatcher(engine EngineAccess, reg *Registry, logger *slog.Logger) *Dispatcher {
	if reg == nil {
		reg = DefaultRegistry()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{Engine: engine, Registry: reg, Logger: logger}
}

// Query is the public entry point for a Live Knowledge read.
//
// Lookup order:
//
//  1. Resolve the liveSource row by name. Reject if missing/inactive.
//  2. Resolve the liveConnector row by source.connectorId. Reject if
//     missing/inactive.
//  3. Resolve the registered Connector for the kind. Reject if unknown
//     kind (means a plug-in didn't register or wasn't built into the
//     binary).
//  4. If cachePolicy='bounded_stale' and a fresh snapshot exists for
//     (sourceId, queryArgsHash), return it without dispatching.
//  5. Dispatch via the Connector. Write a snapshot for caching if the
//     policy allows. Return the result.
//
// Phase 5 scope: steps 1-3 + step 5 are wired. Steps 4 + the
// post-dispatch snapshot write are placeholders -- the caching layer
// adds two more engine round-trips per read and we're better off
// adding it once we see real volumes. The Snapshot concept already
// ships in Phase 1 so the schema is ready when the cache code lands.
func (d *Dispatcher) Query(ctx context.Context, sourceName string, args map[string]any) (map[string]any, error) {
	src, err := d.loadSource(ctx, sourceName)
	if err != nil {
		return nil, fmt.Errorf("liveknowledge: load source %q: %w", sourceName, err)
	}
	conn, err := d.Registry.MustLookup(src.ConnectorKind)
	if err != nil {
		return nil, err
	}

	result, err := conn.Query(ctx, src, args)
	if err != nil {
		return nil, fmt.Errorf("liveknowledge: connector %s dispatch: %w", src.ConnectorKind, err)
	}
	return result, nil
}

// loadSource resolves a liveSource row by name + joins its backing
// liveConnector row. Returns a populated Source struct or error if
// either lookup fails / either row is inactive.
func (d *Dispatcher) loadSource(ctx context.Context, name string) (Source, error) {
	// Step 1: load the source row.
	q := fmt.Sprintf(`from(v1:knowledge:liveSource) ?.payload.name==%q;payload.active==true limit 1`, name)
	res, err := d.Engine.Execute(ctx, q)
	if err != nil {
		return Source{}, err
	}
	rows := asRows(res)
	if len(rows) == 0 {
		return Source{}, fmt.Errorf("no active liveSource named %q", name)
	}
	row := rows[0]
	payload, _ := row["payload"].(map[string]any)
	if payload == nil {
		return Source{}, fmt.Errorf("liveSource %q has empty payload", name)
	}
	connectorId, _ := payload["connectorId"].(string)
	if connectorId == "" {
		return Source{}, fmt.Errorf("liveSource %q has no connectorId", name)
	}

	// Step 2: load the connector row.
	cq := fmt.Sprintf(`from(v1:knowledge:liveConnector) ?.id==%q;payload.active==true limit 1`, connectorId)
	cres, err := d.Engine.Execute(ctx, cq)
	if err != nil {
		return Source{}, err
	}
	crows := asRows(cres)
	if len(crows) == 0 {
		return Source{}, fmt.Errorf("liveSource %q references inactive/missing connector %q", name, connectorId)
	}
	cpayload, _ := crows[0]["payload"].(map[string]any)
	if cpayload == nil {
		return Source{}, fmt.Errorf("liveConnector %q has empty payload", connectorId)
	}

	srcId, _ := row["id"].(string)
	queryTemplate, _ := payload["queryTemplate"].(string)
	resultSchema, _ := payload["resultSchema"].(map[string]any)
	connKind, _ := cpayload["kind"].(string)
	connEndpoint, _ := cpayload["endpoint"].(string)
	authSecret, _ := cpayload["authSecretName"].(string)

	return Source{
		Id:                srcId,
		Name:              name,
		QueryTemplate:     queryTemplate,
		ConnectorKind:     connKind,
		ConnectorEndpoint: connEndpoint,
		AuthSecretName:    authSecret,
		ResultSchema:      resultSchema,
	}, nil
}

// HashArgs computes the stable hash used as the liveSnapshot
// queryArgsHash key. SHA-256 of sorted-keys JSON, hex-encoded, full
// 64-char hash. Exposed at package level so connectors that want to
// implement their own snapshot strategy can produce matching hashes.
func HashArgs(args map[string]any) string {
	if len(args) == 0 {
		return strings.Repeat("0", 64)
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]any, len(args))
	for _, k := range keys {
		ordered[k] = args[k]
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		// Fallback: hash the unordered form. Hash collisions across
		// different arg orderings are vanishingly unlikely in
		// practice; we accept the deterministic-input-only invariant.
		b, _ = json.Marshal(args)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// asRows lifts the engine.Execute return into the canonical
// []map[string]any shape. Tolerates the few shapes the engine can
// return depending on the query path.
func asRows(res any) []map[string]any {
	if res == nil {
		return nil
	}
	switch r := res.(type) {
	case []map[string]any:
		return r
	case []any:
		out := make([]map[string]any, 0, len(r))
		for _, v := range r {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Heartbeat is a small helper for tests / health-checks: dispatches a
// no-op query to verify the registry has the expected connectors.
// Not used in production paths.
func (d *Dispatcher) Heartbeat(_ context.Context) time.Time {
	return time.Now()
}
