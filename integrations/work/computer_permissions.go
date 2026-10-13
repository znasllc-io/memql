package work

import (
	"context"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func validateComputerPermissionDecision(row map[string]any, decision string, answer map[string]any) error {
	subject := rowMap(row, "subject")
	typed, present := subject["computerPermission"]
	if !present {
		if _, supplied := answer["permissionMode"]; supplied {
			return fmt.Errorf("work: this approval offers no saved computer permission")
		}
		return nil
	}
	if rowString(row, "kind") != "scopeElevation" {
		return fmt.Errorf("work: computer permission must be a scope decision")
	}
	if decision != "approved" {
		return nil
	}
	if _, err := permission.Parse(typed); err != nil {
		return err
	}
	if rowString(subject, "agentId") == "" || workstate.ArtifactHash(subject) != rowString(row, "artifactHash") {
		return fmt.Errorf("work: computer permission changed; request it again")
	}
	if mode := rowString(answer, "permissionMode"); mode != "once" && mode != "always" {
		return fmt.Errorf("work: choose once or always for this computer permission")
	}
	return nil
}

func samePermissionID(a, b string) bool {
	return a != "" && b != "" && memql.BareShortId(a) == memql.BareShortId(b)
}

func permissionFromRow(row map[string]any, req permission.Request, now time.Time) *permission.Grant {
	if rowString(row, "kind") != "scopeElevation" || rowString(row, "decision") != "approved" || !samePermissionID(rowString(row, "ownerUserId"), req.OwnerID) || !samePermissionID(rowString(row, "decidedBy"), req.OwnerID) || rowString(row, "permissionRevokedAt") != "" {
		return nil
	}
	subject := rowMap(row, "subject")
	if !samePermissionID(rowString(subject, "agentId"), req.AgentID) || workstate.ArtifactHash(subject) != rowString(row, "artifactHash") {
		return nil
	}
	scope, err := permission.Parse(subject["computerPermission"])
	if err != nil {
		return nil
	}
	mode := rowString(rowMap(row, "answer"), "permissionMode")
	if mode == "once" {
		until, ok := rowTime(row, "expiresAt")
		if !ok || !now.Before(until) || !samePermissionID(rowString(row, "runId"), req.RunID) || rowString(row, "permissionUsedAt") != "" {
			return nil
		}
	}
	if !scope.Matches(req, mode) {
		return nil
	}
	return &permission.Grant{ID: rowString(row, "id"), Scope: scope}
}

func (i *Integration) permissionRun(ctx context.Context, req permission.Request, claiming bool) error {
	if rc, present := common.RunFromContext(ctx); present && (!samePermissionID(rc.RunId, req.RunID) || !samePermissionID(rc.OwnerUserId, req.OwnerID)) {
		return fmt.Errorf("computer permission must belong to the originating work run")
	}
	if !samePermissionID(callerUserId(ctx), req.OwnerID) || req.AgentID == "" || req.RunID == "" {
		return fmt.Errorf("computer permission requires the current owner, agent and run")
	}
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workRunForOwner", map[string]any{"runId": req.RunID}))
	if err != nil {
		return err
	}
	if len(rows) != 1 || !samePermissionID(rowString(rows[0], "ownerUserId"), req.OwnerID) || rows[0]["cancelRequested"] == true {
		return fmt.Errorf("computer work is no longer active")
	}
	status := rowString(rows[0], "status")
	if status != "running" && (claiming || status != "waiting") {
		return fmt.Errorf("computer work is no longer active")
	}
	return nil
}

// Find reads on this replica without consuming. The actual stream holder claims
// the receipt, so forwarding can never spend a one-time decision twice.
func (i *Integration) Find(ctx context.Context, req permission.Request) (*permission.Grant, error) {
	if err := i.permissionRun(ctx, req, false); err != nil {
		return nil, err
	}
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workComputerPermissionsForAgent", map[string]any{"agentId": req.AgentID}))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if grant := permissionFromRow(row, req, i.clock()); grant != nil {
			return grant, nil
		}
	}
	return nil, nil
}

func (i *Integration) Claim(ctx context.Context, id string, req permission.Request, registrationID, machineID string) (*permission.Scope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool, err := i.gatePool()
	if err != nil {
		return nil, err
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), pool, 0, "work-approval-decision:"+memql.BareShortId(id))
	if err != nil {
		return nil, err
	}
	defer hold.Release()
	if err := i.permissionRun(ctx, req, true); err != nil {
		return nil, err
	}
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("computer permission is unavailable")
	}
	row := rows[0]
	grant := permissionFromRow(row, req, i.clock())
	if grant == nil || !samePermissionID(grant.Scope.RegistrationID, registrationID) || grant.Scope.MachineID != machineID {
		return nil, fmt.Errorf("computer permission no longer covers this action on this machine")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := hold.Verify(write); err != nil {
		return nil, err
	}
	if rowString(rowMap(row, "answer"), "permissionMode") == "once" {
		now := i.clock().UTC()
		if err := i.store().writeInternal(ownerActor(write, req.OwnerID), "mutation "+call("useWorkComputerPermission", map[string]any{"approvalId": id, "permissionUsedAt": rfc(now), "versionTime": rfc(workRowVersionAfter(row["createdAt"], now))})); err != nil {
			return nil, err
		}
	}
	return &grant.Scope, nil
}

func (i *Integration) handleRevokeComputerPermission(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if run, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && run.RunId != "") {
		return nil, fmt.Errorf("work: revocation requires a person's interactive decision")
	}
	id := argString(args, "permissionId")
	if id == "" {
		return nil, fmt.Errorf("work: a permissionId is required")
	}
	pool, err := i.gatePool()
	if err != nil {
		return nil, err
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), pool, 0, "work-approval-decision:"+memql.BareShortId(id))
	if err != nil {
		return nil, err
	}
	defer hold.Release()
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || !samePermissionID(rowString(rows[0], "ownerUserId"), ac.UserId) || rowString(rows[0], "decision") != "approved" || rowString(rowMap(rows[0], "answer"), "permissionMode") != "always" {
		return nil, fmt.Errorf("work: no saved permission belongs to this caller")
	}
	row := rows[0]
	if _, err := permission.Parse(rowMap(row, "subject")["computerPermission"]); err != nil {
		return nil, err
	}
	if rowString(row, "permissionRevokedAt") == "" {
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := hold.Verify(write); err != nil {
			return nil, err
		}
		now := i.clock().UTC()
		if err := i.store().writeInternal(ownerActor(write, ac.UserId), "mutation "+call("revokeWorkComputerPermission", map[string]any{"approvalId": id, "permissionRevokedAt": rfc(now), "versionTime": rfc(workRowVersionAfter(row["createdAt"], now))})); err != nil {
			return nil, err
		}
	}
	return i.resultNode(map[string]any{"permissionId": id, "revoked": true}), nil
}

// Permission decisions use the same receipt lock as claim and revocation, held
// independently of caller cancellation until the bounded durable write returns.
func (i *Integration) computerPermissionDecisionGate(ctx context.Context, id string) (func(), func(context.Context) error, error) {
	if i.computerDecisionGate != nil {
		return i.computerDecisionGate(ctx, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	pool, err := i.gatePool()
	if err != nil {
		return nil, nil, err
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), pool, 0, "work-approval-decision:"+memql.BareShortId(id))
	if err != nil {
		return nil, nil, err
	}
	return hold.Release, hold.Verify, nil
}

type approvalDecisionVerifierKey struct{}

func verifyApprovalDecisionWrite(ctx context.Context) error {
	if verify, ok := ctx.Value(approvalDecisionVerifierKey{}).(func(context.Context) error); ok {
		return verify(ctx)
	}
	return nil
}
