package goalfixture

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

var recordedGoTest = regexp.MustCompile(`^(GOWORK=off )?go test( -count=1| -v| -timeout=[1-9][0-9]*s)* (\.|\./\.\.\.)$`)
var passedGoTest = regexp.MustCompile(`(?m)^ok\s+evaluation\.invalid/totals\s`)

type recordedMessage struct {
	Role, Content, Name, ToolCallId string
	ToolCalls                       []recordedCall
}
type recordedCall struct{ ID, Name, Arguments string }
type recordedTest struct{ evidence, verdict string }

// ConfirmRecordedChecks reads the exact tool exchanges saved by the native
// continuation barrier. A tool_result observation's isError=false only proves
// transport success: workerHost can return a nonzero command exit inside a
// successful protocol response. Neither a model claim nor an independent
// rerun proves that the model saw the failing test and then recovered.
func (w *Workspace) ConfirmRecordedChecks(assertions []goaleval.Assertion, observations []map[string]any) error {
	proof := map[string][]string{}
	for _, row := range observations {
		var red string
		for _, test := range w.recordedTests(row) {
			if test.verdict == "fail" {
				red = test.evidence
				proof["red"] = append(proof["red"], red)
				proof["reproduction"] = append(proof["reproduction"], red)
			} else if test.verdict == "pass" {
				proof["checks"] = append(proof["checks"], test.evidence)
				if red != "" {
					proof["regression"] = append(proof["regression"], red, test.evidence)
				}
			}
		}
	}
	for n := range assertions {
		criterion := assertions[n].CriterionID
		switch criterion {
		case "checks", "red", "reproduction", "regression":
		default:
			continue
		}
		path := filepath.Join(w.Root, "oracle-recorded-"+criterion+".json")
		detail := map[string]any{"receipts": proof[criterion], "independentVerdict": assertions[n].Verdict}
		if len(proof[criterion]) == 0 {
			detail["detail"] = "No matching native checkpoint proves the required test execution and outcome; command evidence is unverified."
			if assertions[n].Verdict == "pass" {
				assertions[n].Verdict = "unverified"
			}
		} else {
			detail["detail"] = "Native checkpoint binds the command, checkout, exit status and output; regression requires a behavioral failure followed by a passing test in that recorded history."
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

func (w *Workspace) recordedTests(row map[string]any) []recordedTest {
	data, _ := row["data"].(map[string]any)
	id, _ := row["id"].(string)
	if row["kind"] != "note" || id == "" || data["continuation"] != true || data["messagesEncoding"] != "json-base64-v1" {
		return nil
	}
	encoded, _ := data["messages"].(string)
	if len(encoded) > 32*1024*1024 {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	var messages []recordedMessage
	if err != nil || json.Unmarshal(raw, &messages) != nil {
		return nil
	}
	pending := map[string]recordedCall{}
	seen := map[string]bool{}
	var tests []recordedTest
	for _, message := range messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID == "" || seen[call.ID] {
					return nil
				}
				seen[call.ID] = true
				pending[call.ID] = call
			}
			continue
		}
		if message.Role != "tool" {
			continue
		}
		call, ok := pending[message.ToolCallId]
		delete(pending, message.ToolCallId)
		if !ok || call.Name != "workerHost" || message.Name != call.Name {
			continue
		}
		var args struct {
			Action string
			Args   struct{ Cmd, Cwd string }
		}
		if json.Unmarshal([]byte(call.Arguments), &args) != nil || args.Action != "exec" || args.Args.Cwd != w.Checkout || !recordedGoTest.MatchString(args.Args.Cmd) {
			continue
		}
		if verdict := recordedTestVerdict(message.Content); verdict != "" {
			tests = append(tests, recordedTest{evidence: "observation:" + id + "/toolCall:" + call.ID, verdict: verdict})
		}
	}
	return tests
}

func recordedTestVerdict(content string) string {
	type node struct {
		Concept string
		Payload struct {
			OK     *bool
			Output struct {
				ExitCode       *int
				Stdout, Stderr string
				Signal         string
			}
		}
	}
	var envelope struct {
		Nodes []node
		Data  []map[string]node
	}
	if json.Unmarshal([]byte(content), &envelope) != nil {
		return ""
	}
	nodes := envelope.Nodes
	for _, row := range envelope.Data {
		for _, n := range row {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) != 1 || nodes[0].Concept != "integration:worker:result" {
		return ""
	}
	payload := nodes[0].Payload
	output := payload.Output
	if payload.OK == nil || !*payload.OK || output.ExitCode == nil || output.Signal != "" {
		return ""
	}
	text := output.Stdout + "\n" + output.Stderr
	if *output.ExitCode == 0 && passedGoTest.MatchString(text) {
		return "pass"
	}
	if *output.ExitCode > 0 && strings.Contains(text, "--- FAIL: Test") && !strings.Contains(text, "[build failed]") {
		return "fail"
	}
	return ""
}
