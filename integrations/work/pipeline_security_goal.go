package work

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// OpenPipelineSecurityGoal is the internal, owner-bound admission seam for one
// verified finding. The caller binds requestKey to owner/source/scanner/finding,
// and BeforeRun revalidates its source and live scan consent before each write.
// A later scan retains the original evidence instead of changing an in-flight
// proposal. A closed goal stays closed: only its owner may reopen accepted work.
// Partial bootstrap resumes on another replica under the same durable gate.
func (i *Integration) OpenPipelineSecurityGoal(ctx context.Context, requestKey string, g DirectGoal) (ReviewGoalReceipt, error) {
	owner := memql.BareShortId(strings.TrimSpace(g.OwnerUserId))
	if i == nil || owner == "" || memql.BareShortId(callerUserId(ctx)) != owner || requestKey == "" || len(requestKey) > 200 || strings.TrimSpace(g.Statement) == "" || g.BeforeRun == nil {
		return ReviewGoalReceipt{}, fmt.Errorf("work: a security finding requires its authenticated owner, stable identity, statement and current source gate")
	}
	receipt := ReviewGoalIdentity(owner, "pipeline-security:"+requestKey)
	receipt.ApprovalID = ""
	gates, err := i.gatePool()
	if err != nil {
		return receipt, err
	}
	hold, err := memql.AcquireGateHold(ctx, gates, 15*time.Second, "work-security-goal:"+receipt.GoalID)
	if err != nil {
		return receipt, err
	}
	defer hold.Release()
	scoped := memql.ContextWithFreshRead(ownerActor(ctx, owner))
	st := i.store()
	goal, err := st.goalForOwner(scoped, receipt.GoalID)
	if err != nil {
		return receipt, err
	}
	fingerprint := workstate.ArtifactHash(map[string]any{"source": "pipeline-security", "request": requestKey})
	if goal != nil {
		if rowString(goal, "requestFingerprint") != fingerprint || rowString(goal, "requestedVia") != "pipeline" {
			return receipt, fmt.Errorf("work: security finding identity belongs to a different request")
		}
		if rowString(goal, "status") == "closed" {
			return receipt, nil
		}
		g.Statement, g.Input, g.AccountIds = rowString(goal, "statement"), rowMap(goal, "input"), rowStringSlice(goal, "accountIds")
	}
	run, err := st.runForOwner(scoped, receipt.RunID)
	if err != nil {
		return receipt, err
	}
	if run != nil {
		// A durable compiling run belongs to the normal compiler/event recovery
		// path. Weekly observations must not restart completed work or spend a
		// second compilation; the sweep recovers a lost initial dispatch.
		return receipt, nil
	}
	if !i.hasCompileSurface() {
		return receipt, errNoCompileSurface
	}
	spine, err := goalSpine(scoped, owner)
	if err != nil {
		return receipt, err
	}
	write := func(effect func(context.Context) error) error {
		if err := hold.Verify(scoped); err != nil {
			return err
		}
		if err := g.BeforeRun(scoped, receipt.GoalID, receipt.RunID); err != nil {
			return err
		}
		// Source validation can wait on the provider. A gate connection lost
		// during that wait no longer serializes this write, even if its earlier
		// verification passed; recheck immediately before the durable effect.
		if err := hold.Verify(scoped); err != nil {
			return err
		}
		return effect(scoped)
	}
	if goal == nil {
		if err := write(func(c context.Context) error {
			return st.createGoalRow(c, goalSeed{GoalId: receipt.GoalID, Statement: g.Statement, Origin: "system", Input: g.Input,
				AccountIds: g.AccountIds, RequestedVia: "pipeline", RequestFingerprint: fingerprint})
		}); err != nil {
			return receipt, err
		}
	}
	if err := write(func(c context.Context) error {
		return st.createRunRow(c, runSeed{Spine: spine, RunId: receipt.RunID, GoalId: receipt.GoalID, AutomationName: compilingAutomationName,
			Input: g.Input, TriggeredBy: "pipeline-security", Mode: modeLive, Status: runStatusCompiling,
			StartedAt: i.clock().UTC(), OwnerUserId: owner})
	}); err != nil {
		return receipt, err
	}
	// Admission is durable before dispatch. A lost event is recovered by the
	// existing compiling-run sweep under its ordinary owner and budget gates.
	_ = i.dispatchCompile(scoped, CompileRequest{GoalId: receipt.GoalID, RunId: receipt.RunID, OwnerUserId: owner, Statement: g.Statement, Input: g.Input})
	return receipt, nil
}
