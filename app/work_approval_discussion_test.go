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
		{"advice", `reply := builtin askRunApprovalMessage(templateId: "askApprovalReply")
 return reply`, true},
		{"mutation", `reply := mutation decideApproval(approvalId: "parent", decision: "approved")
 return reply`, false},
		{"host action", `reply := builtin workerRunShell(command: "touch file")
 return reply`, false},
		{"extra action", `reply := builtin askRunApprovalMessage(templateId: "askApprovalReply")
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
