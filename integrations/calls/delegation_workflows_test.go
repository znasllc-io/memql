package calls

import (
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
)

func TestAgentRequestScopeRejectsMissingAndMixedEnvelopes(t *testing.T) {
	for _, r := range []callrules.AgentRequest{
		{Action: "invite"}, {Action: "workflow"}, {Action: "resource"},
		{Action: "invite", Invitation: &callrules.InviteRequest{}, Workflow: &callrules.WorkflowRequest{Action: "snapshot"}},
		{Action: "workflow", Workflow: &callrules.WorkflowRequest{Action: "start"}, Resource: &callrules.ResourceRequest{Action: "start"}},
	} {
		if got := agentRequestScope(r); got != "" {
			t.Fatalf("unbounded or incomplete envelope accepted: %s", r.Action)
		}
	}
	if agentRequestScope(callrules.AgentRequest{Action: "invite", Invitation: &callrules.InviteRequest{}}) != "invite" {
		t.Fatal("valid explicit invitation envelope rejected")
	}
}
