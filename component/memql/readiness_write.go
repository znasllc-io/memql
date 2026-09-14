package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/envregistry"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// SetReadinessIdentity names this node in the rows it writes.
func (e *MemQLEngine) SetReadinessIdentity(nodeId, nodeType string) {
	if e == nil {
		return
	}
	e.readinessNodeId = strings.TrimSpace(nodeId)
	e.readinessNodeType = strings.TrimSpace(nodeType)
}

// readinessIdentity answers who this node is for the purposes of a readiness
// row. The fallbacks matter: a row keyed on an empty node id would collide
// with every other node's row for the same module, and the fold would then be
// reading one node's opinion while believing it had read the cluster's.
func (e *MemQLEngine) readinessIdentity() (string, string) {
	nodeId, nodeType := e.readinessNodeId, e.readinessNodeType
	if nodeId == "" {
		nodeId = strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
	}
	if nodeId == "" {
		if h, err := os.Hostname(); err == nil {
			nodeId = h
		}
	}
	if nodeType == "" {
		nodeType = envregistry.ResolveNodeType()
	}
	return nodeId, nodeType
}

// readinessRowID is the deterministic id a rewrite versions. Deterministic so
// a re-evaluation appends a new VERSION of one logical row rather than a
// second row -- the same reason the cluster singletons sit at literal ids.
// readinessRewriteFloor bounds how long an unchanged verdict may stand without
// being restated. Below it, a rewrite that would carry the same state, core
// flag and lanes as the row already holds is SKIPPED: the fleet's steady state
// used to append a version per module per node on every trigger (772k
// v1:platform:moduleReadiness versions for a few dozen live ids on
// a production instance, 2026-09-13), and every one of those rows was news to nobody.
// At or past the floor the row is restated so `reportedAt` never reads as
// abandoned.
const readinessRewriteFloor = 10 * time.Minute

// readinessRewriteNeeded decides whether next must be written over prev. A nil
// prev (no standing row, or the read failed) always writes -- the write is
// idempotent and the cost of a needless one is a version, while the cost of a
// missed one is a stale verdict.
func readinessRewriteNeeded(prev *readiness.NodeReport, next readiness.NodeReport, now time.Time) bool {
	if prev == nil {
		return true
	}
	if prev.State != next.State || prev.Reason != next.Reason || prev.Core != next.Core || !readinessLanesEqual(prev.Lanes, next.Lanes) {
		return true
	}
	return now.Sub(prev.ReportedAt) >= readinessRewriteFloor
}

func readinessLanesEqual(a, b []readiness.LaneReport) bool {
	if len(a) != len(b) {
		return false
	}
	ra, err := json.Marshal(a)
	if err != nil {
		return false
	}
	rb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ra) == string(rb)
}

// readinessStanding is what this node's rows said before a pass: its latest
// row per module, and whether the read that answered that question landed.
//
// READABLE IS A SEPARATE FACT FROM EMPTY, and the difference is load-bearing
// for exactly one decision. "No row for this module" lets an unknown verdict
// be written, so a fresh pod can say "could not evaluate" instead of nothing;
// "the read failed" must NOT, because a standing row we could not see may be
// a correct known verdict that unknown would then replace.
type readinessStanding struct {
	rows     map[string]*readiness.NodeReport
	readable bool
}

// knownRow reports whether the standing row for module is a KNOWN verdict --
// any state but Unknown -- or whether that cannot be ruled out because the
// read failed.
func (s readinessStanding) knownRow(module string) bool {
	if !s.readable {
		return true
	}
	r := s.rows[module]
	return r != nil && r.State != readiness.Unknown
}

// standingReadiness reads this node's current verdict per module. A failed
// read answers an empty, UNREADABLE standing, which makes every known module
// rewrite -- the pre-existing behaviour -- rather than skipping on a guess, and
// holds every unknown one back.
func (e *MemQLEngine) standingReadiness(ctx context.Context, nodeId string) readinessStanding {
	out := readinessStanding{rows: map[string]*readiness.NodeReport{}}
	reports, err := e.readModuleReadinessRows(ctx)
	if err != nil {
		e.safeLogger().Warn("module readiness: standing rows unreadable; rewriting every known module", "error", err)
		return out
	}
	out.readable = true
	for i := range reports {
		if reports[i].NodeId == nodeId {
			r := reports[i]
			out.rows[r.Module] = &r
		}
	}
	return out
}

func readinessRowID(module, nodeId string) string {
	return ModuleReadinessConcept + ":" + module + "--" + nodeId
}

// renderRecordModuleReadiness renders the @serverOnly call as MemQL TEXT.
// Every string goes through QuoteString -- the lexer's own escaping, which
// diverges from Go's %q on four control characters (memql#4256) -- and the
// lanes ride as a JSON literal, which the parser accepts as a list of objects.
//
// `reason` is rendered only when the report carries one (an Unknown verdict),
// so a known verdict's call is byte-for-byte what it was before the field.
func renderRecordModuleReadiness(r readiness.NodeReport, rowId string) (string, error) {
	lanes := r.Lanes
	if lanes == nil {
		lanes = []readiness.LaneReport{}
	}
	raw, err := json.Marshal(lanes)
	if err != nil {
		return "", err
	}
	reason := ""
	if r.Reason != "" {
		reason = ", reason: " + langparser.QuoteString(r.Reason)
	}
	return fmt.Sprintf(
		"mutation recordModuleReadiness(rowId: %s, module: %s, nodeId: %s, nodeType: %s, state: %s%s, core: %t, lanes: %s, reportedAt: %s)",
		langparser.QuoteString(rowId),
		langparser.QuoteString(r.Module),
		langparser.QuoteString(r.NodeId),
		langparser.QuoteString(r.NodeType),
		langparser.QuoteString(string(r.State)),
		reason,
		r.Core,
		string(raw),
		langparser.QuoteString(r.ReportedAt.UTC().Format(time.RFC3339)),
	), nil
}

// readinessEvaluateContext is the context the EVALUATION runs under, and it is
// a cluster owner where readinessWriteContext below deliberately is not.
//
// ===========================================================================
// WHY THE EVALUATION NEEDS MORE AUTHORITY THAN THE WRITE
// ===========================================================================
// The write is admitted by @serverOnly plus internal origin onto a public
// concept with no owner field, so a reader is all it needs and all it gets.
// The evaluation now READS v1:worker:registration, which declares the
// composite owner tier -- and under any narrower actor that read does not
// fail, it answers ZERO ROWS AND NO ERROR. A cluster full of machines would
// report `ai` unconfigured on every node, forever, with nothing in any log.
//
// It is also what clears integrations/email's statusAuthorized, and that fixed
// a second bug by the same line. app/run.go's boot write passes
// context.Background(); the evaluation ran on it; statusAuthorized refuses a
// context with no AccessContext; and evaluateModule maps an errored probe to
// notApplicable. So `email` read "not applicable" on every node of every
// cluster, permanently. The cause was recorded as a lazy-sender timing
// problem and is not one -- integrations/email/status.go's describer is a
// reproduction of the resolution algorithm that never materializes a sender,
// so timing cannot reach it. See readiness_email_probe_test.go.
//
// ===========================================================================
// WHY IT IS NOT auth.MaintenanceActor
// ===========================================================================
// That constructor is keyed on a compiled-in list of AUTOMATION names, and
// TestMaintenanceAutomationsAreArgued requires every entry to resolve to an
// automation that loads from this repo's dsl/ tree. Readiness is a Go boot
// path, not an automation, so it cannot be listed -- and listing it would
// break the property that makes the list checkable at all.
//
// This is the arrangement seed_materializer.go's systemActorContext already
// uses, for the same reason and with the same shape: a named synthetic cluster
// owner, built in the package that needs it, whose authority comes from being
// compiled in here rather than from anything a caller supplies.
//
// ===========================================================================
// IT REPLACES THE CALLER, WHICH IS THE POINT
// ===========================================================================
// readinessRecompute can be pulled by a cluster owner, and the shipped
// evaluator resolved THEIR machines through the fleet seam and wrote them as a
// node fact. Nothing a caller supplies survives this line, so the same rows
// produce the same verdict however the evaluation was triggered. Asserted by
// TestTheEvaluationContextIsTheClustersOwn.
func readinessEvaluateContext(ctx context.Context) context.Context {
	const actorId = "system:maintenance:moduleReadiness"
	claims := map[string]any{"sub": actorId, "email": actorId, "role": "system"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: actorId,
		// RoleOwner is what buys the composite tier's cluster-owner escape:
		// AccessContext.IsClusterOwner() reads Role == RoleOwner and nothing
		// else, which is why auth.ContextWithUserActor is not a substitute
		// (it hardcodes RoleWriter).
		Role: auth.RoleOwner,
		// Not a principal, so the rank rules do not govern it (D4, epic
		// memql#4832). Without this a rank-strict concept would read this
		// actor as an owner touching a PEER owner's row.
		Unranked: true,
		// And SYNTHETIC: this is the cluster describing itself, so it can
		// never be a row's owner.
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// readinessWriteContext is the engine's own identity for these rows: internal
// origin (what the @serverOnly gate requires) plus a synthetic, unranked
// actor, so the row carries a createdBy and the rank rules do not govern it
// (D4 of the RANK epic). The shape mirrors component/automations'
// contextWithSystemActor.
//
// It REPLACES the caller's actor rather than adding to it, and that is the
// property that makes stamping internal origin here safe on a
// request-derived context (readinessRecompute can be pulled by a cluster
// owner). The stamped context reaches exactly one mutation, every argument of
// which is computed from the manifest and this node's own environment -- no
// caller-supplied value reaches a readiness row, and the caller's authority is
// not carried past this line. Asserted by
// TestReadinessWriteCarriesNoCallerAuthority.
func readinessWriteContext(ctx context.Context) context.Context {
	const actorId = "system:readiness"
	claims := map[string]any{"sub": actorId, "email": actorId, "role": "system"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: actorId,
		// RoleReader, not RoleOwner: this writer needs no cluster-owner
		// escape. The concept is public/requiresIdentity with no owner
		// field, and the write is admitted by @serverOnly plus internal
		// origin, so any more authority than this would be authority
		// nothing here uses.
		Role:      auth.RoleReader,
		Unranked:  true,
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// readinessMemory is this process's record of the KNOWN verdicts it has
// evaluated, per module.
//
// It exists for one rule (persistReadiness): an Unknown verdict is written
// only when nothing known stands for that module. The standing row answers
// that question on every pass -- and a standing read that FAILED already
// counts as "something known may stand" (readinessStanding.knownRow). This is
// the process's own half: a verdict it knows it evaluated still counts when a
// read that did land comes back without the row. The zero value is ready to
// use.
type readinessMemory struct {
	mu    sync.Mutex
	known map[string]readiness.State
}

func (m *readinessMemory) hasKnown(module string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.known[module]
	return ok
}

func (m *readinessMemory) remember(module string, s readiness.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.known == nil {
		m.known = map[string]readiness.State{}
	}
	m.known[module] = s
}

// readinessPersistInput is everything the persist rule looks at. A STRUCT,
// deliberately, so a later rule is one more field and one more clause rather
// than a second code path or a new parameter at every call site.
type readinessPersistInput struct {
	State readiness.State
	// KnownBefore is true when a known verdict stands for the module -- this
	// process evaluated one, the standing row is one -- or when the standing
	// read failed and that cannot be ruled out.
	KnownBefore bool
}

// persistReadiness decides whether a report may be written at all (D1 of the
// 2026-09-14 readiness-convergence record). Every known state may. Unknown may
// only when nothing known stands, so "could not ask" can never replace an
// answer: a fresh pod says why it has no verdict, and a pod that had one keeps
// it. Whether a permitted write is also NEEDED is readinessRewriteNeeded's
// question, asked after this one.
func persistReadiness(in readinessPersistInput) bool {
	if in.State != readiness.Unknown {
		return true
	}
	return !in.KnownBefore
}

// ReadinessUnknownError says a pass could not evaluate one or more modules.
// The rows for every KNOWN module in the same pass were still written; this is
// returned so a caller retries the pass (the recompute subscriber does, with
// backoff) rather than believing the cluster is described. Reasons are the
// closed vocabulary from component/memql/readiness, never resolver error text,
// which stayed in the evaluator's own log line.
type ReadinessUnknownError struct {
	Modules []string
	Reasons map[string]string
}

func (e *ReadinessUnknownError) Error() string {
	return fmt.Sprintf("module readiness: %d module(s) could not be evaluated: %s",
		len(e.Modules), strings.Join(e.pairs(), ", "))
}

// pairs renders module=reason for each unknown module, in module order.
func (e *ReadinessUnknownError) pairs() []string {
	out := make([]string, 0, len(e.Modules))
	for _, m := range e.Modules {
		out = append(out, m+"="+e.Reasons[m])
	}
	return out
}

// IsReadinessUnknown reports whether err is a pass that could not evaluate
// some module, as opposed to a pass whose write failed.
func IsReadinessUnknown(err error) bool {
	var u *ReadinessUnknownError
	return errors.As(err, &u)
}

// readinessExecutor runs one rendered @serverOnly call. Injected so the write
// decision is testable with no engine and no database.
type readinessExecutor func(ctx context.Context, call string) error

// readinessExecutor is the engine's own: the rendered call under the write
// context. Built once per pass so every module's write shares one actor.
func (e *MemQLEngine) readinessExecutor(ctx context.Context) readinessExecutor {
	wctx := readinessWriteContext(ctx)
	return func(_ context.Context, call string) error {
		_, err := e.Execute(wctx, call)
		return err
	}
}

// writeModuleReadiness is the whole write decision with its inputs handed in.
//
// A FAILED WRITE stops at the first module and leaves the PREVIOUS version of
// every remaining row standing, which is the right failure: a stale verdict a
// person can act on beats a half-rewritten set nobody can interpret.
//
// AN UNKNOWN EVALUATION does not stop the pass: every known module is still
// written (when it changed, or its row is past the restatement floor), an
// unknown one only where nothing known stands (persistReadiness), and the pass
// returns *ReadinessUnknownError so the caller retries.
func writeModuleReadiness(ctx context.Context, r readinessResolvers, mods []envregistry.Module, nodeId, nodeType string, mem *readinessMemory, standing readinessStanding, exec readinessExecutor, now time.Time) (int, error) {
	// THE CALLER'S CONTEXT, AND THAT IS DELIBERATE. `evaluateModule` applies
	// the evaluation actor itself, at the one place a context reaches a
	// resolver -- see the comment there for why this is not a line here.
	reports := evaluateModules(ctx, r, mods, nodeId, nodeType, now)
	written := 0
	unknown := &ReadinessUnknownError{Reasons: map[string]string{}}
	for _, rep := range reports {
		if rep.State == readiness.Unknown {
			unknown.Modules = append(unknown.Modules, rep.Module)
			unknown.Reasons[rep.Module] = rep.Reason
		}
		knownBefore := mem.hasKnown(rep.Module) || standing.knownRow(rep.Module)
		if !persistReadiness(readinessPersistInput{State: rep.State, KnownBefore: knownBefore}) {
			continue
		}
		if !readinessRewriteNeeded(standing.rows[rep.Module], rep, now) {
			if rep.State != readiness.Unknown {
				mem.remember(rep.Module, rep.State)
			}
			continue
		}
		call, err := renderRecordModuleReadiness(rep, readinessRowID(rep.Module, nodeId))
		if err != nil {
			return written, fmt.Errorf("module readiness: render %s: %w", rep.Module, err)
		}
		if err := exec(ctx, call); err != nil {
			return written, fmt.Errorf("module readiness: write %s: %w", rep.Module, err)
		}
		written++
		if rep.State != readiness.Unknown {
			mem.remember(rep.Module, rep.State)
		}
	}
	if len(unknown.Modules) > 0 {
		sort.Strings(unknown.Modules)
		return written, unknown
	}
	return written, nil
}

// WriteModuleReadiness evaluates every module and writes this node's rows as
// new versions of their deterministic ids. Returns how many were written.
//
// When any module could not be evaluated the error is *ReadinessUnknownError
// (IsReadinessUnknown); the rows for the other modules were still written.
// Callers that need the cluster described retry -- the recompute subscriber
// does.
func (e *MemQLEngine) WriteModuleReadiness(ctx context.Context) (int, error) {
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		return 0, fmt.Errorf("module readiness: manifest: %w", err)
	}
	nodeId, nodeType := e.readinessIdentity()
	standing := e.standingReadiness(ctx, nodeId)
	return writeModuleReadiness(ctx, e.readinessResolvers(), manifest.Modules, nodeId, nodeType, &e.readinessMemory, standing, e.readinessExecutor(ctx), time.Now().UTC())
}
