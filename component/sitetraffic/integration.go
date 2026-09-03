package sitetraffic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The DSL-callable half: `builtin siteTrafficInWindow`, declared in
// dsl/platform/builtins.memql and executed here.
//
// # Why a builtin rather than a query
//
// A DSL `query` reads the graph, and these rows are not in it: the aggregate
// is a dedicated relation TimescaleDB maintains, with no concept mapping and
// no filter path. `codeMetricsInWindow` is the cautionary case -- a `query`
// over a hypertable that has no Go provider behind it and answers nothing.
// The working shape for a read served from Go is the `@sdk` builtin whose
// executor returns synthetic nodes (`dataOrigins`, `modelCatalog`), and that
// is what this is.
//
// # Why it is registered on EVERY node type
//
// The builtin is declared in dsl/platform/builtins.memql, which every binary
// loads, and a capability present in the DSL and absent from the registry is
// a boot-time resolution failure. So the plug-in registers everywhere and the
// reader is built everywhere -- the same reason `customDomain` and `release`
// do. The WRITER is the opposite: it is wired only into an edge node, which
// is the only node type that serves a site.

// integrationName is the plug-in name. Spelled as a string literal in
// RegisterPlugin below, because the taxonomy gate scans source for the
// literal; TestSiteTrafficRegistrationNameIsTheLiteral asserts the two agree.
const integrationName = "siteTraffic"

// resultConcept is the concept the returned nodes carry -- declared in
// dsl/observability/concepts.memql, which is what makes the rows readable by
// a client that knows the concept rather than a shape somebody has to guess.
const resultConcept = "v1:observability:siteTraffic"

// Integration exposes the read capability.
type Integration struct {
	engine Engine
	bunDB  func() *bun.DB
	logger *slog.Logger
}

// NewIntegration wires the handles. The db getter is resolved lazily at call
// time, not at construction: a node's database handle is not necessarily up
// when plug-ins are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{engine: engine, bunDB: bunDB, logger: logger}
}

func init() {
	memql.RegisterPlugin("siteTraffic", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine, pctx.BunDB, pctx.Logger), nil
	})
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "inWindow",
			Description: "Read a deployable's traffic over a window, from the aggregate the edge's request log is folded into (epic memql#4906).",
			Handler:     i.handleInWindow,
			ArgsSchema: map[string]string{
				"siteIds":     "[]string (required) -- the deployables to read, bare ids",
				"bucket":      "string (required) -- 1m or 1h",
				"windowStart": "string (required) -- RFC3339, inclusive",
				"windowEnd":   "string (required) -- RFC3339, exclusive",
				"summary":     "boolean -- one row per deployable for the whole window instead of one per bucket",
			},
		},
	}
}

func (i *Integration) handleInWindow(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	q := Query{
		Bucket:  strings.TrimSpace(stringArg(args, "bucket")),
		Summary: boolArg(args, "summary"),
		SiteIds: stringsArg(args, "siteIds"),
	}
	var err error
	if q.WindowStart, err = timeArg(args, "windowStart"); err != nil {
		return nil, err
	}
	if q.WindowEnd, err = timeArg(args, "windowEnd"); err != nil {
		return nil, err
	}

	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	readings, err := NewReader(db, i.engine).Read(ctx, q)
	if err != nil {
		return nil, err
	}
	return nodesFor(readings), nil
}

// nodesFor projects readings onto synthetic nodes of v1:observability:siteTraffic.
//
// NO ROW IS SYNTHESIZED FOR A DEPLOYABLE WITH NO TRAFFIC. The absence IS the
// unmeasured answer, and a zero-filled row would be a measurement nobody made.
func nodesFor(readings []Reading) []memorynodes.MemoryNode {
	out := make([]memorynodes.MemoryNode, 0, len(readings))
	for _, r := range readings {
		payload := map[string]any{
			"siteId":           r.SiteId,
			"bucket":           r.Bucket,
			"windowStart":      r.WindowStart.Format(time.RFC3339),
			"windowEnd":        r.WindowEnd.Format(time.RFC3339),
			"requestCount":     r.RequestCount,
			"errorCount":       r.ErrorCount,
			"clientErrorCount": r.ClientErrorCount,
			"bytesTotal":       r.BytesTotal,
		}
		// An unset lastServedAt would be the zero time, which reads as the
		// year 1 rather than as "not known". A row that came back at all has
		// one, but a relation that answered NULL must not put a fabricated
		// date in front of anybody.
		if !r.LastServedAt.IsZero() {
			payload["lastServedAt"] = r.LastServedAt.Format(time.RFC3339)
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			raw = []byte("{}")
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        fmt.Sprintf("%s:%s:%s", r.SiteId, r.Bucket, r.WindowStart.UTC().Format(time.RFC3339)),
			Concept:   resultConcept,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: r.WindowStart.UTC(),
			Payload:   raw,
		})
	}
	return out
}

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

func stringsArg(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		if single, ok := args[key].(string); ok && strings.TrimSpace(single) != "" {
			return []string{single}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// timeArg parses an RFC3339 instant. A malformed one is REFUSED rather than
// defaulted: a window silently widened to "since the epoch" would read a
// cluster's whole history back as though the caller had asked for it.
func timeArg(args map[string]any, key string) (time.Time, error) {
	raw := strings.TrimSpace(stringArg(args, key))
	if raw == "" {
		return time.Time{}, fmt.Errorf("%s is required, as an RFC3339 instant", key)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 instant; got %q", key, raw)
	}
	return t.UTC(), nil
}
