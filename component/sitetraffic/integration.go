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
	"github.com/znasllc-io/memql/core/statsperiod"
)

// The DSL-callable half, declared in dsl/platform/builtins.memql and executed
// here. Two reads any caller makes about the deployables they may see:
// `builtin siteTrafficInWindow`, the request counts the list row shows, and
// `builtin siteStatistics`, a period of website statistics (statistics.go).
// Two pieces of scheduled work, each of which only its own automation's
// maintenance principal may run: `builtin siteStatisticsRollup`, the
// five-minute rollup the statistics are read from (rollup.go), and `builtin
// geoCountryRefresh`, the monthly country database (countryrefresh.go).
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
// (auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep")).
const rollupPrincipal = "system:maintenance:" + rollupAutomation

// rollupAutomation is the engine automation whose maintenance principal the
// rollup admits, decided on the principal its constructor minted.
const rollupAutomation = "siteStatisticsRollupSweep"

// countryRefreshConcept is the concept of the one node the country refresh
// answers with, its report; like rollupConcept it is stored nowhere.
const countryRefreshConcept = "integration:siteTraffic:countryRefresh"

// countryRefreshPrincipal is the only caller the country refresh admits: the
// maintenance principal of its daily automation.
const countryRefreshPrincipal = "system:maintenance:" + countryRefreshAutomation

// countryRefreshAutomation is the engine automation whose maintenance principal
// the country refresh admits.
const countryRefreshAutomation = "geoCountryRefreshSweep"

// Integration exposes the two reads, the rollup and the country refresh.
type Integration struct {
	engine Engine
	// bunDB is the request pool: both reads and the country refresh run on it.
	bunDB func() *bun.DB
	// maintenance opens the session the rollup runs on, and the rollup runs on
	// nothing else (rollup.go, "Where the statements run"). nil refuses the
	// rollup and nothing more: the reads and the country refresh never use it.
	maintenance MaintenanceSession
	logger      *slog.Logger

	// now is the clock of the rollup, the country refresh and the statistics
	// read's period; nil is time.Now. A field so a test runs a builtin at an
	// instant of its own choosing.
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

// NewIntegration wires the handles. The db getter and the session opener are
// resolved lazily at call time, not at construction: a node's database handle
// is not necessarily up when plug-ins are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, maintenance MaintenanceSession, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		engine: engine, bunDB: bunDB, maintenance: maintenance, logger: logger,
		countryBaseURL: defaultCountryBaseURL,
		httpClient:     &http.Client{Timeout: 2 * time.Minute},
	}
}

// A context without OpenMaintenanceSession still builds the plug-in: the
// builtins are declared in every binary's DSL and must resolve everywhere
// (above), and only the rollup needs the session, so it is the rollup that
// refuses (handleRollup).
func init() {
	memql.RegisterPlugin("siteTraffic", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine, pctx.BunDB, pctx.OpenMaintenanceSession, pctx.Logger), nil
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
			Name:        "statistics",
			Description: "Read a period of website statistics for the deployables the caller may read: totals against the previous period, a series of one point per hour, day or month, the leading pages, sources, countries, devices, browsers and systems, and each deployable's own figures. A figure nothing measured is null, never zero.",
			Handler:     i.handleStatistics,
			ArgsSchema: map[string]string{
				"siteIds":    "[]string -- the deployables to read, bare ids, at most 200; empty reads every deployable the caller may read",
				"accountIds": "[]string -- with no siteIds, keeps the read to these organizations, bare ids; it never widens what the caller reaches",
				"period":     "string (required) -- 24h, 7d, 30d, 90d or 12m",
			},
		},
		{
			Name:        "rollup",
			Description: "Fold the edge's request log into hourly and daily website statistics: each request once it is two hours old, the last two hours on every run, today and every earlier logged day not yet final (a day is final once its fold reaches its end, at 02:00 UTC the next day), the 24-hour window's and each month's top lists, then retention. Admits only the siteStatisticsRollupSweep maintenance principal.",
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
// next run carries those days on from their progress (rollup.go).
//
// NO MAINTENANCE SESSION, NO ROLLUP. The run's statements go to the database's
// dedicated maintenance session (rollup.go, "Where the statements run"), so a
// node whose plug-in context offers none is refused, never served from the
// request pool, as memql.PluginContext.OpenMaintenanceSession requires. The app
// bootstrap gives every node an opener; on a node without a database, the
// first run answers the opener's own refusal.
func (i *Integration) handleRollup(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || !auth.IsMaintenancePrincipalOf(actor, rollupAutomation) {
		return nil, fmt.Errorf("site statistics rollup requires the engine maintenance principal")
	}
	if i.maintenance == nil {
		return nil, errNoMaintenanceSession
	}
	if !i.rollupRunning.CompareAndSwap(false, true) {
		return nil, nil
	}
	defer i.rollupRunning.Store(false)

	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := Rollup(ctx, i.maintenance, now)
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
	if !ok || !auth.IsMaintenancePrincipalOf(actor, countryRefreshAutomation) {
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
			i.logger.Warn("Site statistics for a day were not written; the next run carries it on",
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

// handleStatistics answers builtin siteStatistics: one node whose payload is
// the read's answer (Statistics.Payload), which a client reads as its row.
//
// ANY CALLER, NARROWED. There is no principal check here, as there is none on
// handleInWindow: what the caller reaches is decided in the read, under their
// own actor, and a caller who reaches nothing gets the answer of a deployable
// with no traffic. Only a malformed call is refused -- more than 200 named
// deployables, or a period the read does not offer, said in a sentence that
// names the periods.
//
// HEALTH IS NARROWED BY PART. A caller without execute app:deployables/health
// (owner and developer; owner's direction, 2026-11-10) gets the same answer
// with every Health figure null (Statistics.WithoutHealth), so a client reading
// their storefront's visitors never receives its error or response-time
// figures. Regression: TestHealthFiguresReachOnlyTheHealthPart (db).
// handleInWindow's errorCount is not narrowed: the Deployables list reads it
// only for "served X ago", and the concept declares it required.
func (i *Integration) handleStatistics(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	stats, err := NewReader(db, i.engine).Statistics(ctx, StatisticsQuery{
		// namedIdsArg, not stringsArg: a list of blank ids is NAMED and names
		// nothing, and must not read as no list at all (statisticsSites).
		SiteIds:    namedIdsArg(args, "siteIds"),
		AccountIds: namedIdsArg(args, "accountIds"),
		Period:     statsperiod.Period(stringArg(args, "period")),
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	if subject, ok := auth.SubjectFromContext(ctx); !ok || !auth.CapableFor(ctx, subject, auth.VerbExecute, healthPart) {
		stats = stats.WithoutHealth()
	}
	payload, err := json.Marshal(stats.Payload())
	if err != nil {
		return nil, fmt.Errorf("site statistics: encode the answer: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "statistics",
		Concept:   statisticsConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
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

// namedIdsArg decodes a list of ids the way the statistics read needs it:
// KEEPING THAT IT WAS NAMED. stringsArg drops a blank or non-string element,
// so a list holding nothing usable decodes to empty -- the spelling of "none
// named", which the statistics read answers with every deployable the caller
// may read. Here such an element is kept as "", which names no deployable, so
// the list stays named and its answer stays empty.
//
// Absent, null and an empty list name nothing (nil or empty). A string is one
// id, blank or not. Any other value is a list of one unusable id.
func namedIdsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, len(v))
		for i, e := range v {
			if s, ok := e.(string); ok {
				out[i] = s
			}
		}
		return out
	case []string:
		return append([]string{}, v...)
	case string:
		return []string{v}
	default:
		return []string{""}
	}
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
