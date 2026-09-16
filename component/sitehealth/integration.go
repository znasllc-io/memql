package sitehealth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type Engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}
type Checker interface {
	Check(context.Context, Site) Observation
}
type Integration struct {
	engine  Engine
	db      func() *bun.DB
	checker Checker
	logger  *slog.Logger
	running atomic.Bool
}

func init() {
	memql.RegisterPlugin("siteHealth", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
		if domain == "" {
			domain = "memql.localhost"
		}
		probe, err := NewProber(domain, strings.TrimSpace(os.Getenv("MEMQL_SITE_HEALTH_DIAL_ADDRESS")), strings.TrimSpace(os.Getenv("MEMQL_SITE_HEALTH_CA_FILE")))
		if err != nil {
			if pctx.Logger != nil {
				pctx.Logger.Warn("Website health checker is not configured", "component", "siteHealth", "error", err)
			}
		}
		return &Integration{engine: pctx.Engine, db: pctx.BunDB, checker: probe, logger: pctx.Logger}, nil
	})
}

func (i *Integration) IntegrationName() string { return "siteHealth" }
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "read", Description: "Read current website observations, authorized by the site", Handler: i.read, ArgsSchema: map[string]string{"siteIds": "[]string"}},
		{Name: "sweep", Description: "Check due published websites once per cluster", Handler: i.sweep},
	}
}

func (i *Integration) sites(ctx context.Context) ([]Site, error) {
	result, err := i.engine.Execute(ctx, "query sitesAll()")
	if err != nil {
		return nil, err
	}
	var sites []Site
	for _, row := range memql.MaterializeRows(result) {
		str := func(k string) string { v, _ := row[k].(string); return v }
		sites = append(sites, Site{memql.BareShortId(str("id")), str("hostname"), str("bundleRef"), str("status")})
	}
	return sites, nil
}

func (i *Integration) database() (*bun.DB, error) {
	if i.db == nil || i.db() == nil {
		return nil, fmt.Errorf("website health storage is unavailable")
	}
	return i.db(), nil
}

// A read never probes a URL. Authorization is reevaluated against today's site
// rows, so an old observation cannot retain access for a previous owner.
func (i *Integration) read(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	want := map[string]bool{}
	switch ids := args["siteIds"].(type) {
	case []any:
		for _, v := range ids {
			if id, ok := v.(string); ok {
				want[memql.BareShortId(id)] = true
			}
		}
	case []string:
		for _, id := range ids {
			want[memql.BareShortId(id)] = true
		}
	}
	if len(want) > 200 {
		return nil, fmt.Errorf("read website health in batches of at most 200 sites")
	}
	if len(want) == 0 {
		return nil, nil
	}
	sites, err := i.sites(ctx)
	if err != nil {
		return nil, err
	}
	allowed := map[string]Site{}
	for _, s := range sites {
		if want[s.ID] && s.Status == "live" {
			allowed[s.ID] = s
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(allowed))
	for id := range allowed {
		ids = append(ids, id)
	}
	var rows []Observation
	if err := db.NewSelect().TableExpr("site_health").Where("site_id IN (?)", bun.In(ids)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	var out []memorynodes.MemoryNode
	for _, row := range rows {
		s := allowed[row.SiteID]
		if row.Hostname != s.Hostname || row.BundleRef != s.BundleRef {
			continue
		}
		payload, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, memorynodes.MemoryNode{ID: row.SiteID, Concept: "v1:platform:siteHealth", Type: memorynodes.NodeTypeObject, CreatedAt: row.CheckedAt, Payload: payload})
	}
	return out, nil
}

func (i *Integration) sweep(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || actor.UserId != "system:maintenance:checkDeployableHealth" || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("website health checks require the engine maintenance principal")
	}
	if !i.running.CompareAndSwap(false, true) {
		return nil, nil
	}
	defer i.running.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	sites, err := i.sites(ctx)
	if err != nil {
		return nil, err
	}
	var previous []Observation
	if err := db.NewSelect().TableExpr("site_health").Scan(ctx, &previous); err != nil {
		return nil, err
	}
	old := map[string]Observation{}
	for _, o := range previous {
		old[o.SiteID] = o
	}
	due := dueSites(sites, old, time.Now())
	// Oldest first and a bounded batch: a large cluster gets eventual checks
	// without an unbounded burst or silently ignoring every site after page one.
	if len(due) > 64 {
		due = due[:64]
	}
	jobs := make(chan Site)
	var wg sync.WaitGroup
	var changed, failed atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for site := range jobs {
				o := i.checker.Check(ctx, site)
				_, err := db.NewRaw(`INSERT INTO site_health (site_id,hostname,bundle_ref,checked_at,state,reason,http_status,duration_ms)
				VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (site_id) DO UPDATE SET
				hostname=EXCLUDED.hostname,bundle_ref=EXCLUDED.bundle_ref,checked_at=EXCLUDED.checked_at,
				state=EXCLUDED.state,reason=EXCLUDED.reason,http_status=EXCLUDED.http_status,duration_ms=EXCLUDED.duration_ms
				WHERE site_health.checked_at <= EXCLUDED.checked_at`, o.SiteID, o.Hostname, o.BundleRef, o.CheckedAt, o.State, o.Reason, o.HTTPStatus, o.DurationMs).Exec(ctx)
				if err != nil {
					failed.Add(1)
					continue
				}
				prior := old[site.ID]
				if prior.State != o.State || prior.HTTPStatus != o.HTTPStatus {
					changed.Add(1)
				}
			}
		}()
	}
	for _, site := range due {
		select {
		case jobs <- site:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	// Remove observations for sites no longer published; storage remains one
	// row per active site. Existing results are never counted as visitor traffic.
	active := []string{}
	for _, s := range sites {
		if s.Status == "live" {
			active = append(active, s.ID)
		}
	}
	q := db.NewDelete().TableExpr("site_health")
	if len(active) > 0 {
		q = q.Where("site_id NOT IN (?)", bun.In(active))
	} else {
		q = q.Where("TRUE")
	}
	if _, err := q.Exec(ctx); err != nil {
		return nil, err
	}
	if changed.Load() > 0 && i.logger != nil {
		i.logger.Info("Website health changed", "component", "siteHealth", "checked", len(due), "changed", changed.Load())
	}
	if failed.Load() > 0 {
		return nil, fmt.Errorf("website health: %d observations could not be stored", failed.Load())
	}
	return nil, ctx.Err()
}

func dueSites(sites []Site, old map[string]Observation, now time.Time) []Site {
	due := []Site{}
	for _, s := range sites {
		if s.Status != "live" || s.ID == "" {
			continue
		}
		o := old[s.ID]
		if o.Hostname != s.Hostname || o.BundleRef != s.BundleRef || now.Sub(o.CheckedAt) >= 110*time.Second {
			due = append(due, s)
		}
	}
	sort.Slice(due, func(a, b int) bool { return old[due[a].ID].CheckedAt.Before(old[due[b].ID].CheckedAt) })
	return due
}
