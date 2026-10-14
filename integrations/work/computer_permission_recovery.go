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

func computerDecisionState(row, answer map[string]any, now time.Time) (string, string) {
	if rowString(row, "permissionRevokedAt") != "" {
		return "revoked", "This permission was revoked. Request access again in Ask."
	}
	if rowString(row, "permissionUsedAt") != "" {
		return "used", "This one-time permission has already been used."
	}
	subject := rowMap(row, "subject")
	if workstate.ArtifactHash(subject) != rowString(row, "artifactHash") {
		if repairableComputerFingerprint(row) {
			return "review", "This request needs its saved review refreshed. Review it again to continue."
		}
		return "changed", "This permission request changed. Discuss it in Ask to request the access now needed."
	}
	if reviewed := rowString(answer, "permissionHash"); (subject["requiresReviewHash"] == true || reviewed != "") && reviewed != rowString(row, "artifactHash") {
		return "changed", "This permission request changed. Review its current details before approving."
	}
	if until, ok := rowTime(row, "expiresAt"); (rowString(row, "decision") != "approved" || rowString(rowMap(row, "answer"), "permissionMode") != "always") && (!ok || !now.Before(until)) {
		return "expired", "This access request expired. Review it again to continue."
	}
	return "", ""
}

// This is a repair proof, never an alternate authorization check. Only an
// explicit owner refresh can rewrite the fingerprint. Reconstruct the exact
// producer representation that caused the bug; arbitrary mismatches fail closed.
func repairableComputerFingerprint(row map[string]any) bool {
	if rowString(row, "kind") != "scopeElevation" || rowString(row, "decision") != "" {
		return false
	}
	subject := rowMap(row, "subject")
	if len(subject) != 6 || rowString(subject, "agentId") == "" {
		return false
	}
	for _, k := range []string{"agentId", "intent", "summary", "target"} {
		if _, ok := subject[k].(string); !ok {
			return false
		}
	}
	scope, err := permission.Parse(subject["computerPermission"])
	if err != nil {
		return false
	}
	normalized, err := workstate.NormalizeApprovalSubject(subject)
	if err != nil {
		return false
	}
	raw, ok := normalized["requireLabels"].(map[string]any)
	if !ok || raw["machineId"] != scope.MachineID {
		return false
	}
	labels := map[string]string{}
	for k, v := range raw {
		text, ok := v.(string)
		if !ok {
			return false
		}
		labels[k] = text
	}
	normalized["requireLabels"] = labels
	return workstate.ArtifactHash(normalized) == rowString(row, "artifactHash")
}

func (i *Integration) handleRefreshComputerPermission(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if run, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && run.RunId != "") {
		return nil, fmt.Errorf("review requires a person's interactive request")
	}
	id := argString(args, "approvalId")
	if id == "" {
		return nil, fmt.Errorf("choose the computer permission to review")
	}
	release, verify, err := i.computerPermissionDecisionGate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("this request is unavailable")
	}
	row := rows[0]
	recorded := rowString(row, "decision") == "approved"
	blocked, _ := computerDecisionState(row, rowMap(row, "answer"), i.clock())
	if rowString(row, "kind") != "scopeElevation" || rowString(row, "permissionUsedAt") != "" ||
		(rowString(row, "decision") != "" && (!recorded || (blocked != "expired" && blocked != "revoked"))) {
		return nil, fmt.Errorf("only a pending or expired unused computer request can be reviewed again")
	}
	if err = i.validateGuidanceRun(ctx, row); err != nil {
		return nil, err
	}
	subject := rowMap(row, "subject")
	scope, err := permission.Parse(subject["computerPermission"])
	if err != nil {
		return nil, err
	}
	hash := workstate.ArtifactHash(subject)
	if hash != rowString(row, "artifactHash") && !repairableComputerFingerprint(row) {
		return i.resultNode(map[string]any{"approvalId": id, "permissionState": "changed", "message": "This permission changed. Discuss it in Ask to request access again."}), nil
	}
	// A new enrollment is a different authority even on a similarly named host.
	workers, err := i.store().query(ctx, "query myWorkersWithStatus()")
	if err != nil {
		return nil, err
	}
	found := false
	var replacements []map[string]any
	for _, w := range workers {
		labels := rowMap(w, "labels")
		if rowString(labels, "machineId") != scope.MachineID || rowString(w, "revokedAt") != "" {
			continue
		}
		if samePermissionID(rowString(w, "id"), scope.RegistrationID) {
			found = true
		}
		replacements = append(replacements, w)
	}
	if !found && len(replacements) == 1 {
		// The owner explicitly asked to review again. Show the NEW enrollment
		// before deciding; never transfer an existing grant to it.
		scope.RegistrationID = rowString(replacements[0], "id")
		label := rowString(replacements[0], "displayName")
		if label == "" {
			label = rowString(replacements[0], "name")
		}
		scope.MachineLabel = label
		subject, err = workstate.NormalizeApprovalSubject(subject)
		if err != nil {
			return nil, err
		}
		subject["computerPermission"] = scope.Map()
		subject["target"] = label
		subject["requiresReviewHash"] = true
		found = true
	}
	// Refresh is a new review, even when only its deadline changed. A delayed
	// click from the older card must not decide the new review.
	if found {
		subject, err = workstate.NormalizeApprovalSubject(subject)
		if err != nil {
			return nil, err
		}
		subject["requiresReviewHash"] = true
		subject["reviewedAt"] = rfc(workRowVersionAfter(row["createdAt"], i.clock().UTC()))
		hash = workstate.ArtifactHash(subject)
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err = verify(write); err != nil {
		return nil, err
	}
	now := i.clock().UTC()
	requested, expires := rfc(now), rfc(now.Add(time.Hour))
	if !found {
		requested, expires = rowString(row, "requestedAt"), rowString(row, "expiresAt")
	}
	if !found && recorded {
		return i.resultNode(map[string]any{"approvalId": id, "permissionState": "machine_changed", "message": "The selected machine is no longer enrolled. Select it again in Ask."}), nil
	}
	if err = i.store().writeInternal(ownerActor(write, ac.UserId), "mutation "+call("refreshWorkComputerPermission", map[string]any{"approvalId": id, "subject": subject, "artifactHash": hash, "requestedAt": requested, "expiresAt": expires, "versionTime": rfc(workRowVersionAfter(row["createdAt"], now))})); err != nil {
		return nil, err
	}
	if !found {
		return i.resultNode(map[string]any{"approvalId": id, "permissionState": "machine_changed", "message": "This machine's enrollment changed or was removed. Discuss it in Ask to select the current machine and request access again."}), nil
	}
	return i.resultNode(map[string]any{"approvalId": id, "permissionState": "review_ready", "message": "Review the machine and access below, then choose whether to allow it."}), nil
}

func (i *Integration) recoverComputerDecision(ctx context.Context, id, decision string, answer map[string]any) (map[string]any, error) {
	if decision != "approved" || (rowString(answer, "permissionMode") != "once" && rowString(answer, "permissionMode") != "always") {
		return nil, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	row := rows[0]
	if rowString(row, "kind") != "scopeElevation" || rowString(row, "decision") != "approved" || !samePermissionID(rowString(row, "decidedBy"), callerUserId(ctx)) {
		return nil, nil
	}
	if rowString(rowMap(row, "answer"), "permissionMode") != rowString(answer, "permissionMode") {
		return nil, fmt.Errorf("this request already has a different recorded permission choice")
	}
	if err = validateComputerPermissionDecision(row, decision, answer); err != nil {
		return nil, err
	}
	if hash := rowString(answer, "permissionHash"); hash != "" && hash != rowString(row, "artifactHash") {
		return nil, fmt.Errorf("the reviewed permission changed")
	}
	result := map[string]any{"approvalId": id, "runId": rowString(row, "runId"), "decision": "approved", "runResumed": false, "recovered": true}
	run, err := i.store().runForOwner(ctx, rowString(row, "runId"))
	if err != nil {
		return nil, err
	}
	if run != nil && samePermissionID(rowString(run, "humanResumeId"), id) {
		result["runResumed"] = true
		return result, nil
	}
	if state, message := computerDecisionState(row, answer, i.clock()); state != "" {
		result["permissionState"] = state
		result["message"] = message
		return result, nil
	}
	if err = i.validateGuidanceRun(ctx, row); err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	resumed, err := i.resumeParkedRun(ownerActor(ctx, callerUserId(ctx)), rowString(row, "runId"), id, "approved", nil, i.clock().UTC())
	result["runResumed"] = resumed
	if err != nil {
		result["resumeError"] = err.Error()
	}
	return result, nil
}
