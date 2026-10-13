package work

import (
	workstate "github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"testing"
	"time"
)

func computerPermissionFixture() (map[string]any, permission.Request) {
	req := permission.Request{OwnerID: "owner", AgentID: "agent", RunID: "run", Tool: "workerHost", Action: "fs_read", Args: map[string]any{"path": "/docs/report"}}
	hash, _ := permission.Hash(req.Tool, req.Action, req.Args)
	s := permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Laptop", Tool: req.Tool, Action: req.Action, Directory: "/docs", ArgsHash: hash, AllowedModes: []string{"once", "always"}}
	subject := map[string]any{"agentId": "agent", "computerPermission": s.Map()}
	row := map[string]any{"id": "permission", "ownerUserId": "owner", "runId": "run", "kind": "scopeElevation", "decision": "approved", "decidedBy": "owner", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "answer": map[string]any{"permissionMode": "always"}, "expiresAt": time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}
	return row, req
}
func TestComputerPermissionAlwaysSurvivesPendingExpiry(t *testing.T) {
	row, req := computerPermissionFixture()
	req.RunID = "next-run"
	if permissionFromRow(row, req, time.Now()) == nil {
		t.Fatal("Always inherited pending expiry or original run")
	}
	for _, key := range []string{"ownerUserId", "decidedBy", "decision", "artifactHash", "permissionRevokedAt"} {
		copy := map[string]any{}
		for k, v := range row {
			copy[k] = v
		}
		copy[key] = "changed"
		if permissionFromRow(copy, req, time.Now()) != nil {
			t.Fatal(key)
		}
	}
	req.AgentID = "other"
	if permissionFromRow(row, req, time.Now()) != nil {
		t.Fatal("agent scope escaped")
	}
}
func TestComputerPermissionOnceRequiresUnusedExactLiveReceipt(t *testing.T) {
	row, req := computerPermissionFixture()
	row["answer"] = map[string]any{"permissionMode": "once"}
	row["expiresAt"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	if permissionFromRow(row, req, time.Now()) == nil {
		t.Fatal("unused once refused")
	}
	row["permissionUsedAt"] = time.Now().Format(time.RFC3339Nano)
	if permissionFromRow(row, req, time.Now()) != nil {
		t.Fatal("spent once admitted")
	}
	delete(row, "permissionUsedAt")
	req.RunID = "other"
	if permissionFromRow(row, req, time.Now()) != nil {
		t.Fatal("once escaped run")
	}
}
func TestComputerPermissionDecisionRejectsInventedModes(t *testing.T) {
	row, _ := computerPermissionFixture()
	if err := validateComputerPermissionDecision(row, "approved", map[string]any{"permissionMode": "always"}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "all", "forever"} {
		if validateComputerPermissionDecision(row, "approved", map[string]any{"permissionMode": mode}) == nil {
			t.Fatal(mode)
		}
	}
	delete(rowMap(row, "subject"), "computerPermission")
	if validateComputerPermissionDecision(row, "approved", map[string]any{"permissionMode": "always"}) == nil {
		t.Fatal("legacy scope promoted to always")
	}
}
