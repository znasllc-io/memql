package planner

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

func TestInvocationPolicyChecksNestedCallsAndAllowsToolBatches(t *testing.T) {
	compile := func(source string) *automations.Automation {
		t.Helper()
		a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation research {\n"+source+"\n}", "test.memql")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	tool := `first := builtin runAgentTurn(templateId: "evidence", data: {subject: "one"})`
	text := `plain := builtin ai(templateId: "summary", data: {})`
	original := compile(tool + "\n" + text)
	policy := []map[string]string{{"from": "runAgentTurn", "to": "ai", "argument": "templateId"}}
	for _, tc := range []struct {
		name, body string
		refuse     bool
	}{
		{"tool batches", tool + "\nsecond := builtin runAgentTurn(templateId: \"evidence\", data: {subject: \"two\"})\n" + text, false},
		{"different text task", tool + "\n" + text, false},
		{"loop substitution", `for item in [1, 2] { result := builtin ai(templateId: "evidence", data: {}) }`, true},
		{"parallel substitution", `result := parallel { branch text { answer := builtin ai(templateId: "evidence", data: {}) } }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := replanPreservesInvocation(original, compile(tc.body), policy)
			if (err != nil) != tc.refuse {
				t.Fatalf("capability preservation = %v, refuse %v", err, tc.refuse)
			}
		})
	}
	// Do not reject a text invocation that already existed beside the tool call.
	mixed := compile(tool + "\ntext := builtin ai(templateId: \"evidence\", data: {})")
	if err := replanPreservesInvocation(mixed, mixed, policy); err != nil {
		t.Fatal(err)
	}
}
