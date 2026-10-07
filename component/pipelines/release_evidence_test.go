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
		"call": map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}, "result": map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{strings.Repeat("c", 64)}}}}
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

func TestReleaseStepEvidenceKeepsDeliveryAndSelectionDistinct(t *testing.T) {
	for _, kind := range []string{"notification", "planned skip"} {
		t.Run(kind, func(t *testing.T) {
			r, s := releaseWorkFixture()
			call := s["call"].(map[string]any)
			if kind == "notification" {
				call["pipelineKind"] = "notify"
				s["result"] = map[string]any{"status": "succeeded", "notificationStatus": "delivered", "channel": "owners", "kind": "email", "requestIds": []string{"request-1"}}
			} else {
				call["skip"] = map[string]any{"code": CodeNotAffected, "reason": "No db-gated packages."}
				s["status"], s["errorCode"] = "skipped", CodeNotAffected
				s["result"] = map[string]any{"code": CodeNotAffected, "reason": "No db-gated packages."}
			}
			got, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s)
			if err != nil {
				t.Fatal(err)
			}
			if (kind == "notification" && (got.StepKind != "notify" || got.Status != "done")) || (kind == "planned skip" && (got.Status != "skipped" || got.SkipCode != CodeNotAffected)) {
				t.Fatal("typed verdict lost", got)
			}
			if _, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s); err == nil {
				t.Fatal("non-command receipt became artifact production proof")
			}
		})
	}
}

func TestReleaseStepEvidenceRefusesInventedDeliveryOrSkip(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"missing declared kind": func(s map[string]any) { delete(s["call"].(map[string]any), "pipelineKind") },
		"unplanned skip": func(s map[string]any) {
			s["status"] = "skipped"
			s["result"] = map[string]any{"code": CodeNotAffected, "reason": "not planned"}
		},
		"blocked step": func(s map[string]any) { s["status"] = "skipped"; s["errorCode"] = "pipeline_stage_blocked" },
		"skip code drift": func(s map[string]any) {
			s["call"].(map[string]any)["skip"] = map[string]any{"code": CodeNotAffected, "reason": "Nothing selected."}
			s["status"], s["errorCode"] = "skipped", CodePassedEarlier
			s["result"] = map[string]any{"code": CodePassedEarlier, "reason": "Nothing selected."}
		},
		"notification has no receipt": func(s map[string]any) {
			s["call"].(map[string]any)["pipelineKind"] = "notify"
			s["result"] = map[string]any{"status": "succeeded"}
		},
		"notification pending": func(s map[string]any) {
			s["call"].(map[string]any)["pipelineKind"] = "notify"
			s["result"] = map[string]any{"status": "succeeded", "notificationStatus": "pending", "channel": "owners", "kind": "email", "requestIds": []string{"one"}}
		},
		"notification duplicate receipts": func(s map[string]any) {
			s["call"].(map[string]any)["pipelineKind"] = "notify"
			s["result"] = map[string]any{"status": "succeeded", "notificationStatus": "delivered", "channel": "owners", "kind": "email", "requestIds": []string{"one", "one"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, s := releaseWorkFixture()
			change(s)
			if _, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s); err == nil {
				t.Fatal("false typed receipt accepted")
			}
		})
	}
}
