package packages

import (
	"strings"
	"testing"
)

func TestSourceRegistrationRequiresOrganizationBeforeResolvingGitHub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account any
	}{{"missing", nil}, {"empty", ""}, {"whitespace", "  "}} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &recordingEngine{}
			logger, _ := capturedLog()
			i, _ := credentialHarness(t, engine, logger)
			args := map[string]any{
				"credentialId":       "grant-one",
				"sourceConnectionId": "source-one",
				"repoUrl":            "https://github.com/acme/widget",
				"accountId":          tc.account,
			}
			if tc.account == nil {
				delete(args, "accountId")
			}
			nodes, err := i.handleSourceRegister(callerCtx("requester"), args, 0)
			if RefusalCode(err) != "organization_required" || !strings.Contains(err.Error(), "select an active MemQL organization") {
				t.Fatalf("want actionable organization refusal, got %v", err)
			}
			if len(nodes) != 0 || len(engine.statements()) != 0 {
				t.Fatalf("missing ownership must not read credentials or write a source: nodes=%v statements=%v", nodes, engine.statements())
			}
		})
	}
}

func TestSourceRegistrationStillRequiresOwnedConnectionInputs(t *testing.T) {
	for _, missing := range []string{"credentialId", "sourceConnectionId"} {
		t.Run(missing, func(t *testing.T) {
			engine := &recordingEngine{}
			logger, _ := capturedLog()
			i, _ := credentialHarness(t, engine, logger)
			args := map[string]any{"credentialId": "grant-one", "sourceConnectionId": "source-one", "accountId": "self"}
			delete(args, missing)
			nodes, err := i.handleSourceRegister(callerCtx("requester"), args, 0)
			if RefusalCode(err) != "source_connection_unavailable" || len(nodes) != 0 || len(engine.statements()) != 0 {
				t.Fatalf("missing %s must retain the source refusal without effects: nodes=%v err=%v statements=%v", missing, nodes, err, engine.statements())
			}
		})
	}
}
