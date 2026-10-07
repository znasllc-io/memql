package pipelines

import (
	"encoding/json"
	"strings"
	"testing"
)

func releaseWorkFixture() (map[string]any, map[string]any) {
	fp := "work-definition-v2:" + strings.Repeat("a", 64)
	r := map[string]any{"id": "run", "ownerUserId": "owner", "status": "succeeded", "mode": "live", "templateFingerprint": fp, "triggeredBy": WorkTriggerPrefix + "full",
		"input": map[string]any{"repository": "acme/engine", "sha": strings.Repeat("b", 40), "mode": "full", "event": "push", "pipelineId": "pipeline", "pipelineRunId": "pipeline-run", "attempt": 1}}
	s := map[string]any{"id": "receipt", "createdAt": "2026-10-06T01:00:00Z", "finishedAt": "2026-10-06T01:00:00Z", "ownerUserId": "owner", "runId": "run", "key": "test.full", "status": "done", "stepType": "exec", "kind": "deterministic", "attempt": 1,
		"call": map[string]any{"construct": "pipeline", "definitionFingerprint": fp}, "result": map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{strings.Repeat("c", 64)}}}}
	return r, s
}

func TestReleaseEvidenceBindsActualReceiptAndSource(t *testing.T) {
	r, s := releaseWorkFixture()
	first, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s)
	if err != nil {
		t.Fatal(err)
	}
	r["heartbeatAt"] = "later"
	same, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s)
	if err != nil || first.ReceiptDigest != same.ReceiptDigest {
		t.Fatal("heartbeat changed completed receipt", err)
	}
	for name, change := range map[string]func(map[string]any, map[string]any){
		"source":  func(r, s map[string]any) { r["input"].(map[string]any)["sha"] = strings.Repeat("d", 40) },
		"attempt": func(r, s map[string]any) { s["attempt"] = 2 },
		"version": func(r, s map[string]any) { s["createdAt"] = "2026-10-06T02:00:00Z" },
		"result":  func(r, s map[string]any) { s["result"].(map[string]any)["additionalVerdict"] = "changed" },
		"call":    func(r, s map[string]any) { s["call"].(map[string]any)["name"] = "changed" },
	} {
		t.Run(name, func(t *testing.T) {
			r, s := releaseWorkFixture()
			change(r, s)
			got, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s)
			if err != nil || got.ReceiptDigest == first.ReceiptDigest {
				t.Fatal("changed receipt reused identity", err)
			}
		})
	}
}

func TestReleaseEvidenceRefusesFalseSuccess(t *testing.T) {
	for name, change := range map[string]func(map[string]any, map[string]any){
		"owner":             func(r, s map[string]any) { r["ownerUserId"] = "stranger" },
		"step owner":        func(r, s map[string]any) { s["ownerUserId"] = "stranger" },
		"run running":       func(r, s map[string]any) { r["status"] = "running" },
		"run failed":        func(r, s map[string]any) { r["status"] = "failed" },
		"cancel requested":  func(r, s map[string]any) { r["cancelRequested"] = true },
		"caller payload":    func(r, s map[string]any) { r["callerSuppliedPayload"] = true },
		"replay":            func(r, s map[string]any) { r["mode"] = "replay" },
		"different run":     func(r, s map[string]any) { s["runId"] = "other" },
		"unbound execution": func(r, s map[string]any) { s["call"].(map[string]any)["definitionFingerprint"] = "" },
		"skip":              func(r, s map[string]any) { s["status"] = "skipped" },
		"failed result":     func(r, s map[string]any) { s["result"].(map[string]any)["status"] = "failed" },
		"nonzero exit":      func(r, s map[string]any) { s["result"].(map[string]any)["metadata"].(map[string]any)["exitCode"] = 1 },
		"missing exit": func(r, s map[string]any) {
			delete(s["result"].(map[string]any)["metadata"].(map[string]any), "exitCode")
		},
		"fractional attempt": func(r, s map[string]any) { s["attempt"] = 1.1 },
		"overflow attempt":   func(r, s map[string]any) { s["attempt"] = json.Number("99999999999999999999") },
		"bad artifact": func(r, s map[string]any) {
			s["result"].(map[string]any)["metadata"].(map[string]any)["artifactIntentIds"] = []string{"bad"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, s := releaseWorkFixture()
			change(r, s)
			if _, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
}
