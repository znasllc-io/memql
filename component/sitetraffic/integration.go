package sitetraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The DSL-callable half: `builtin siteTrafficInWindow` and `builtin
// siteStatisticsRollup`, declared in dsl/platform/builtins.memql and executed
// here. The first is a read any caller makes about the deployables they may
// see; the second is the five-minute rollup (rollup.go), which only its
// scheduled automation's maintenance principal may run.
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

// rollupConcept is the concept of the one node the rollup answers with, its
// report. Not a DSL concept: nothing stores it and no client reads it, which
// is how component/logstore's sweep answers too (integration:logs:sweep).
const rollupConcept = "integration:siteTraffic:rollup"

// rollupPrincipal is the only caller the rollup admits: the maintenance
// principal its scheduled automation runs under
// (auth.MaintenanceActor("siteStatisticsRollupSweep")).
const rollupPrincipal = "system:maintenance:siteStatisticsRollupSweep"

// countryRefreshConcept is the concept of the one node the country refresh
// answers with, its report; like rollupConcept it is stored nowhere.
const countryRefreshConcept = "integration:siteTraffic:countryRefresh"

// countryRefreshPrincipal is the only caller the country refresh admits: the
// maintenance principal of its daily automation.
const countryRefreshPrincipal = "system:maintenance:geoCountryRefreshSweep"

// Integration exposes the read, the rollup and the country refresh.
type Integration struct {
	engine Engine
	bunDB  func() *bun.DB
	logger *slog.Logger

	// now is the rollup's clock; nil is time.Now. A field so a test runs the
	// builtin at an instant of its own choosing.
	now func() time.Time

	// rollupRunning keeps one rollup per process. A run slower than the
	// five-minute schedule must not have the next one queue up behind it --
	// each would wait on the table lock and then redo the same days. The
	// lock in rollup.go is what keeps two REPLICAS apart; this keeps one
	// process from stacking its own runs.
	rollupRunning atomic.Bool

	// countryBaseURL and httpClient are the country refresh's source and
	// transport. Fields so a test points them at an httptest.Server and never
	// reaches the internet; NewIntegration sets the production defaults.
	countryBaseURL string
	httpClient     *http.Client
}

// NewIntegration wires the handles. The db getter is resolved lazily at call
// time, not at construction: a node's database handle is not necessarily up
// when plug-ins are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		engine: engine, bunDB: bunDB, logger: logger,
		countryBaseURL: defaultCountryBaseURL,
		httpClient:     &http.Client{Timeout: 2 * time.Minute},
	}
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
		{
			Name:        "rollup",
			Description: "Fold the edge's request log into hourly and daily website statistics: today and every hour of it, every earlier logged day not yet final (a day is final once recomputed at or after 02:00 UTC the next day), then retention. Admits only the siteStatisticsRollupSweep maintenance principal.",
			Handler:     i.handleRollup,
		},
		{
			Name:        "countryRefresh",
			Description: "Download the current month's DB-IP IP-to-Country Lite database into edge_geo_country when the stored release is older, falling back to the previous month while DB-IP has not published the current one. Admits only the geoCountryRefreshSweep maintenance principal.",
			Handler:     i.handleCountryRefresh,
		},
	}
}

// handleRollup runs the rollup for the scheduled automation, and for nobody
// else, and answers one node with its report.
//
// THE PRINCIPAL AND NOTHING ELSE, the site health sweep's rule
// (component/sitehealth). The rollup writes every deployable's statistics and
// spends the database's time on a schedule; a cluster owner has no reason to
// run it by hand, and an owner who could would only queue a second run behind
// the lock. So the check is the automation's own maintenance id together
// with the cluster-owner role that principal carries -- the id alone with any
// other role is refused, as is any other automation's principal.
//
// Every day the run could not write is logged here, one Warn naming the day,
// and the run's error is returned, so the automation's run says it failed; the
// next run recomputes those days (rollup.go).
func (i *Integration) handleRollup(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || actor.UserId != rollupPrincipal || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("site statistics rollup requires the engine maintenance principal")
	}
	if !i.rollupRunning.CompareAndSwap(false, true) {
		return nil, nil
	}
	defer i.rollupRunning.Store(false)

	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := Rollup(ctx, db, now)
	i.logRollupFailures(err)
	// A routine run is the same line every five minutes, so it is Debug; a
	// backfill or a retention delete is a change worth a line at Info. A
	// failure has its own Warn above.
	level := slog.LevelDebug
	if rep.Backfilled || rep.Deleted > 0 {
		level = slog.LevelInfo
	}
	i.logger.Log(ctx, level, "Site statistics rolled up",
		"component", "sitetraffic", "days", rep.Days, "hours", rep.Hours,
		"backfilled", rep.Backfilled, "deleted", rep.Deleted)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("site statistics rollup: encode the report: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "siteStatisticsRollup",
		Concept:   rollupConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
}

// handleCountryRefresh runs the monthly country download for its daily
// automation, and for nobody else (handleRollup's rule).
//
// A FAILED DOWNLOAD IS A WARN AND AN ANSWER, NOT AN ERROR. The stored database
// is untouched and keeps serving, and the next day's run tries again, so there
// is nothing for a person to act on; an erroring automation would fail every
// day on a cluster with no route to the internet and bury real failures. The
// report's Reason carries the cause for anyone reading the run, and the Warn
// is what an operator searching logs finds. Only a refused principal is an
// error, because that is a wiring fault and not a transient one.
func (i *Integration) handleCountryRefresh(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || actor.UserId != countryRefreshPrincipal || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("country refresh requires the engine maintenance principal")
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := refreshCountries(ctx, db, i.httpClient, i.countryBaseURL, now)
	if err != nil {
		i.logger.Warn("Country database refresh failed; the stored release stays in use",
			"component", "sitetraffic", "release", rep.Release, "error", err)
		rep.Reason = err.Error()
	} else if rep.Downloaded {
		i.logger.Info("Country database refreshed (IP Geolocation by DB-IP, CC BY 4.0)",
			"component", "sitetraffic", "release", rep.Release)
	}
	payload, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("country refresh: encode the report: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "geoCountryRefresh",
		Concept:   countryRefreshConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
}

// logRollupFailures writes one Warn per failure in a run's error: one per day
// the run could not write, naming the day, so a day that keeps failing is
// findable by its date, and one for a failed plan or retention pass.
func (i *Integration) logRollupFailures(err error) {
	if err == nil {
		return
	}
	failures := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failures = joined.Unwrap()
	}
	for _, f := range failures {
		var day *dayFailure
		if errors.As(f, &day) {
			i.logger.Warn("Site statistics for a day were not written; the next run recomputes it",
				"component", "sitetraffic", "day", day.Day.Format(time.DateOnly), "error", day.Err)
			continue
		}
		i.logger.Warn("Site statistics rollup failed", "component", "sitetraffic", "error", f)
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
