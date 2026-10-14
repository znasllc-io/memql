package goaleval

import (
	"encoding/json"
	"testing"
)

func TestRecordedChecksRefuseEchoesErrorsAndForeignDirectories(t *testing.T) {
	for _, variant := range []string{"actual", "echo", "error", "foreign", "truncated", "model-claim"} {
		t.Run(variant, func(t *testing.T) {
			w := &Workspace{Root: t.TempDir(), Checkout: "/fixture/checkout"}
			cmd, cwd := "go test -count=1 ./...", w.Checkout
			if variant == "echo" {
				cmd = "echo go test -count=1 ./..."
			}
			if variant == "foreign" {
				cwd = "/unrelated"
			}
			args, _ := json.Marshal(map[string]any{"action": "exec", "args": map[string]any{"cmd": cmd, "cwd": cwd}})
			row := map[string]any{"id": "receipt", "kind": "tool_result", "data": map[string]any{"tool": "workerHost", "isError": variant == "error", "argsTruncated": variant == "truncated", "args": string(args)}}
			if variant == "model-claim" {
				row["kind"] = "note"
			}
			assertions := []Assertion{{CriterionID: "checks", Verdict: "pass"}}
			if err := w.ConfirmRecordedChecks(assertions, []map[string]any{row}); err != nil {
				t.Fatal(err)
			}
			if (assertions[0].Verdict == "pass") != (variant == "actual") {
				t.Fatal(assertions)
			}
		})
	}
}
