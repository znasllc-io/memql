package goalfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

var recordedGoTest = regexp.MustCompile(`^(GOWORK=off )?go test( [A-Za-z0-9_./=,-]+)*$`)

// ConfirmRecordedChecks complements independent test execution with a durable
// worker receipt. Unsupported shell wrappers remain unverified; a string in a
// model's answer or an echoed command is not a command execution receipt.
func (w *Workspace) ConfirmRecordedChecks(assertions []goaleval.Assertion, observations []map[string]any) error {
	for n := range assertions {
		if assertions[n].CriterionID != "checks" {
			continue
		}
		var receipts []string
		for _, row := range observations {
			data, _ := row["data"].(map[string]any)
			if row["kind"] != "tool_result" || data["tool"] != "workerHost" || data["isError"] != false || data["argsTruncated"] == true {
				continue
			}
			var args map[string]any
			raw, _ := data["args"].(string)
			if json.Unmarshal([]byte(raw), &args) != nil || args["action"] != "exec" {
				continue
			}
			input, _ := args["args"].(map[string]any)
			cmd, _ := input["cmd"].(string)
			cwd, _ := input["cwd"].(string)
			if cwd == w.Checkout && recordedGoTest.MatchString(cmd) {
				id, _ := row["id"].(string)
				if id != "" {
					receipts = append(receipts, "observation:"+id)
				}
			}
		}
		path := filepath.Join(w.Root, "oracle-recorded-checks.json")
		detail := map[string]any{"receipts": receipts, "independentVerdict": assertions[n].Verdict}
		if len(receipts) == 0 {
			detail["detail"] = "No successful direct go test worker receipt for the evaluation checkout; command execution is unverified."
			if assertions[n].Verdict == "pass" {
				assertions[n].Verdict = "unverified"
			}
		} else {
			detail["detail"] = "Recorded test execution and separately rerun final package tests."
		}
		b, err := json.MarshalIndent(detail, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			return err
		}
		assertions[n].Evidence = append(assertions[n].Evidence, path)
	}
	return nil
}
