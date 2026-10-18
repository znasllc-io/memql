package memql

// routes_builtin.go -- the route builtins (design record 2026-10-18-routes):
// routesForActor, routeSave, routeDelete, routeReset, autoBalance,
// autoBalanceSave, autoBalanceReset, myBudget and myBudgetSave.
//
// Each answers ONE ROW. A list read (routesForActor) carries its list and the
// routing document's revision on that row, because the revision is what every
// later write names: a client that read the routes and the revision from two
// replies could pair a list with a revision it was never read at.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

const (
	routeCatalogConcept = "v1:router:routeCatalog"
	autoBalanceConcept  = "v1:router:autoBalance"
	myBudgetConcept     = "v1:router:myBudget"
)

// routeCaller is the person a route builtin acts for. Every one of them is a
// person's own question -- their routes, their budget -- so a call with no
// person behind it is refused rather than answered for nobody.
func routeCaller(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" {
		return "", fmt.Errorf("routes require sign-in")
	}
	return strings.TrimSpace(ac.UserId), nil
}

// routesForActorBuiltin answers {routes, revision}.
func (e *MemQLEngine) routesForActorBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := routeCaller(ctx)
	if err != nil {
		return nil, err
	}
	summaries, revision, err := e.routeSummaries(ctx, owner)
	if err != nil {
		return nil, err
	}
	routes := make([]any, 0, len(summaries))
	for _, s := range summaries {
		routes = append(routes, s.asMap())
	}
	return singleVirtualRow(routeCatalogConcept, "routes", map[string]any{"routes": routes, "revision": revision})
}

// routeSummaries is every route this person can pick, with readiness: Auto,
// Local, Apps, then their own by name.
func (e *MemQLEngine) routeSummaries(ctx context.Context, owner string) ([]routeSummaryJSON, int64, error) {
	if e == nil || e.policies == nil {
		return nil, 0, fmt.Errorf("routing is not configured on this node")
	}
	doc, err := e.policies.RouteConfiguration(ctx)
	if err != nil {
		return nil, 0, err
	}
	readiness := e.routeReadinessFor(ctx)
	var out []routeSummaryJSON
	for _, id := range []string{RouteIdAuto, RouteIdLocal, RouteIdApps} {
		var override *RouteJobs
		if j, ok := doc.Routes[id]; ok {
			c := j.Clone()
			override = &c
		}
		spec, _ := BuiltinRouteSpec(id, override)
		out = append(out, readiness.summarize(spec))
	}
	personal, err := readPersonalRoutes(ctx, e, owner)
	if err != nil {
		return nil, 0, err
	}
	sort.SliceStable(personal, func(i, j int) bool {
		return strings.ToLower(personal[i].Name) < strings.ToLower(personal[j].Name)
	})
	for _, spec := range personal {
		out = append(out, readiness.summarize(spec))
	}
	return out, doc.Revision, nil
}

// routeSummaryFor answers one route's summary, re-reading readiness.
func (e *MemQLEngine) routeSummaryFor(ctx context.Context, spec RouteSpec) map[string]any {
	return e.routeReadinessFor(ctx).summarize(spec).asMap()
}

// routeSaveBuiltin creates or changes a route.
func (e *MemQLEngine) routeSaveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := routeCaller(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := routeJobsArg(args)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(BareShortId(stringArg(args, "id")))
	switch {
	case id == RouteIdAuto:
		return nil, fmt.Errorf("Auto has no jobs to change; change its balance instead")
	case IsEditableBuiltinRoute(id):
		revision, err := routeRevisionArg(args, "revision")
		if err != nil {
			return nil, err
		}
		if err := e.policies.SaveRouteOverride(ctx, revision, id, jobs); err != nil {
			return nil, err
		}
		spec, _ := BuiltinRouteSpec(id, &jobs)
		row := e.routeSummaryFor(ctx, spec)
		row["revision"] = revision + 1
		return singleVirtualRow(RouteConceptID, id, row)
	}

	jobs = normalizeRouteJobs(jobs)
	if err := ValidateRouteJobs(jobs); err != nil {
		return nil, err
	}
	mine, err := readPersonalRoutes(ctx, e, owner)
	if err != nil {
		return nil, err
	}
	var spec RouteSpec
	if id == "" {
		name, err := NormalizeRouteName(stringArg(args, "name"))
		if err != nil {
			return nil, err
		}
		spec = RouteSpec{Id: id, Name: name, Kind: RouteKindCustom, Scope: RouteScopePersonal, OwnerUserId: owner}
		spec.Id = newRouteId()
	} else {
		if !ValidRouteId(id) {
			return nil, fmt.Errorf("%q is not a route id", id)
		}
		found := false
		for _, r := range mine {
			if r.Id == id {
				spec, found = r, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("route %s is not one of your routes", id)
		}
		if raw := strings.TrimSpace(stringArg(args, "name")); raw != "" {
			name, err := NormalizeRouteName(raw)
			if err != nil {
				return nil, err
			}
			spec.Name = name
		}
	}
	for _, r := range mine {
		if r.Id != spec.Id && strings.EqualFold(r.Name, spec.Name) {
			return nil, fmt.Errorf("you already have a route named %q", r.Name)
		}
	}
	spec.Jobs = jobs
	if err := writePersonalRoute(ctx, e, spec, true); err != nil {
		return nil, err
	}
	return singleVirtualRow(RouteConceptID, spec.Id, e.routeSummaryFor(ctx, spec))
}

// newRouteId mints a personal route's bare id.
func newRouteId() string { return id.NewShortId() }

// routeDeleteBuiltin deletes one of the caller's own routes.
func (e *MemQLEngine) routeDeleteBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := routeCaller(ctx)
	if err != nil {
		return nil, err
	}
	routeId := strings.TrimSpace(BareShortId(stringArg(args, "id")))
	if IsBuiltinRouteId(routeId) {
		return nil, fmt.Errorf("%s is a built-in route and cannot be deleted", builtinRouteNames[routeId])
	}
	spec, found, err := ReadPersonalRoute(ctx, e, owner, routeId)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("route %s is not one of your routes", routeId)
	}
	if err := writePersonalRoute(ctx, e, spec, false); err != nil {
		return nil, err
	}
	return singleVirtualRow(RouteConceptID, spec.Id, map[string]any{"id": spec.Id, "status": "deleted"})
}

// routeResetBuiltin restores Local or Apps to the shipped default.
func (e *MemQLEngine) routeResetBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := routeCaller(ctx); err != nil {
		return nil, err
	}
	routeId := strings.TrimSpace(stringArg(args, "id"))
	revision, err := routeRevisionArg(args, "revision")
	if err != nil {
		return nil, err
	}
	if err := e.policies.ResetRouteOverride(ctx, revision, routeId); err != nil {
		return nil, err
	}
	spec, _ := BuiltinRouteSpec(routeId, nil)
	row := e.routeSummaryFor(ctx, spec)
	row["revision"] = revision + 1
	return singleVirtualRow(RouteConceptID, routeId, row)
}

// autoBalanceBuiltin answers Auto's balance; anyone signed in may read it.
func (e *MemQLEngine) autoBalanceBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := routeCaller(ctx); err != nil {
		return nil, err
	}
	if e == nil || e.policies == nil {
		return nil, fmt.Errorf("routing is not configured on this node")
	}
	doc, err := e.policies.RouteConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	return singleVirtualRow(autoBalanceConcept, "auto", autoBalanceRow(doc.Auto, doc.Revision))
}

func autoBalanceRow(saved *AutoBalance, revision int64) map[string]any {
	current := RecommendedAutoBalance
	if saved != nil {
		current = *saved
	}
	return map[string]any{
		"speed":   current.Speed,
		"cost":    current.Cost,
		"quality": current.Quality,
		"recommended": map[string]any{
			"speed":   RecommendedAutoBalance.Speed,
			"cost":    RecommendedAutoBalance.Cost,
			"quality": RecommendedAutoBalance.Quality,
		},
		"customized": saved != nil,
		"revision":   revision,
	}
}

// autoBalanceSaveBuiltin changes Auto's balance for the cluster.
func (e *MemQLEngine) autoBalanceSaveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	revision, err := routeRevisionArg(args, "revision")
	if err != nil {
		return nil, err
	}
	balance := AutoBalance{}
	for _, w := range []struct {
		key string
		dst *int
	}{{"speed", &balance.Speed}, {"cost", &balance.Cost}, {"quality", &balance.Quality}} {
		n, err := routeIntArg(args, w.key)
		if err != nil {
			return nil, err
		}
		*w.dst = n
	}
	if err := e.policies.SaveAutoBalance(ctx, revision, balance); err != nil {
		return nil, err
	}
	return singleVirtualRow(autoBalanceConcept, "auto", autoBalanceRow(&balance, revision+1))
}

// autoBalanceResetBuiltin returns Auto to the recommended balance.
func (e *MemQLEngine) autoBalanceResetBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	revision, err := routeRevisionArg(args, "revision")
	if err != nil {
		return nil, err
	}
	if err := e.policies.ResetAutoBalance(ctx, revision); err != nil {
		return nil, err
	}
	return singleVirtualRow(autoBalanceConcept, "auto", autoBalanceRow(nil, revision+1))
}

// myBudgetBuiltin answers the caller's monthly budget and this month's spend.
func (e *MemQLEngine) myBudgetBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := routeCaller(ctx)
	if err != nil {
		return nil, err
	}
	state, err := ReadUserBudget(ctx, e, owner, time.Now())
	if err != nil {
		return nil, err
	}
	return singleVirtualRow(myBudgetConcept, "budget", state.row())
}

// myBudgetSaveBuiltin sets or removes the caller's monthly budget.
func (e *MemQLEngine) myBudgetSaveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := routeCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	limit, set, err := routeOptionalFloatArg(args, "monthlyLimitUSD")
	if err != nil {
		return nil, err
	}
	if set && (limit <= 0 || limit > 1_000_000) {
		return nil, fmt.Errorf("a monthly budget is more than $0 and at most $1,000,000; leave it out to remove it")
	}
	if err := writeUserBudget(ctx, e, owner, limit, set, now); err != nil {
		return nil, err
	}
	state, err := ReadUserBudget(ctx, e, owner, now)
	if err != nil {
		return nil, err
	}
	return singleVirtualRow(myBudgetConcept, "budget", state.row())
}

// ---- argument readers ----

func routeJobsArg(args map[string]any) (RouteJobs, error) {
	read := func(key string) ([]string, error) {
		raw, ok := args[key]
		if !ok || raw == nil {
			return nil, fmt.Errorf("%s is required", key)
		}
		switch v := raw.(type) {
		case []string:
			return append([]string(nil), v...), nil
		case []any:
			out := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("every source in %s must be a string", key)
				}
				out = append(out, s)
			}
			return out, nil
		}
		return nil, fmt.Errorf("%s must be a list of sources", key)
	}
	fast, err := read("fast")
	if err != nil {
		return RouteJobs{}, err
	}
	strong, err := read("strong")
	if err != nil {
		return RouteJobs{}, err
	}
	reasoning, err := read("reasoning")
	if err != nil {
		return RouteJobs{}, err
	}
	return RouteJobs{Fast: fast, Strong: strong, Reasoning: reasoning}, nil
}

func routeRevisionArg(args map[string]any, key string) (int64, error) {
	n, err := routeIntArg(args, key)
	if err != nil {
		return 0, fmt.Errorf("%s is required: read routesForActor first and pass its revision", key)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be nonnegative", key)
	}
	return int64(n), nil
}

func routeIntArg(args map[string]any, key string) (int, error) {
	switch v := args[key].(type) {
	case int:
		return v, nil
	case int64:
		if v > math.MaxInt32 || v < math.MinInt32 {
			return 0, fmt.Errorf("%s is out of range", key)
		}
		// narrowing: GUARDED -- bounded to the int32 range on the line above.
		return int(v), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v > math.MaxInt32 || v < math.MinInt32 {
			return 0, fmt.Errorf("%s must be a whole number", key)
		}
		// narrowing: GUARDED -- whole and within the int32 range, checked above.
		return int(v), nil
	}
	return 0, fmt.Errorf("%s is required", key)
}

func routeOptionalFloatArg(args map[string]any, key string) (float64, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false, fmt.Errorf("%s must be a number", key)
		}
		return v, true, nil
	case int:
		return float64(v), true, nil
	case int64:
		return float64(v), true, nil
	}
	return 0, false, fmt.Errorf("%s must be a number", key)
}

// ---- readiness ----

// routeReadiness is what one person can reach right now, read once per
// routesForActor so every route on the list is judged against the same moment.
type routeReadiness struct {
	doors inferenceDoors
	// embedder names the bound embedding model, "" when none is bound.
	embedder string
	// embeddingReachable is true when an embedding model could serve this
	// person now: the bound one, or one that would be bound automatically.
	embeddingReachable bool
}

func (e *MemQLEngine) routeReadinessFor(ctx context.Context) routeReadiness {
	r := routeReadiness{doors: e.inferenceDoors(ctx)}
	if binding, err := e.ReadEmbedderBinding(ctx); err == nil && binding.Valid() {
		r.embedder = binding.ProviderRef
	}
	r.embeddingReachable = r.embedder != "" || len(r.doors.EmbeddingModelIds) > 0 || (e.providers != nil && e.providers.federationEmbedderAvailable())
	return r
}

// generationReady reports whether anything can answer this person.
func (r routeReadiness) generationReady() bool {
	return r.doors.LocalEligible || r.doors.AppEligible || r.doors.Federation
}

// summarize judges one route against this moment.
func (r routeReadiness) summarize(spec RouteSpec) routeSummaryJSON {
	out := routeSummaryJSON{
		Id: spec.Id, Name: spec.Name, Kind: spec.Kind, Scope: spec.Scope,
		Description: spec.Description, Customized: spec.Customized, Jobs: []routeJobJSON{},
	}
	if spec.Kind == RouteKindAuto {
		switch {
		case !r.generationReady():
			out.Reason = "Needs a source that can answer"
		case !r.embeddingReachable:
			out.Reason = "Needs an embedding model"
		default:
			out.Ready = true
		}
		return out
	}
	ready := true
	reason := ""
	for _, job := range []struct {
		key   airoute.Level
		label string
	}{{airoute.LevelFast, "Quick tasks"}, {airoute.LevelStrong, "Main work"}, {airoute.LevelReasoning, "Deep reasoning"}} {
		sources := spec.Jobs.For(job.key)
		serves := ""
		for _, entry := range sources {
			if label, ok := r.servesNow(entry); ok {
				serves = label
				break
			}
		}
		out.Jobs = append(out.Jobs, routeJobJSON{Job: string(job.key), Sources: sources, Editable: true, ServesNow: serves, Ready: serves != ""})
		if serves == "" && ready {
			ready = false
			reason = r.unreadyReason(sources, job.label)
		}
	}
	embedder := r.embedder
	out.Jobs = append(out.Jobs, routeJobJSON{Job: string(airoute.LevelEmbeddings), Sources: []string{"embedder:active"}, Editable: false, ServesNow: embedder, Ready: r.embeddingReachable})
	if ready && !r.embeddingReachable {
		ready = false
		reason = "Needs an embedding model"
	}
	out.Ready = ready
	out.Reason = reason
	return out
}

// servesNow says whether one source can serve this person now, and how to
// name it on the route page ("Serves now: Claude Code").
func (r routeReadiness) servesNow(entry string) (string, bool) {
	d := r.doors
	switch {
	case entry == "fleet:strongest" || entry == "fleet:fastest":
		if d.LocalEligible && len(d.EligibleModelIds) > 0 {
			if entry == "fleet:fastest" {
				return "Fastest local model", true
			}
			return "Strongest local model", true
		}
	case strings.HasPrefix(entry, FleetReferencePrefix):
		model := strings.TrimPrefix(entry, FleetReferencePrefix)
		for _, m := range d.EligibleModelIds {
			if m == model {
				return model, true
			}
		}
	case entry == AppWildcard:
		if d.AppEligible && len(d.RunnableApps) > 0 {
			names := make([]string, 0, len(d.RunnableApps))
			for _, a := range d.RunnableApps {
				names = append(names, appDisplayName(a))
			}
			return strings.Join(names, " or "), true
		}
	case strings.HasPrefix(entry, AppReferencePrefix):
		appId, _, _ := SplitAppReference(entry)
		for _, a := range d.RunnableApps {
			if a == appId {
				return appDisplayName(a), true
			}
		}
	case strings.HasPrefix(entry, "federation:"):
		if d.Federation {
			switch strings.TrimPrefix(entry, "federation:") {
			case "cheapest":
				return "Cheapest vendor", true
			case "strongest":
				return "Strongest vendor", true
			}
			return strings.TrimPrefix(entry, "federation:"), true
		}
	}
	return "", false
}

// unreadyReason is one short sentence naming what a job is missing.
func (r routeReadiness) unreadyReason(sources []string, job string) string {
	allApps, allFleet, allVendors := len(sources) > 0, len(sources) > 0, len(sources) > 0
	for _, s := range sources {
		allApps = allApps && strings.HasPrefix(s, AppReferencePrefix)
		allFleet = allFleet && strings.HasPrefix(s, FleetReferencePrefix)
		allVendors = allVendors && strings.HasPrefix(s, "federation:")
	}
	switch {
	case allApps:
		return "No signed-in app on your machines"
	case allFleet:
		return "No local model reachable"
	case allVendors:
		return "No vendor is set up"
	}
	return job + ": nothing can serve it now"
}

func appDisplayName(appId string) string {
	switch appId {
	case "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return appId
}

// federationEmbedderAvailable reports whether a federated vendor here can
// produce embeddings -- the third way an embedding model is reachable.
func (r *ProviderRegistry) federationEmbedderAvailable() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry == nil || !entry.Available || providerAuthSourceOf(entry) != AuthSourceFederation {
			continue
		}
		if _, ok := entry.Client.(EmbeddingAIProvider); ok {
			return true
		}
	}
	return false
}

// ---- the user budget ----

// UserBudget is one person's monthly budget and this month's metered spend.
type UserBudget struct {
	// Limit is the monthly budget in USD; Set is false when there is none.
	Limit float64
	Set   bool
	// Spent is this month's metered spend, in USD.
	Spent float64
	// ResetsAt is the first instant of next month, UTC.
	ResetsAt time.Time
}

// State is where the budget stands: "none", "under", "near" (80% or more) or
// "over" (100% or more). Auto reads it (routes_auto.go).
func (b UserBudget) State() string {
	switch {
	case !b.Set || b.Limit <= 0:
		return "none"
	case b.Spent >= b.Limit:
		return "over"
	case b.Spent >= 0.8*b.Limit:
		return "near"
	}
	return "under"
}

func (b UserBudget) row() map[string]any {
	out := map[string]any{
		"monthlyLimitUSD":   nil,
		"spentThisMonthUSD": math.Round(b.Spent*100) / 100,
		"resetsAt":          b.ResetsAt.UTC().Format(time.RFC3339),
		"state":             b.State(),
	}
	if b.Set {
		out["monthlyLimitUSD"] = b.Limit
	}
	return out
}

// monthWindow is the UTC calendar month containing now.
func monthWindow(now time.Time) (start, next time.Time) {
	now = now.UTC()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// userBudgetRowId is the budget row a person's monthly budget lives at: one
// row per person, derived from them, so a second save updates rather than
// forking a second budget Auto would have to choose between.
func userBudgetRowId(owner string) string {
	return "user-" + BareShortId(owner)
}

// ReadUserBudget reads one person's budget and this month's metered spend.
func ReadUserBudget(ctx context.Context, exec routeExecutor, owner string, now time.Time) (UserBudget, error) {
	start, next := monthWindow(now)
	out := UserBudget{ResetsAt: next}
	maint := RouteMaintenanceContext(ContextWithFreshRead(ctx))
	q, err := languageParser.RenderCall("routerBudgets", map[string]any{"scope": "user"})
	if err != nil {
		return out, err
	}
	res, err := exec.Execute(maint, "query "+q)
	if err != nil {
		return out, fmt.Errorf("read budget: %w", err)
	}
	for _, row := range MaterializeRows(res) {
		scopeId, _ := row["scopeId"].(string)
		if !sameUserId(scopeId, owner) {
			continue
		}
		if period, _ := row["periodType"].(string); period != "monthly" {
			continue
		}
		if limit, ok := row["limitUSD"].(float64); ok && limit > 0 {
			out.Limit, out.Set = limit, true
		}
	}
	q, err = languageParser.RenderCall("routerCallsForBudget", map[string]any{"userId": owner, "since": start.Format(time.RFC3339)})
	if err != nil {
		return out, err
	}
	res, err = exec.Execute(maint, "query "+q)
	if err != nil {
		return out, fmt.Errorf("read this month's spend: %w", err)
	}
	for _, row := range MaterializeRows(res) {
		billing, _ := row["billing"].(string)
		if billing != "" && billing != "metered" {
			// Subscription, local and unknown never count: the budget is
			// what MemQL is billed for, the way the dollar ceiling is.
			continue
		}
		if cost, ok := row["totalCost"].(float64); ok && cost > 0 && !math.IsInf(cost, 0) {
			out.Spent += cost
		}
	}
	return out, nil
}

// writeUserBudget sets (set=true) or removes one person's monthly budget.
func writeUserBudget(ctx context.Context, exec routeExecutor, owner string, limit float64, set bool, now time.Time) error {
	_, next := monthWindow(now)
	if !set {
		limit = 0
	}
	q, err := languageParser.RenderCall("setBudget", map[string]any{
		"id":         userBudgetRowId(owner),
		"scope":      "user",
		"scopeId":    owner,
		"periodType": "monthly",
		"limitUSD":   limit,
		"resetAt":    next.Format(time.RFC3339),
		"active":     set,
	})
	if err != nil {
		return err
	}
	_, err = exec.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+q)
	return err
}
