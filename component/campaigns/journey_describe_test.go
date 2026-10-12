package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type proposalEngine struct {
	*fakeEngine
	answer   string
	aiCalls  int
	catalogs map[string]bool
}

func (e *proposalEngine) Execute(ctx context.Context, q string) (any, error) {
	for _, name := range []string{"audiences", "templates", "senderIdentities"} {
		if strings.HasPrefix(q, "query "+name+"(") {
			e.catalogs[name] = true
			switch name {
			case "audiences":
				return rowsEnvelope([]map[string]any{e.audiences["a1"]}), nil
			case "templates":
				return rowsEnvelope([]map[string]any{e.template}), nil
			default:
				return rowsEnvelope([]map[string]any{e.senderIdentities["s1"]}), nil
			}
		}
	}
	return e.fakeEngine.Execute(ctx, q)
}
func (e *proposalEngine) InvokeAIStructured(_ context.Context, prompt string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (string, error) {
	e.aiCalls++
	if len(e.catalogs) != 3 || !strings.Contains(fmt.Sprint(data["catalog"]), "Confirm") || prompt != "composeEmailJourney" || schemaName != "email_journey_proposal" || !strict || !json.Valid(schema) {
		return "", fmt.Errorf("proposal did not read its authorized catalog first")
	}
	return e.answer, nil
}
func proposalFixture() *proposalEngine {
	return &proposalEngine{fakeEngine: &fakeEngine{
		accounts:         map[string]map[string]any{"org": {"id": "org", "status": "active"}},
		audiences:        map[string]map[string]any{"a1": {"id": "a1", "name": "Newsletter", "status": "active", "accountId": "org", "confirmation": "required"}},
		template:         map[string]any{"id": "t1", "name": "Confirm", "status": "ready", "accountId": "org", "textBody": "Confirm: {{confirmUrl}}"},
		senderIdentities: map[string]map[string]any{"s1": {"id": "s1", "accountId": "org", "status": "active", "address": "hello@example.test"}},
	}, catalogs: map[string]bool{}, answer: `{"action":"draft","name":"Welcome","explanation":"Ask for confirmation","trigger":{"kind":"joined","audience":"a1"},"steps":[{"kind":"confirm","template":"t1","sender":"s1","days":0,"hours":0,"timeoutDays":7}]}`}
}
func TestDescribeJourneyReadsCatalogAndReturnsValidatedProposalWithoutWriting(t *testing.T) {
	e := proposalFixture()
	w := newTestWorker(t, e, &recordingSender{})
	nodes, err := w.handleDescribeJourney(importCtx(), map[string]any{"accountId": "org", "sentence": "Ask people to confirm"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, nodes)
	if len(out["issues"].([]any)) != 0 {
		t.Fatalf("issues: %v", out)
	}
	draft := out["proposal"].(map[string]any)
	step := draft["steps"].([]any)[0].(map[string]any)
	if step["id"] != "step-1" || step["templateId"] != "t1" || e.aiCalls != 1 {
		t.Fatalf("proposal: %v calls %d", draft, e.aiCalls)
	}
	if len(callsWithPrefix(e.fakeEngine, "mutation ")) != 0 {
		t.Fatal("proposal wrote state")
	}
}
func TestDescribeJourneyNamesMissingTemplateAndRefusesInvalidProposal(t *testing.T) {
	for _, tc := range []struct{ name, answer, want string }{
		{"missing", strings.ReplaceAll(proposalFixture().answer, `"template":"t1"`, `"template":"Welcome note"`), "Create a template named “Welcome note”."},
		{"unsupported", `{"action":"unsupported","explanation":"A purchase trigger isn't available."}`, "Couldn't draft this automation: A purchase trigger isn't available."},
		{"zero wait", strings.ReplaceAll(proposalFixture().answer, `"kind":"confirm"`, `"kind":"wait"`), "The proposed automation was refused: A wait lasts from one hour to 365 days."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := proposalFixture()
			e.answer = tc.answer
			w := newTestWorker(t, e, &recordingSender{})
			nodes, err := w.handleDescribeJourney(importCtx(), map[string]any{"accountId": "org", "sentence": "Welcome people"}, 0)
			if tc.name == "missing" {
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(decodeResult(t, nodes))
				if !strings.Contains(string(raw), tc.want) {
					t.Fatalf("%s", raw)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v want %q", err, tc.want)
			}
		})
	}
}
