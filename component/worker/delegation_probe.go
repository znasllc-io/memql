package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// delegation_probe.go answers the planner's two live delegation
// questions (memql#4362).
//
// IT LIVES HERE, UNTAGGED, FOR A REASON. Worker streams terminate on the
// AGENT node, but Tasks are emitted by the PLANNER node, which runs its
// own binary (app/build_planner.go) and holds no registry at all. A
// probe compiled only into the agent build would answer "no machine" on
// every planner node in a real cluster: delegation would be inert in the
// deployed topology while passing every single-process test. That is the
// exact failure class CLAUDE.md's multi-node section describes, and it
// is why this file carries no build tag and reads ROWS.
//
// Two paths, and the order matters:
//
//   - The in-memory registry, when this node holds worker streams. It is
//     authoritative and current to the last heartbeat.
//   - The persisted registration row otherwise, keyed on the `app:`
//     labels the engine derives and stores alongside the inventory.
//     Those labels are written on the same beat the registry is updated
//     and OUTSIDE the lastSeenAt throttle (see server.go), precisely so
//     a reader that is not holding the stream gets the same answer.

// RegistrationFreshFor is how long after its last heartbeat a persisted
// registration still counts as online for triage.
//
// Generous relative to the 15s beat because the DB flush is throttled to
// once a minute: a perfectly healthy machine can have a row up to a
// minute stale, and a tighter window would refuse delegation to a
// machine that is right there. The two errors cost different amounts --
// being wrong this way costs one in-process fallback, being wrong the
// other way parks a Task on a laptop that is gone.
const RegistrationFreshFor = 3 * time.Minute

// ProbeEngine is the narrow read surface the probe needs.
type ProbeEngine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// DelegationProbe answers "is a machine with this app online" and "how
// many sessions does this user have live". It satisfies the planner's
// DelegationProbe interface.
type DelegationProbe struct {
	// Registry is the live registry when this node holds worker streams.
	// Nil on a planner node -- the case the row path serves.
	Registry *Registry
	Engine   ProbeEngine
}

// NewDelegationProbe builds a probe. A nil registry is normal and
// expected: it is what a planner node passes.
func NewDelegationProbe(registry *Registry, engine ProbeEngine) *DelegationProbe {
	return &DelegationProbe{Registry: registry, Engine: engine}
}

// FindMachineForApp returns an online machine that can actually run
// appId, or "" when none can. "Can actually run" is the same
// allowed-AND-signed-in test selection uses, so the triage cannot
// promise a machine that dispatch would then refuse.
func (p *DelegationProbe) FindMachineForApp(ctx context.Context, ownerUserId, appId string) string {
	if p == nil {
		return ""
	}
	if p.Registry != nil {
		if id := firstConnectedRunning(p.Registry, ownerUserId, appId); id != "" {
			return id
		}
		// A node WITH a registry and no local match still falls through
		// to the rows. In a multi-replica mesh the machine may be
		// connected to a DIFFERENT agent replica, and answering "no
		// machine" because it is not on this one is precisely the
		// single-node assumption the row path exists to avoid.
	}
	return p.findMachineInRows(ctx, ownerUserId, appId)
}

func (p *DelegationProbe) findMachineInRows(ctx context.Context, ownerUserId, appId string) string {
	if p.Engine == nil || strings.TrimSpace(ownerUserId) == "" || !IsKnownAppId(appId) {
		return ""
	}
	rows := p.query(ctx, ownerUserId,
		fmt.Sprintf(`query workersForUser(ownerUserId:%s)`, langparser.QuoteString(ownerUserId)))
	now := time.Now().UTC()
	for _, row := range rows {
		if !RegistrationIsUsable(row, now) {
			continue
		}
		// The derived LABEL, not the raw inventory. Re-deriving
		// runnability here would be a second copy of the
		// allowed-AND-signed-in rule, and two copies disagree the first
		// time one changes. The label is the engine's own answer.
		labels := stringKeyedMap(row["labels"])
		if labels == nil {
			continue
		}
		if _, ok := labels[AppLabelKey(appId)]; !ok {
			continue
		}
		if id, _ := row["id"].(string); id != "" {
			return id
		}
	}
	return ""
}

// LiveSessionCount returns how many app sessions the user currently has
// open across every machine.
//
// It reads the ROW, never a local count: sessions live on whichever
// agent replica holds the machine's stream, so counting in memory would
// give each replica a private view of a cap that is per-user across the
// cluster. A read failure returns 0, which fails OPEN -- the cap is a
// courtesy limit on the user's own machines, and refusing their work
// because a query blipped is the worse error.
func (p *DelegationProbe) LiveSessionCount(ctx context.Context, ownerUserId string) int {
	if p == nil || p.Engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return 0
	}
	return len(p.query(ctx, ownerUserId, "query liveAppSessionsForUser()"))
}

// query runs a caller-scoped read under the OWNER's actor. The engine
// borrows their authority rather than out-ranking them with a system
// read that would work for anyone else's rows too.
func (p *DelegationProbe) query(ctx context.Context, ownerUserId, q string) []map[string]any {
	res, err := p.Engine.Execute(auth.ContextWithUserActor(ctx, ownerUserId), q)
	if err != nil || res == nil {
		return nil
	}
	if rows := payloadRows(res.OutputPayload()); len(rows) > 0 {
		return rows
	}
	// A shape() query lands on the Data axis, but a construct without one
	// lands on Bundle.Nodes. Reading only the axis this call happens to
	// use today makes the probe silently empty the first time the query's
	// projection changes.
	return nodeRows(res)
}

// RegistrationIsUsable reports whether a persisted registration is
// neither revoked nor stale.
func RegistrationIsUsable(row map[string]any, now time.Time) bool {
	if revoked, _ := row["revokedAt"].(string); strings.TrimSpace(revoked) != "" {
		return false
	}
	raw, _ := row["lastSeenAt"].(string)
	lastSeen, ok := parseRowTime(raw)
	if !ok {
		// A registration with no readable lastSeenAt is NOT fresh. An
		// unparseable timestamp reading as "online" would delegate to a
		// machine nobody has heard from.
		return false
	}
	return now.Sub(lastSeen) <= RegistrationFreshFor
}

func parseRowTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func payloadRows(payload any) []map[string]any {
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

func nodeRows(res *memqlengine.ExecuteResult) []map[string]any {
	if res == nil || res.Bundle == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		row := nodePayload(node)
		if row == nil {
			continue
		}
		if _, ok := row["id"]; !ok && node.GetId() != "" {
			row["id"] = node.GetId()
		}
		out = append(out, row)
	}
	return out
}

func nodePayload(node *memqlv1.MemoryNode) map[string]any {
	if node == nil || node.Payload == nil {
		return nil
	}
	raw, err := json.Marshal(node.Payload.AsMap())
	if err != nil {
		return nil
	}
	var row map[string]any
	if json.Unmarshal(raw, &row) != nil {
		return nil
	}
	return row
}

func stringKeyedMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// DelegationPreference reads the owner's stored delegation policy and
// renders it in the planner's triage shape (memql#4362).
//
// It lives here, untagged, for the same reason the probe does: the
// PLANNER node runs the triage and cannot compile an agent-tagged
// package. A reader available only on the agent would leave every
// planner answering "delegation off" regardless of what the user saved.
//
// A read failure yields the ZERO preference, which reads as delegation
// OFF. Failing the other way would delegate somebody's work to their
// laptop because a query blipped.
// StoredPreference is the delegation policy as this package reads it.
//
// It is a LOCAL struct rather than the planner's own type on purpose:
// component/worker is the protocol and registry layer, and depending on
// component/planner would invert the layering (and add a module edge
// worker -> planner that nothing else wants). The conversion happens in
// app/, which already imports both -- glue belongs where the wiring is.
type StoredPreference struct {
	Enabled               bool
	EligibleKinds         []string
	AppOrder              []string
	MaxConcurrentSessions int
}

func (p *DelegationProbe) DelegationPreference(ctx context.Context, ownerUserId string) StoredPreference {
	if p == nil || p.Engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return StoredPreference{}
	}
	rows := p.query(ctx, ownerUserId, "query delegationPolicyForUser()")
	if len(rows) == 0 {
		// An ABSENT row is "never delegate", not "use the defaults". A
		// user who has never opened the policy editor is not delegating,
		// which is the state they should be surprised by least.
		return StoredPreference{}
	}
	row := rows[0]
	enabled, _ := row["preferSubscriptionApps"].(bool)
	return StoredPreference{
		Enabled:               enabled,
		EligibleKinds:         stringSliceFrom(row["eligibleKinds"]),
		AppOrder:              stringSliceFrom(row["appOrder"]),
		MaxConcurrentSessions: intFromRow(row["maxConcurrentSessions"]),
	}
}

func stringSliceFrom(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// intFromRow reads an integer off a graph row.
//
// The narrowing is SATURATING rather than bare (CodeQL
// go/incorrect-integer-conversion). Both wider cases are reachable with a
// value int cannot hold -- JSON decodes every number to float64, and these
// rows are written by cockpits reporting their own capability caps -- while a
// bare conversion wraps on a 32-bit build and is implementation-defined for
// an out-of-range float. A concurrency cap that wrapped negative would read
// as "this machine can run nothing" and take a signed-in worker silently out
// of the fleet, which is the failure this file exists to report on.
func intFromRow(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return num.ClampInt64(n)
	case float64:
		return num.ClampFloat64(n)
	}
	return 0
}

// firstConnectedRunning returns the id of a worker on THIS replica that
// can actually run appId, or "".
//
// It is an existence check, not routing. Choosing BETWEEN machines is the
// Fleet router's job (memql#4350) -- strategy, policy labels, load -- and
// re-implementing any of that here would be a second router that
// disagrees with the first. The triage only needs to know whether
// delegating is possible at all; the executor asks the router which
// machine when the Task actually runs.
func firstConnectedRunning(r *Registry, ownerUserId, appId string) string {
	for _, w := range r.WorkersForUser(ownerUserId) {
		if !w.SupportsCapability(CapabilityHeadless) {
			continue
		}
		if w.RunsApp(appId) {
			return w.RegistrationId
		}
	}
	return ""
}
