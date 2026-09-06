package work

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// store.go -- the engine seam, and the TWO actor rules the work spine turns
// on. Both are load-bearing and both fail SILENTLY when they are missing,
// which is why they live in one file with one stamp site each rather than
// being spelled at every call.
//
// ===========================================================================
// RULE 1: EVERY WRITE NEEDS INTERNAL ORIGIN, BECAUSE EVERY WORK MUTATION IS
// @serverOnly
// ===========================================================================
// dsl/work/mutations.memql declares all nine writers @serverOnly.
// auth.OriginFromContext defaults to OriginClient (the zero value), and the
// function validator refuses a @serverOnly construct on any other origin with
// ONE WARN and carries on. For a write that means the row is never inserted
// and nothing above it hears about it: a goal that reports an id nothing
// stored, a decision the run never sees, a sweep that closes nothing.
//
// So executeInternal below stamps auth.ContextWithInternalOrigin INLINE, as
// the argument to the one Execute that needs it. That is the whole of the
// stamping in this package (TestTheStampNeverEscapesItsCall counts the site
// and requires exactly one). The marked context is a LOCAL and is never
// returned: a returned one would be inherited by a later frame and would open
// every OTHER @serverOnly construct in the tree for the rest of the call,
// which is the memql#2879 / memql#2989 escalation.
//
// ===========================================================================
// RULE 2: EVERY READ AND EVERY OWNED WRITE NEEDS AN ACTOR THAT IS THE OWNER
// ===========================================================================
// Every work concept declares @rowAuthz(owner="ownerUserId", clusterOwner).
//
//   - The READ gate (component/memql/rowauthz_enforce.go) has NO
//     internal-origin bypass and answers "no identity, no rows". An unstamped
//     read returns ZERO ROWS AND NO ERROR -- which reads as "the goal has no
//     runs", "there is nothing parked", "nothing to archive".
//   - The WRITE guard ignores the second argument (memql#4312), so a cluster
//     owner does NOT get to rewrite somebody else's owned row. A write onto a
//     run belonging to a person must run AS that person.
//
// Hence ownerActor: the engine BORROWS the row owner's authority rather than
// out-ranking it, exactly as component/worker's store and component/campaigns'
// drain worker do. The owner value is always COPIED OFF A ROW THE CALLER
// ALREADY READ UNDER THEIR OWN ACTOR, so it can never name a user the caller
// could not act as -- it is never taken off a request field.
//
// An EMPTY owner is left alone rather than substituted. A present-and-empty
// ownerUserId is the deployment's own row (the concepts say so), and stamping
// a synthetic actor over it would turn a meaningful empty into a false name.
//
// Reads are DELIBERATELY UNSTAMPED for origin. Stamping internal origin on a
// read would widen it silently and move the decision away from the actor,
// which is the failure nobody notices.

// Engine is the narrow surface this package needs. *memql.MemQLEngine
// satisfies it directly.
type Engine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// store runs the work namespace's constructs against the engine.
type store struct {
	engine Engine
}

// ---------------------------------------------------------------------------
// Actors
// ---------------------------------------------------------------------------

// ownerActor stamps the row owner's actor on ctx.
//
// A blank owner returns ctx UNCHANGED and says so, rather than erroring:
// auth.ContextWithUserActor is itself a no-op for a blank id, and a
// present-and-empty ownerUserId is a legitimate state on every work concept
// (the deployment's own run). Callers that need a non-empty owner check for
// one themselves and say what they were doing.
func ownerActor(ctx context.Context, ownerUserId string) context.Context {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return ctx
	}
	return auth.ContextWithUserActor(ctx, owner)
}

// callerUserId is the authenticated caller, or "" when there is none.
func callerUserId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	return strings.TrimSpace(ac.UserId)
}

// requirePrincipal admits a signed-in person and refuses everything else.
//
// The floor is the HANDLER's because a builtin's annotation set carries no
// @requiresRank (the logs plug-in's reasoning, component/logstore/plugin.go).
// It is deliberately NOT a rank floor: these capabilities are caller-scoped,
// and the row tier is what decides which goals are the caller's. A connector
// or an anonymous actor is refused by name so the reason is legible.
func requirePrincipal(ctx context.Context) (*auth.AccessContext, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return nil, fmt.Errorf("work: no authenticated caller")
	}
	if ac.IsAnonymousActor() || ac.IsConnector() || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("work: an anonymous or connector actor may not act on goals")
	}
	return ac, nil
}

// requireClusterOwner is the sweeps' floor.
//
// The cluster's MAINTENANCE PRINCIPAL clears it: auth.MaintenanceActor carries
// RoleOwner precisely so the composite tier's escape admits it, and both work
// sweeps are named in component/auth/maintenance_actor.go. So the scheduled
// automation passes, an owner running the sweep by hand passes, and nobody
// else does -- the same shape component/logstore's requireOwner has, for the
// same reason: these reads span every owner by nature.
func requireClusterOwner(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("work: no authenticated caller")
	}
	if ac.IsClusterOwner() {
		return nil
	}
	return fmt.Errorf("work: role %q may not run this sweep; it is reserved to a cluster owner (the scheduled automation runs under the cluster's maintenance principal)", string(ac.Role))
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// executeInternal runs one @serverOnly construct under a context this function
// constructs. THE ONE STAMP SITE IN THIS PACKAGE -- see the header. The marked
// context dies at the Execute it is passed to; this returns a RESULT, never a
// context.
func (s *store) executeInternal(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("work: engine not configured")
	}
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(query), err)
	}
	return memqlRows(res), nil
}

// writeInternal runs one @serverOnly mutation. A wrapper rather than a second
// stamp, which is what keeps the call-site count at one.
func (s *store) writeInternal(ctx context.Context, query string) error {
	_, err := s.executeInternal(ctx, query)
	return err
}

// query runs a read UNSTAMPED, so the actor in ctx decides the scope. See
// RULE 2 in the header: stamping here would widen the read silently.
func (s *store) query(ctx context.Context, q string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("work: engine not configured")
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstConstruct(q), err)
	}
	return memqlRows(res), nil
}

// queryInternal runs a @serverOnly READ. The stamp opens the construct; the
// ACTOR in ctx still decides which rows come back, because the read gate has
// no internal-origin bypass.
func (s *store) queryInternal(ctx context.Context, q string) ([]map[string]any, error) {
	return s.executeInternal(ctx, q)
}

// ---------------------------------------------------------------------------
// Reads (caller-scoped: run UNSTAMPED under whatever actor ctx carries)
// ---------------------------------------------------------------------------

func (s *store) goalForOwner(ctx context.Context, goalId string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("workGoalForOwner", map[string]any{"goalId": goalId})))
}

func (s *store) runsForGoal(ctx context.Context, goalId string) ([]map[string]any, error) {
	return s.query(ctx, "query "+call("workRunsForGoal", map[string]any{"goalId": goalId}))
}

func (s *store) runForOwner(ctx context.Context, runId string) (map[string]any, error) {
	return one(s.query(ctx, "query "+call("workRunForOwner", map[string]any{"runId": runId})))
}

// pendingApprovalsForOwner is the caller's own undecided approvals.
//
// The id-addressed read (workApprovalById) is @serverOnly, and reaching for it
// here would be the wrong shape twice over: it would need the stamp, and the
// stamp buys nothing on a read -- the ACTOR decides the rows. This list is
// already owner-filtered in its own predicate, so finding the approval in it
// IS the ownership check.
func (s *store) pendingApprovalsForOwner(ctx context.Context) ([]map[string]any, error) {
	return s.query(ctx, "query workApprovalsForOwner()")
}

// ---------------------------------------------------------------------------
// Writes (@serverOnly: internal origin, plus the owner's borrowed authority)
// ---------------------------------------------------------------------------

// createGoalRow inserts v1:work:goal. ownerUserId is NOT an argument: the
// concept marks it @serverSet and createWorkGoal stamps it from actor.userId,
// so the owner reaches the row through the caller's own actor and through
// nothing else.
func (s *store) createGoalRow(ctx context.Context, g goalSeed) error {
	return s.writeInternal(ctx, "mutation "+call("createWorkGoal", map[string]any{
		"goalId":           g.GoalId,
		"statement":        g.Statement,
		"origin":           g.Origin,
		"responsibilityId": g.ResponsibilityId,
		"accountIds":       optStrings(g.AccountIds),
		"input":            optMap(g.Input),
		"ceilings":         optMap(g.Ceilings),
		"requestedVia":     g.RequestedVia,
	}))
}

// createRunRow inserts v1:work:run. Same @serverSet rule as the goal: the
// owner arrives through the actor, which is why every caller of this passes a
// context already stamped with ownerActor.
func (s *store) createRunRow(ctx context.Context, r runSeed) error {
	return s.writeInternal(ctx, "mutation "+call("createWorkRun", map[string]any{
		"runId":               r.RunId,
		"goalId":              r.GoalId,
		"automationName":      r.AutomationName,
		"templateFingerprint": r.TemplateFingerprint,
		"input":               optMap(r.Input),
		"inputFingerprint":    r.InputFingerprint,
		"triggeredBy":         r.TriggeredBy,
		"mode":                r.Mode,
		"forkedFromRunId":     r.ForkedFromRunId,
		"forkAtStepKey":       r.ForkAtStepKey,
		"status":              r.Status,
		"nodeId":              r.NodeId,
		"startedAt":           rfc(r.StartedAt),
	}))
}

// updateRun is the read-merge advance. Every field NOT named keeps its prior
// value, so callers pass only what changed.
func (s *store) updateRun(ctx context.Context, runId string, fields map[string]any) error {
	args := map[string]any{"runId": runId}
	maps.Copy(args, fields)
	return s.writeInternal(ctx, "mutation "+call("updateWorkRun", args))
}

// decideApprovalRow records a decision on v1:work:approval.
func (s *store) decideApprovalRow(ctx context.Context, approvalId, decision, decidedBy string, at time.Time, answer map[string]any) error {
	return s.writeInternal(ctx, "mutation "+call("decideWorkApproval", map[string]any{
		"approvalId": approvalId,
		"decision":   decision,
		"decidedBy":  decidedBy,
		"decidedAt":  rfc(at),
		"answer":     optMap(answer),
	}))
}

// createApprovalRow raises a human gate.
func (s *store) createApprovalRow(ctx context.Context, a approvalSeed) error {
	return s.writeInternal(ctx, "mutation "+call("createWorkApproval", map[string]any{
		"approvalId":   a.ApprovalId,
		"runId":        a.RunId,
		"stepKey":      a.StepKey,
		"kind":         a.Kind,
		"subject":      optMap(a.Subject),
		"artifactHash": a.ArtifactHash,
		"question":     a.Question,
		"options":      optMaps(a.Options),
		"evidence":     optMap(a.Evidence),
		"requestedAt":  rfc(a.RequestedAt),
		"expiresAt":    rfcOrEmpty(a.ExpiresAt),
	}))
}

// ---------------------------------------------------------------------------
// Seeds
// ---------------------------------------------------------------------------

type goalSeed struct {
	GoalId           string
	Statement        string
	Origin           string
	ResponsibilityId string
	AccountIds       []string
	Input            map[string]any
	Ceilings         map[string]any
	RequestedVia     string
}

type runSeed struct {
	RunId               string
	GoalId              string
	AutomationName      string
	TemplateFingerprint string
	Input               map[string]any
	InputFingerprint    string
	TriggeredBy         string
	Mode                string
	ForkedFromRunId     string
	ForkAtStepKey       string
	Status              string
	NodeId              string
	StartedAt           time.Time
	// OwnerUserId is NOT written as an argument -- createWorkRun stamps it
	// from actor.userId. It is carried here so the caller can build the
	// borrowed-authority context from the same value it read off the goal.
	OwnerUserId string
}

type approvalSeed struct {
	ApprovalId   string
	RunId        string
	StepKey      string
	Kind         string
	Subject      map[string]any
	ArtifactHash string
	Question     string
	Options      []map[string]any
	Evidence     map[string]any
	RequestedAt  time.Time
	ExpiresAt    time.Time
}

// ---------------------------------------------------------------------------
// Call composition
// ---------------------------------------------------------------------------

// call composes `name(k: v, ...)` with bare-identifier keys and JSON-encoded
// values, keys sorted so a log line is stable.
//
// STRINGS GO THROUGH langparser.QuoteString, never Go's %q. The two escape
// grammars diverge on four control bytes -- %q emits \x00, \a, \v and \xNN,
// and the MemQL lexer's readString implements the JSON escapes and rejects
// every one of them, so one such byte in an interpolated value makes the whole
// statement unparseable (memql#3035, memql#3611). A goal statement is a
// person's free text, so this is not a theoretical difference here.
func call(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	first := true
	for _, k := range keys {
		v := args[k]
		// A nil argument is DROPPED, never rendered as `null`. An optional
		// object field given null fails the concept's type check and the
		// engine refuses the whole row, so "the caller said nothing" and "the
		// caller said null" must not render the same way. The typed-nil arms
		// matter as much as the untyped one: a nil map inside an `any` is not
		// == nil.
		if v == nil || isNilValue(v) {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(literal(v))
	}
	b.WriteByte(')')
	return b.String()
}

// isNilValue reports whether v is a typed nil map or slice.
func isNilValue(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return t == nil
	case []map[string]any:
		return t == nil
	case []string:
		return t == nil
	case []any:
		return t == nil
	}
	return false
}

func literal(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return "null"
	}
	return string(b)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// firstConstruct names the construct in an error, so a failure says which call
// failed rather than only that one did.
func firstConstruct(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " ("); i > 0 {
		head := q[:i]
		rest := strings.TrimSpace(q[i:])
		if j := strings.IndexByte(rest, '('); j > 0 {
			return head + " " + strings.TrimSpace(rest[:j])
		}
		return head
	}
	return "work"
}

// ---------------------------------------------------------------------------
// Row decoding
// ---------------------------------------------------------------------------

// memqlRows normalises whatever the engine handed back into plain rows.
//
// EVERY WORK QUERY CARRIES A SHAPE, and a shaped query returns `output` rows
// and NILS the GraphBundle -- so a caller that only handled the bundle branch
// reads an empty result from a perfectly good query. Both branches are
// reachable here: the reads are shaped, and a mutation answers with a bundle.
func memqlRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memqlengine.ExecuteResult); ok {
		if res == nil {
			return nil
		}
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
			if payload := n.GetPayload(); payload != nil {
				maps.Copy(row, payload.AsMap())
			}
			out = append(out, row)
		}
		return out
	}
	switch v := raw.(type) {
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
		if rows, ok := v["rows"].([]any); ok {
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		// A builtin reply and a single shaped row both arrive as ONE
		// id-keyed map; unwrap it so a caller sees rows either way.
		if looksIdKeyed(v) {
			out := make([]map[string]any, 0, len(v))
			for id, r := range v {
				m, ok := r.(map[string]any)
				if !ok {
					continue
				}
				if _, has := m["id"]; !has {
					m["id"] = id
				}
				out = append(out, m)
			}
			return out
		}
		return []map[string]any{v}
	}
	return nil
}

// looksIdKeyed reports whether every value of m is itself a row map -- the
// shape the engine returns for a keyed result set. A flat row (whose values
// are scalars) is not, and must not be exploded into its own fields.
func looksIdKeyed(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return true
}

func one(rows []map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func rowString(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func rowMap(row map[string]any, key string) map[string]any {
	if row == nil {
		return nil
	}
	if m, ok := row[key].(map[string]any); ok {
		return m
	}
	return nil
}

func rowTime(row map[string]any, key string) (time.Time, bool) {
	raw := rowString(row, key)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// Argument shaping
// ---------------------------------------------------------------------------

// optMap, optMaps and optStrings render an OPTIONAL object / array argument:
// present when the caller supplied one, and ABSENT otherwise.
//
// Absent, not empty, and the difference is a refused row rather than a
// cosmetic one. call() drops a nil argument entirely; these return nil so it
// does. Rendering `null` instead fails the concept's type check and the engine
// refuses the WHOLE row ("expected object, but got null"), and rendering `{}`
// writes an empty object over a field the caller said nothing about -- which
// on a read-merge update is a CLEAR the caller did not ask for.
//
// A caller that genuinely means "clear this" passes an explicit empty map
// through the fields map instead (resumeParkedRun's `waitingOn: {}`), which
// call() renders because a non-nil empty map is not nil.
func optMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func optMaps(m []map[string]any) []map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func optStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func rfc(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// rfcOrEmpty answers "" for the zero time rather than year 1, so an absent
// expiry is absent rather than long past.
func rfcOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return rfc(t)
}
