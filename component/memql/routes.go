package memql

// routes.go -- a PERSON'S ROUTE (design record 2026-10-18-routes).
//
// A route is how a person decides, for one message in Ask, which sources do
// each job. Four kinds ship:
//
//   - AUTO is no route at all: the rules decide every call, and Auto ranks the
//     general shipped chains by measured speed, reliability, cost and quality
//     (routes_auto.go). It is the zero value everywhere.
//   - LOCAL and APPS are built-in routes with a job list each. An owner or a
//     developer may change either for the whole cluster; the change is an
//     OVERRIDE in the routing document, beside the policy and rule overrides
//     and under the same revision, and Restore default removes it.
//   - A PERSON'S OWN routes are v1:router:route rows, named by their owner and
//     private to them.
//
// # The jobs
//
// A route names sources for three jobs, the three levels a person can feel:
// Quick tasks (fast), Main work (strong) and Deep reasoning (reasoning).
//
// SEARCH AND MEMORY -- the embeddings level -- IS NEVER A ROUTE'S. Every route
// serves embeddings from the cluster's bound embedder, because the index holds
// vectors from exactly one model and a route that picked another would write
// vectors into a space they do not belong to (D10 of the app-session record,
// and the embeddingsBinding policy's own invariant). So a route has no field
// for it, and the router never applies a route to an embedding call.
//
// # Never outside the route
//
// A call made under a route walks the route's sources for its level and
// NOTHING ELSE. When they are exhausted the call degrades inside the route
// (reasoning to strong to fast, through the route's own lists) and then
// refuses, naming the route. That is what makes "Apps" mean apps: a route that
// fell through to the rules would quietly spend a vendor's money on a person
// who chose a subscription.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
)

// The built-in route ids. Stable strings: the wire carries them as
// `route:<id>`, the decision record stores them, and History filters on them.
const (
	RouteIdAuto  = "auto"
	RouteIdLocal = "local"
	RouteIdApps  = "apps"
)

// The route kinds, as routesForActor reports them.
const (
	RouteKindAuto   = "auto"
	RouteKindLocal  = "local"
	RouteKindApps   = "apps"
	RouteKindCustom = "custom"
)

// The route scopes: a built-in route is the cluster's, a person's own is theirs.
const (
	RouteScopeCluster  = "cluster"
	RouteScopePersonal = "personal"
)

// RouteSourcePrefix names a route in a turn's source: `route:<id>`.
const RouteSourcePrefix = "route:"

// RouteConceptID is the concept a person's own route is a row of.
const RouteConceptID = "v1:router:route"

// MaxRouteSources is how many sources one job may list. Four is enough to say
// "this model, then that one, then an app, then a vendor" and few enough that
// the route page can show a job on one line.
const MaxRouteSources = 4

// MaxRouteNameLength bounds a person's route name, in characters.
const MaxRouteNameLength = 40

// routeIdPattern is what a route id may look like on the wire: a built-in id or
// the bare short id of a person's route (a UUID). Anything else is refused at
// parse, before a row is read.
var routeIdPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// RouteJobs is the sources of each job, in the order they are tried.
type RouteJobs struct {
	Fast      []string `json:"fast"`
	Strong    []string `json:"strong"`
	Reasoning []string `json:"reasoning"`
}

// For returns the sources for one level. Embeddings and an unknown level have
// none: a route never governs them.
func (j RouteJobs) For(level airoute.Level) []string {
	switch level {
	case airoute.LevelFast:
		return append([]string(nil), j.Fast...)
	case airoute.LevelStrong:
		return append([]string(nil), j.Strong...)
	case airoute.LevelReasoning:
		return append([]string(nil), j.Reasoning...)
	}
	return nil
}

// Clone returns a deep copy.
func (j RouteJobs) Clone() RouteJobs {
	return RouteJobs{
		Fast:      append([]string(nil), j.Fast...),
		Strong:    append([]string(nil), j.Strong...),
		Reasoning: append([]string(nil), j.Reasoning...),
	}
}

// IsZero reports whether no job names a source.
func (j RouteJobs) IsZero() bool {
	return len(j.Fast) == 0 && len(j.Strong) == 0 && len(j.Reasoning) == 0
}

// builtinRouteDefaults are Local's and Apps' shipped job lists. Auto has none.
//
// Local is the fleet's own ordering, fastest for quick tasks and strongest for
// the rest -- which includes models shared with the person, because the
// router's fleet selectors expand over the person's whole catalog. Apps is the
// wildcard app door at every level: the machine maps each level to its app's
// own model and effort (Haiku, Sonnet or Opus for Claude Code; low, medium or
// high effort for Codex) through its apps.levels.
var builtinRouteDefaults = map[string]RouteJobs{
	RouteIdLocal: {Fast: []string{"fleet:fastest"}, Strong: []string{"fleet:strongest"}, Reasoning: []string{"fleet:strongest"}},
	RouteIdApps:  {Fast: []string{AppWildcard}, Strong: []string{AppWildcard}, Reasoning: []string{AppWildcard}},
}

var builtinRouteNames = map[string]string{
	RouteIdAuto:  "Auto",
	RouteIdLocal: "Local",
	RouteIdApps:  "Apps",
}

var builtinRouteDescriptions = map[string]string{
	RouteIdAuto:  "The best balance of speed, reliability and cost for each job",
	RouteIdLocal: "Local models for everything",
	RouteIdApps:  "Claude Code or Codex, with your embedding model",
}

// IsBuiltinRouteId reports whether id names Auto, Local or Apps.
func IsBuiltinRouteId(id string) bool {
	_, ok := builtinRouteNames[strings.TrimSpace(id)]
	return ok
}

// IsEditableBuiltinRoute reports whether id names a built-in route with a job
// list, which is Local or Apps. Auto has no jobs to edit.
func IsEditableBuiltinRoute(id string) bool {
	_, ok := builtinRouteDefaults[strings.TrimSpace(id)]
	return ok
}

// BuiltinRouteDefault returns Local's or Apps' shipped job list.
func BuiltinRouteDefault(id string) (RouteJobs, bool) {
	j, ok := builtinRouteDefaults[strings.TrimSpace(id)]
	if !ok {
		return RouteJobs{}, false
	}
	return j.Clone(), true
}

// RouteSpec is one route as the router and the builtins read it.
type RouteSpec struct {
	Id          string
	Name        string
	Kind        string
	Scope       string
	Description string
	// Jobs is empty for Auto.
	Jobs RouteJobs
	// Customized is true for a built-in route with a cluster override.
	Customized bool
	// OwnerUserId names whose route a personal one is; empty for a built-in.
	OwnerUserId string
	// UpdatedAt is when a personal route was last written.
	UpdatedAt string
}

// BuiltinRouteSpec returns a built-in route, with its cluster override applied
// when there is one.
func BuiltinRouteSpec(id string, override *RouteJobs) (RouteSpec, bool) {
	id = strings.TrimSpace(id)
	name, ok := builtinRouteNames[id]
	if !ok {
		return RouteSpec{}, false
	}
	spec := RouteSpec{Id: id, Name: name, Kind: id, Scope: RouteScopeCluster, Description: builtinRouteDescriptions[id]}
	if def, editable := builtinRouteDefaults[id]; editable {
		spec.Jobs = def.Clone()
		if override != nil && !override.IsZero() {
			spec.Jobs = override.Clone()
			spec.Customized = true
		}
	}
	return spec, true
}

// RouteIdFromSource reads a turn's source. isRoute reports whether it names a
// route at all; id is empty for Auto (`route:auto`), which is no route.
func RouteIdFromSource(source string) (id string, isRoute bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(source), RouteSourcePrefix)
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == RouteIdAuto {
		return "", true
	}
	return rest, true
}

// ValidRouteId reports whether id has a route id's shape.
func ValidRouteId(id string) bool {
	return routeIdPattern.MatchString(strings.TrimSpace(id))
}

// ValidateRouteJobs refuses a job list the router could not walk.
//
// Each job holds one to MaxRouteSources sources, each one a chain entry the
// router accepts (languageParser.ValidatePolicyEntry, the same grammar a policy
// is held to) AND one of the three source kinds a route may name: fleet:, app:
// or federation:. A `policy:` entry would make a route a second way to name a
// policy, and an `embedder:` entry would put Search and memory on a route,
// which no route may do. A source repeated in one job is refused too: the
// second copy could only ever be passed over.
func ValidateRouteJobs(j RouteJobs) error {
	for _, job := range []struct {
		name    string
		sources []string
	}{{"Quick tasks", j.Fast}, {"Main work", j.Strong}, {"Deep reasoning", j.Reasoning}} {
		if len(job.sources) == 0 {
			return fmt.Errorf("%s needs at least one source", job.name)
		}
		if len(job.sources) > MaxRouteSources {
			return fmt.Errorf("%s lists %d sources; a job holds at most %d", job.name, len(job.sources), MaxRouteSources)
		}
		seen := map[string]bool{}
		for _, raw := range job.sources {
			entry := strings.TrimSpace(raw)
			if err := validateRouteEntry(entry); err != nil {
				return fmt.Errorf("%s: %w", job.name, err)
			}
			if seen[entry] {
				return fmt.Errorf("%s lists %q twice", job.name, entry)
			}
			seen[entry] = true
		}
	}
	return nil
}

func validateRouteEntry(entry string) error {
	switch {
	case entry == "":
		return fmt.Errorf("a source is empty")
	case strings.HasPrefix(entry, "policy:"):
		return fmt.Errorf("%q names a policy; a route lists sources (fleet:, app: or federation:)", entry)
	case strings.HasPrefix(entry, "embedder:"):
		return fmt.Errorf("%q is the embedding model; Search and memory always uses the cluster's, so no route lists it", entry)
	case strings.HasPrefix(entry, FleetReferencePrefix), strings.HasPrefix(entry, AppReferencePrefix), strings.HasPrefix(entry, "federation:"):
		return languageParser.ValidatePolicyEntry(entry)
	}
	return fmt.Errorf("%q is not a source a route can list: use fleet:, app: or federation:", entry)
}

// normalizeRouteJobs trims every source.
func normalizeRouteJobs(j RouteJobs) RouteJobs {
	trim := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			out = append(out, strings.TrimSpace(s))
		}
		return out
	}
	return RouteJobs{Fast: trim(j.Fast), Strong: trim(j.Strong), Reasoning: trim(j.Reasoning)}
}

// NormalizeRouteName trims a route name and refuses one that is empty, too
// long, or the name of a built-in route -- a person's "Local" beside the
// cluster's Local would be two things one word cannot tell apart in a menu.
func NormalizeRouteName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("a route needs a name")
	}
	if utf8.RuneCountInString(name) > MaxRouteNameLength {
		return "", fmt.Errorf("a route name is at most %d characters", MaxRouteNameLength)
	}
	for _, builtin := range builtinRouteNames {
		if strings.EqualFold(name, builtin) {
			return "", fmt.Errorf("%q is a built-in route; choose another name", name)
		}
	}
	return name, nil
}

// AutoBalance is Auto's cluster-wide weighting: Speed, Cost and Quality, each
// 0 to 100. Reliability always weighs the mean of the three (routes_auto.go),
// so no setting trades it away.
type AutoBalance struct {
	Speed   int `json:"speed"`
	Cost    int `json:"cost"`
	Quality int `json:"quality"`
}

// RecommendedAutoBalance is MemQL's recommended balance: equal weights. It is
// the SHIPPED DEFAULT rather than a stored value, so a release can re-tune it
// from measured history without touching anybody's saved settings -- and a
// cluster that never changed it moves with it.
var RecommendedAutoBalance = AutoBalance{Speed: 50, Cost: 50, Quality: 50}

// Validate refuses a weight outside 0..100.
func (b AutoBalance) Validate() error {
	for _, w := range []struct {
		name  string
		value int
	}{{"speed", b.Speed}, {"cost", b.Cost}, {"quality", b.Quality}} {
		if w.value < 0 || w.value > 100 {
			return fmt.Errorf("%s must be between 0 and 100", w.name)
		}
	}
	return nil
}

// RouteOverride returns the cluster override for Local or Apps, when this
// registry (normally a per-decision snapshot) carries one.
func (r *PolicyRegistry) RouteOverride(id string) (RouteJobs, bool) {
	if r == nil {
		return RouteJobs{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.routes[strings.TrimSpace(id)]
	if !ok || j.IsZero() {
		return RouteJobs{}, false
	}
	return j.Clone(), true
}

// AutoBalanceSetting returns Auto's balance from this registry, and whether it
// is a saved override rather than the recommended one.
func (r *PolicyRegistry) AutoBalanceSetting() (AutoBalance, bool) {
	if r == nil {
		return RecommendedAutoBalance, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.auto == nil {
		return RecommendedAutoBalance, false
	}
	return *r.auto, true
}

// SetRouteOverrideForTest installs a Local or Apps override on a registry with
// no store. Test-only in intent; the router's tests use it.
func (r *PolicyRegistry) SetRouteOverrideForTest(id string, jobs RouteJobs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes == nil {
		r.routes = map[string]RouteJobs{}
	}
	r.routes[id] = jobs.Clone()
}

// SetAutoBalanceForTest installs Auto's balance on a registry with no store.
func (r *PolicyRegistry) SetAutoBalanceForTest(b AutoBalance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := b
	r.auto = &c
}

// RouteDocument is the routing document's route half as one read sees it.
type RouteDocument struct {
	Routes   map[string]RouteJobs
	Auto     *AutoBalance
	Revision int64
}

// RouteConfiguration reads the committed route overrides and Auto's balance.
func (r *PolicyRegistry) RouteConfiguration(ctx context.Context) (RouteDocument, error) {
	if r == nil {
		return RouteDocument{}, fmt.Errorf("policy registry unavailable")
	}
	r.mu.RLock()
	attached := r.store != nil
	r.mu.RUnlock()
	if !attached {
		r.mu.RLock()
		defer r.mu.RUnlock()
		out := RouteDocument{Routes: map[string]RouteJobs{}}
		for id, j := range r.routes {
			out.Routes[id] = j.Clone()
		}
		if r.auto != nil {
			c := *r.auto
			out.Auto = &c
		}
		return out, nil
	}
	doc, _, err := r.configuration(ctx)
	if err != nil {
		return RouteDocument{}, err
	}
	out := RouteDocument{Routes: map[string]RouteJobs{}, Revision: doc.Revision}
	for id, j := range doc.Routes {
		out.Routes[id] = j.Clone()
	}
	if doc.Auto != nil {
		c := *doc.Auto
		out.Auto = &c
	}
	return out, nil
}

// SaveRouteOverride changes Local or Apps for the whole cluster.
func (r *PolicyRegistry) SaveRouteOverride(ctx context.Context, expected int64, id string, jobs RouteJobs) error {
	if !IsEditableBuiltinRoute(id) {
		return fmt.Errorf("%q is not a built-in route with jobs; only Local and Apps can be changed for the cluster", id)
	}
	jobs = normalizeRouteJobs(jobs)
	if err := ValidateRouteJobs(jobs); err != nil {
		return err
	}
	return r.changeDocument(ctx, expected, func(doc *PolicyDocument) error {
		if doc.Routes == nil {
			doc.Routes = map[string]RouteJobs{}
		}
		doc.Routes[id] = jobs.Clone()
		return nil
	})
}

// ResetRouteOverride restores Local or Apps to MemQL's default.
func (r *PolicyRegistry) ResetRouteOverride(ctx context.Context, expected int64, id string) error {
	if !IsEditableBuiltinRoute(id) {
		return fmt.Errorf("%q is not a built-in route with jobs; only Local and Apps have a default to restore", id)
	}
	return r.changeDocument(ctx, expected, func(doc *PolicyDocument) error {
		delete(doc.Routes, id)
		return nil
	})
}

// SaveAutoBalance changes Auto's balance for the whole cluster.
func (r *PolicyRegistry) SaveAutoBalance(ctx context.Context, expected int64, balance AutoBalance) error {
	if err := balance.Validate(); err != nil {
		return err
	}
	return r.changeDocument(ctx, expected, func(doc *PolicyDocument) error {
		c := balance
		doc.Auto = &c
		return nil
	})
}

// ResetAutoBalance returns Auto to the recommended balance.
func (r *PolicyRegistry) ResetAutoBalance(ctx context.Context, expected int64) error {
	return r.changeDocument(ctx, expected, func(doc *PolicyDocument) error {
		doc.Auto = nil
		return nil
	})
}

// changeDocument is change()'s twin for the route half of the routing
// document: the same author gate, the same revision check, the same CAS.
// Routes and policies share ONE revision so an edit in either tab refuses a
// stale edit in the other -- two revisions would let a route change race a
// policy change that the route's sources read.
func (r *PolicyRegistry) changeDocument(ctx context.Context, expected int64, edit func(*PolicyDocument) error) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return fmt.Errorf("changing a route for the cluster requires an authenticated owner or developer")
	}
	doc, defaults, err := r.configuration(ctx)
	if err != nil {
		return err
	}
	if expected < 0 || doc.Revision != expected {
		return fmt.Errorf("routing changed since revision %d; refresh and review before saving", expected)
	}
	if err := edit(&doc); err != nil {
		return err
	}
	if _, err := policySnapshot(defaults, doc); err != nil {
		return err
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return fmt.Errorf("persistent routing storage is not configured")
	}
	doc.Revision = expected + 1
	doc.UpdatedBy = ac.UserId
	return store.CompareAndSwap(ctx, expected, doc)
}

// routeExecutor is the one engine method the personal-route reads need, so the
// router can hand its own engine in.
type routeExecutor interface {
	Execute(ctx context.Context, query string) (*ExecuteResult, error)
}

// RouteMaintenanceContext is the principal a personal route is read under when
// the router resolves a call: the cluster's maintenance actor, because the call
// may run on a planner or an agent replica that holds nothing of the person's,
// and a route row declares the composite tier that admits that principal. The
// read always names the owner, so it never returns somebody else's route.
func RouteMaintenanceContext(ctx context.Context) context.Context {
	const actorId = "system:maintenance:routes"
	claims := map[string]any{"sub": actorId, "email": actorId, "role": "system"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: actorId, Role: auth.RoleOwner, Unranked: true, Synthetic: true})
	return auth.ContextWithInternalOrigin(ctx)
}

// ReadRouterCallSignals reads the decisions made since a time, newest first, in
// the shape Auto ranks on. It is read under the maintenance principal because
// the signals are the cluster's, not the caller's: every replica ranks on the
// same evidence whoever's call it is resolving.
func ReadRouterCallSignals(ctx context.Context, exec routeExecutor, since time.Time) ([]map[string]any, error) {
	if exec == nil {
		return nil, nil
	}
	q, err := languageParser.RenderCall("routerCallsForSignals", map[string]any{"since": since.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	res, err := exec.Execute(RouteMaintenanceContext(ctx), "query "+q)
	if err != nil {
		return nil, fmt.Errorf("read recent decisions: %w", err)
	}
	return MaterializeRows(res), nil
}

// canonicalRouteId turns a bare route id into the row's canonical id.
func canonicalRouteId(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, RouteConceptID+":") {
		return id
	}
	return RouteConceptID + ":" + id
}

// ReadPersonalRoute reads one person's route. found is false when the route
// does not exist, was deleted, or is somebody else's.
func ReadPersonalRoute(ctx context.Context, exec routeExecutor, owner, id string) (RouteSpec, bool, error) {
	owner, id = strings.TrimSpace(owner), strings.TrimSpace(id)
	if exec == nil || owner == "" || id == "" || !ValidRouteId(id) || IsBuiltinRouteId(id) {
		return RouteSpec{}, false, nil
	}
	q, err := languageParser.RenderCall("routeForOwner", map[string]any{"routeId": canonicalRouteId(id), "ownerUserId": owner})
	if err != nil {
		return RouteSpec{}, false, err
	}
	res, err := exec.Execute(RouteMaintenanceContext(ContextWithFreshRead(ctx)), "query "+q)
	if err != nil {
		return RouteSpec{}, false, fmt.Errorf("read route %s: %w", id, err)
	}
	rows := MaterializeRows(res)
	if len(rows) == 0 {
		return RouteSpec{}, false, nil
	}
	return personalRouteFromRow(rows[0]), true, nil
}

// readPersonalRoutes reads every live route one person has.
func readPersonalRoutes(ctx context.Context, exec routeExecutor, owner string) ([]RouteSpec, error) {
	owner = strings.TrimSpace(owner)
	if exec == nil || owner == "" {
		return nil, nil
	}
	q, err := languageParser.RenderCall("routesOfOwner", map[string]any{"ownerUserId": owner})
	if err != nil {
		return nil, err
	}
	res, err := exec.Execute(RouteMaintenanceContext(ContextWithFreshRead(ctx)), "query "+q)
	if err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	var out []RouteSpec
	for _, row := range MaterializeRows(res) {
		out = append(out, personalRouteFromRow(row))
	}
	return out, nil
}

func personalRouteFromRow(row map[string]any) RouteSpec {
	strs := func(key string) []string {
		var out []string
		switch v := row[key].(type) {
		case []string:
			out = append(out, v...)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	str := func(key string) string { s, _ := row[key].(string); return strings.TrimSpace(s) }
	id := BareShortId(str("id"))
	return RouteSpec{
		Id:          id,
		Name:        str("name"),
		Kind:        RouteKindCustom,
		Scope:       RouteScopePersonal,
		Description: "",
		Jobs:        RouteJobs{Fast: strs("fast"), Strong: strs("strong"), Reasoning: strs("reasoning")},
		OwnerUserId: str("ownerUserId"),
		UpdatedAt:   str("updatedAt"),
	}
}

// writePersonalRoute writes one person's route whole, under internal origin
// and the CALLER'S actor: the mutation stamps the owner from actor.userId, so
// ctx must be the person whose route it is.
func writePersonalRoute(ctx context.Context, exec routeExecutor, spec RouteSpec, active bool) error {
	q, err := languageParser.RenderCall("routeWrite", map[string]any{
		"routeId":   canonicalRouteId(spec.Id),
		"name":      spec.Name,
		"fast":      spec.Jobs.Fast,
		"strong":    spec.Jobs.Strong,
		"reasoning": spec.Jobs.Reasoning,
		"active":    active,
		"updatedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	_, err = exec.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+q)
	return err
}

// routeSummaryJSON is the wire shape of one route (RouteSummary in the design
// record). Kept here, beside RouteSpec, so the builtins and their tests agree.
type routeSummaryJSON struct {
	Id          string         `json:"id"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Scope       string         `json:"scope"`
	Description string         `json:"description"`
	Ready       bool           `json:"ready"`
	Reason      string         `json:"reason"`
	Customized  bool           `json:"customized"`
	Jobs        []routeJobJSON `json:"jobs"`
}

type routeJobJSON struct {
	Job       string   `json:"job"`
	Sources   []string `json:"sources"`
	Editable  bool     `json:"editable"`
	ServesNow string   `json:"servesNow"`
	Ready     bool     `json:"ready"`
}

func (s routeSummaryJSON) asMap() map[string]any {
	raw, _ := json.Marshal(s)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
