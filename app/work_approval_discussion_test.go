package app

import (
	"github.com/znasllc-io/memql/component/automations"
	"testing"
)

func TestApprovalDiscussionTemplateRefusesExpandedEffects(t *testing.T) {
	loader := automations.NewLoader(automations.LoaderOptions{})
	for _, tc := range []struct {
		name, body string
		allowed    bool
	}{
		{"advice", `reply := builtin askRunApprovalMessage()
 return reply`, true},
		{"secret return", `reply := builtin askRunApprovalMessage()
 return secret("PRIVATE_KEY")`, false},
		{"expanded return", `reply := builtin askRunApprovalMessage()
 return { reply: reply, private: secret("PRIVATE_KEY") }`, false},
		{"secret argument", `reply := builtin askRunApprovalMessage(templateId: secret("PRIVATE_KEY"))
 return reply`, false},
		{"prompt override", `reply := builtin askRunApprovalMessage(templateId: "askApprovalSecondOpinion")
 return reply`, false},
		{"mutation", `reply := mutation decideApproval(approvalId: "parent", decision: "approved")
 return reply`, false},
		{"host action", `reply := builtin workerRunShell(command: "touch file")
 return reply`, false},
		{"extra action", `reply := builtin askRunApprovalMessage()
 builtin workerRunShell(command: "touch file")
 return reply`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auto, err := loader.CompileSource("@template\nautomation askApprovalAnalysis {\n"+tc.body+"\n}", "test")
			if err != nil {
				t.Fatal(err)
			}
			err = approvalDiscussionTemplateRefusal(auto)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v: %v", tc.allowed, err)
			}
		})
	}
}
