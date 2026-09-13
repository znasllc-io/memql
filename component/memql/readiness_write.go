package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
// memql.znas.io, 2026-09-13), and every one of those rows was news to nobody.
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
	if prev.State != next.State || prev.Core != next.Core || !readinessLanesEqual(prev.Lanes, next.Lanes) {
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

// standingReadiness reads this node's current verdict per module. A failed
// read answers an empty map, which makes every module rewrite -- the
// pre-existing behaviour -- rather than skipping on a guess.
func (e *MemQLEngine) standingReadiness(ctx context.Context, nodeId string) map[string]*readiness.NodeReport {
	out := map[string]*readiness.NodeReport{}
	reports, err := e.readModuleReadinessRows(ctx)
	if err != nil {
		e.safeLogger().Warn("module readiness: standing rows unreadable; rewriting every module", "error", err)
		return out
	}
	for i := range reports {
		if reports[i].NodeId == nodeId {
			r := reports[i]
			out[r.Module] = &r
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
func renderRecordModuleReadiness(r readiness.NodeReport, rowId string) (string, error) {
	lanes := r.Lanes
	if lanes == nil {
		lanes = []readiness.LaneReport{}
	}
	raw, err := json.Marshal(lanes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"mutation recordModuleReadiness(rowId: %s, module: %s, nodeId: %s, nodeType: %s, state: %s, core: %t, lanes: %s, reportedAt: %s)",
		langparser.QuoteString(rowId),
		langparser.QuoteString(r.Module),
		langparser.QuoteString(r.NodeId),
		langparser.QuoteString(r.NodeType),
		langparser.QuoteString(string(r.State)),
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

// WriteModuleReadiness evaluates every module and writes this node's rows as
// new versions of their deterministic ids. Returns how many were written.
//
// A failure stops at the first module and leaves the PREVIOUS version of
// every remaining row standing, which is the right failure: a stale verdict a
// person can act on beats a half-rewritten set nobody can interpret.
func (e *MemQLEngine) WriteModuleReadiness(ctx context.Context) (int, error) {
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		return 0, fmt.Errorf("module readiness: manifest: %w", err)
	}
	nodeId, nodeType := e.readinessIdentity()
	// THE CALLER'S CONTEXT, AND THAT IS DELIBERATE. `evaluateModule` applies
	// the evaluation actor itself, at the one place a context reaches a
	// resolver -- see the comment there for why this is not a line here.
	reports := evaluateModules(ctx, e.readinessResolvers(), manifest.Modules, nodeId, nodeType, time.Now().UTC())
	wctx := readinessWriteContext(ctx)
	standing := e.standingReadiness(ctx, nodeId)
	now := time.Now().UTC()
	written := 0
	for _, r := range reports {
		if !readinessRewriteNeeded(standing[r.Module], r, now) {
			continue
		}
		call, err := renderRecordModuleReadiness(r, readinessRowID(r.Module, nodeId))
		if err != nil {
			return written, fmt.Errorf("module readiness: render %s: %w", r.Module, err)
		}
		if _, err := e.Execute(wctx, call); err != nil {
			return written, fmt.Errorf("module readiness: write %s: %w", r.Module, err)
		}
		written++
	}
	return written, nil
}
