package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestWorkPermissionGuidanceRequiresExactTypedCorrection(t *testing.T) {
	for _, mode := range []string{"command", "directory", "unicode-bound", "legacy", "bad-scope", "changed-hash", "missing-agent", "wrong-kind", "mixed-answer", "empty", "untrimmed", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			hash, _ := permission.Hash("workerHost", "exec", map[string]any{"cmd": "printf test", "cwd": "/fixture"})
			scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Test computer", Tool: "workerHost", Action: "exec", Command: "printf test", CWD: "/fixture", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
			if mode == "directory" {
				scope.Kind, scope.Action, scope.Directory = "directory_read", "fs_read", "/fixture"
				scope.Command, scope.CWD, scope.TimeoutSeconds = "", "", 0
			}
			subject := map[string]any{"agentId": "agent", "computerPermission": scope.Map()}
			answer := map[string]any{"feedback": "Correct the request without granting access."}
			kind := work.ApprovalKindScopeElevation
			switch mode {
			case "unicode-bound":
				answer["feedback"] = strings.Repeat("文", 4000)
			case "legacy":
				delete(subject, "computerPermission")
				subject["scope"] = "full"
			case "bad-scope":
				subject["computerPermission"].(map[string]any)["timeoutSeconds"] = 0
			case "missing-agent":
				delete(subject, "agentId")
			case "wrong-kind":
				kind = work.ApprovalKindSideEffect
			case "mixed-answer":
				answer["permissionMode"] = "always"
			case "empty":
				answer["feedback"] = ""
			case "untrimmed":
				answer["feedback"] = " Correct the request. "
			case "oversize":
				answer["feedback"] = strings.Repeat("文", 4001)
			}
			artifact := work.ArtifactHash(subject)
			if mode == "changed-hash" {
				artifact = "other"
			}
			want := mode == "command" || mode == "directory" || mode == "unicode-bound"
			if got := WorkPermissionGuidance(kind, subject, answer, artifact); got != want {
				t.Fatalf("got %t, want %t", got, want)
			}
		})
	}
}
