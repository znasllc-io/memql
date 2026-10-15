package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestApprovalDiscussionExpiryOnlyRelaxesNativeScopeGuidance(t *testing.T) {
	for _, mode := range []string{"live", "expired-command", "expired-witness", "legacy-scope", "invalid-scope", "changed-artifact", "missing-agent", "approved", "answered", "rejected", "cancelled", "replaced", "running", "failed", "succeeded", "different-wait", "budget", "feedback", "side-effect"} {
		t.Run(mode, func(t *testing.T) {
			hash, err := permission.Hash("workerHost", "exec", map[string]any{"cmd": "git status --short", "cwd": "/fixture"})
			require.NoError(t, err)
			scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Test computer", Tool: "workerHost", Action: "exec", Command: "git status --short", CWD: "/fixture", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
			subject := map[string]any{"agentId": "agent", "computerPermission": scope.Map()}
			approval := map[string]any{"id": "request", "kind": "scopeElevation", "subject": subject, "expiresAt": time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}
			run := map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "request"}}
			switch mode {
			case "live":
				approval["expiresAt"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			case "expired-witness", "legacy-scope":
				delete(subject, "computerPermission")
				subject["scope"] = "observe"
				if mode == "expired-witness" {
					subject["nativeScopeCheckpoint"] = map[string]any{"version": 1, "continuationId": "saved", "toolCallId": "pending", "requestHash": "exact-request"}
				}
			case "invalid-scope":
				subject["computerPermission"] = map[string]any{}
			case "missing-agent":
				delete(subject, "agentId")
			case "approved", "answered", "rejected":
				approval["decision"] = mode
			case "cancelled":
				run["cancelRequested"] = true
			case "replaced":
				run["waitingOn"].(map[string]any)["subject"] = "other-request"
			case "running", "failed", "succeeded":
				run["status"] = mode
			case "different-wait":
				run["waitingOn"].(map[string]any)["kind"] = "child"
			case "budget":
				approval["kind"] = "budget"
				subject["resumeKind"], subject["continuationId"] = "checkpoint", "saved"
			case "feedback":
				approval["kind"] = "feedback"
			case "side-effect":
				approval["kind"] = "sideEffect"
			}
			approval["artifactHash"] = work.ArtifactHash(subject)
			if mode == "changed-artifact" {
				approval["artifactHash"] = "changed"
			}
			require.Equal(t, mode == "live", approvalDiscussionPending(approval, run), "expiry must still block new advisor analysis")
			require.Equal(t, mode == "live" || mode == "expired-command" || mode == "expired-witness", approvalDiscussionGuidancePending(approval, run))
		})
	}
}
