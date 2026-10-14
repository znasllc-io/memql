package goalfixture

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

func TestRecordedChecksRequireRealCommandOutcomes(t *testing.T) {
	for _, variant := range []string{"actual", "shape-envelope", "echo", "foreign", "filtered-tests", "no-exit-code", "nonzero", "transport-error", "wrong-concept", "model-claim", "pending-call", "wrong-call", "no-tests", "truncated", "transport-only"} {
		t.Run(variant, func(t *testing.T) {
			w := &Workspace{Root: t.TempDir(), Checkout: "/fixture/checkout"}
			args := map[string]any{"action": "exec", "args": map[string]any{"cmd": "go test -count=1 ./...", "cwd": w.Checkout}}
			input := args["args"].(map[string]any)
			switch variant {
			case "echo":
				input["cmd"] = "echo go test -count=1 ./..."
			case "foreign":
				input["cwd"] = "/unrelated"
			case "filtered-tests":
				input["cmd"] = "go test -run=NoTests ./..."
			}
			output := map[string]any{"exitCode": 0, "stdout": "ok  evaluation.invalid/totals 0.001s\n"}
			payload := map[string]any{"ok": true, "output": output}
			node := map[string]any{"concept": "integration:worker:result", "payload": payload}
			switch variant {
			case "no-exit-code":
				delete(output, "exitCode")
			case "nonzero":
				output["exitCode"] = 1
			case "transport-error":
				payload["ok"] = false
			case "wrong-concept":
				node["concept"] = "model:claim"
			case "no-tests":
				output["stdout"] = "? evaluation.invalid/totals [no test files]"
			}
			var envelope any = map[string]any{"nodes": []any{node}}
			if variant == "shape-envelope" {
				envelope = map[string]any{"data": []any{map[string]any{"result": node}}}
			}
			result, _ := json.Marshal(envelope)
			encodedArgs, _ := json.Marshal(args)
			messages := []recordedMessage{{Role: "assistant", ToolCalls: []recordedCall{{ID: "test", Name: "workerHost", Arguments: string(encodedArgs)}}}, {Role: "tool", Name: "workerHost", ToolCallId: "test", Content: string(result)}}
			switch variant {
			case "model-claim":
				messages[1].Role = "assistant"
			case "pending-call":
				messages[1].Content = "Not executed before the saved checkpoint."
			case "wrong-call":
				messages[1].ToolCallId = "not-this-call"
			}
			row := testCheckpoint(t, messages)
			if variant == "truncated" {
				row["data"].(map[string]any)["messages"] = "not base64"
			}
			if variant == "transport-only" {
				row = map[string]any{"id": "receipt", "kind": "tool_result", "data": map[string]any{"tool": "workerHost", "isError": false, "args": string(encodedArgs)}}
			}
			assertions := []goaleval.Assertion{{CriterionID: "checks", Verdict: "pass"}}
			if err := w.ConfirmRecordedChecks(assertions, []map[string]any{row}); err != nil {
				t.Fatal(err)
			}
			want := variant == "actual" || variant == "shape-envelope"
			if (assertions[0].Verdict == "pass") != want {
				t.Fatal(assertions)
			}
		})
	}
}

func TestRecordedRegressionRequiresFailureThenRecovery(t *testing.T) {
	for _, variant := range []string{"red-green", "green-only", "green-red", "build-failure", "unmatched-result", "different-checkpoints"} {
		t.Run(variant, func(t *testing.T) {
			w := &Workspace{Root: t.TempDir(), Checkout: "/fixture/checkout"}
			args, _ := json.Marshal(map[string]any{"action": "exec", "args": map[string]any{"cmd": "go test -count=1 ./...", "cwd": w.Checkout}})
			pair := func(id string, exit int, stdout string) []recordedMessage {
				content, _ := json.Marshal(map[string]any{"nodes": []any{map[string]any{"concept": "integration:worker:result", "payload": map[string]any{"ok": true, "output": map[string]any{"exitCode": exit, "stdout": stdout}}}}})
				return []recordedMessage{{Role: "assistant", ToolCalls: []recordedCall{{ID: id, Name: "workerHost", Arguments: string(args)}}}, {Role: "tool", Name: "workerHost", ToolCallId: id, Content: string(content)}}
			}
			failure := "--- FAIL: TestSum (0.00s)\nFAIL evaluation.invalid/totals\n"
			if variant == "build-failure" {
				failure += "[build failed]"
			}
			red, green := pair("red", 1, failure), pair("green", 0, "ok evaluation.invalid/totals 0.001s\n")
			messages := append(append([]recordedMessage{}, red...), green...)
			switch variant {
			case "green-only":
				messages = green
			case "green-red":
				messages = append(append([]recordedMessage{}, green...), red...)
			case "unmatched-result":
				messages[1].ToolCallId = "unknown"
			}
			rows := []map[string]any{testCheckpoint(t, messages)}
			if variant == "different-checkpoints" {
				rows = []map[string]any{testCheckpoint(t, red), testCheckpoint(t, green)}
			}
			assertions := []goaleval.Assertion{{CriterionID: "regression", Verdict: "pass"}}
			if err := w.ConfirmRecordedChecks(assertions, rows); err != nil {
				t.Fatal(err)
			}
			if (assertions[0].Verdict == "pass") != (variant == "red-green") {
				t.Fatal(assertions)
			}
		})
	}
}

func testCheckpoint(t *testing.T, messages []recordedMessage) map[string]any {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": "checkpoint", "kind": "note", "data": map[string]any{"continuation": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)}}
}
