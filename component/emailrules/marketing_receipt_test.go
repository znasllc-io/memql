package emailrules

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type marketingReceiptEngine struct {
	rule, result map[string]any
	sends        []string
}

func (e *marketingReceiptEngine) Execute(_ context.Context, q string) (any, error) {
	switch {
	case strings.HasPrefix(q, "query emailRuleById"):
		return rowsEnvelope([]map[string]any{e.rule}), nil
	case strings.HasPrefix(q, "query sendableRecipientsForAudience"):
		return rowsEnvelope([]map[string]any{{"id": "recipient1", "email": "reader@example.com"}}), nil
	case strings.HasPrefix(q, "builtin campaignSendToRecipient"):
		e.sends = append(e.sends, q)
		if e.result == nil {
			return rowsEnvelope(nil), nil
		}
		return rowsEnvelope([]map[string]any{e.result}), nil
	case strings.HasPrefix(q, "mutation recordEmailRuleFiring"):
		return rowsEnvelope(nil), nil
	default:
		return nil, fmt.Errorf("unexpected call: %s", firstWords(q))
	}
}

func TestMarketingRuleCountsOnlyNewAcceptedMessages(t *testing.T) {
	for _, mode := range []string{ModeAudience, ModeRowAddress} {
		for _, tc := range []struct {
			name          string
			result        map[string]any
			sent, skipped int
			refusal       bool
		}{
			{"accepted", map[string]any{"sent": true}, 1, 0, false},
			{"suppressed", map[string]any{"sent": false, "skipped": true}, 0, 1, false},
			{"replayed", map[string]any{"sent": true, "replayed": true}, 0, 1, false},
			{"uncertain", map[string]any{"uncertain": true, "replayed": true}, 0, 0, true},
			{"unrecognized", map[string]any{"sent": false}, 0, 0, true},
			{"missing receipt", nil, 0, 0, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				rule := activeRule("updated")
				rule["recipientMode"], rule["audienceId"], rule["recipientField"] = mode, "audience1", "email"
				engine := &marketingReceiptEngine{rule: rule, result: tc.result}
				firer := NewFirer(engine)
				event := map[string]any{"timestamp": "2026-10-01T01:00:00.000001Z", "payload": map[string]any{"email": "reader@example.com"}}
				out, err := firer.Fire(context.Background(), str(rule, "id"), "row1", event)
				if err != nil || out.Sent != tc.sent || out.Skipped != tc.skipped || (len(out.Refusals) > 0) != tc.refusal {
					t.Fatalf("outcome=%+v err=%v", out, err)
				}
				if len(engine.sends) != 1 || !strings.Contains(engine.sends[0], `requestId: "rule_`) {
					t.Fatalf("missing stable request: %v", engine.sends)
				}
			})
		}
	}
}

func TestMarketingEventIdentitySurvivesReplayWithoutCollapsingUpdates(t *testing.T) {
	rule := Rule{ID: "rule1", EventKind: "updated"}
	event := map[string]any{"timestamp": "2026-10-01T01:00:00.000001Z", "payload": map[string]any{"email": "reader@example.com", "name": "Reader"}}
	first, err := marketingRequestID(rule, "row1", "recipient1", event)
	if err != nil {
		t.Fatal(err)
	}
	// JSON materialization on another node can reorder an object's keys.
	event["payload"] = map[string]any{"name": "Reader", "email": "reader@example.com"}
	if replay, _ := marketingRequestID(rule, "row1", "recipient1", event); replay != first {
		t.Fatal("replay changed the request identifier")
	}
	event["timestamp"] = "2026-10-01T01:00:00.000002Z"
	if next, _ := marketingRequestID(rule, "row1", "recipient1", event); next == first {
		t.Fatal("two updates in the same second collapsed")
	}
	delete(event, "timestamp")
	if _, err := marketingRequestID(rule, "row1", "recipient1", event); err == nil {
		t.Fatal("unidentifiable update accepted")
	}
	rule.EventKind = "created"
	created, err := marketingRequestID(rule, "row1", "recipient1", event)
	if err != nil {
		t.Fatal(err)
	}
	event["timestamp"] = "2026-10-01T01:00:00Z"
	if again, _ := marketingRequestID(rule, "row1", "recipient1", event); again != created {
		t.Fatal("racing row creations named different welcomes")
	}
	if other, _ := marketingRequestID(rule, "row1", "recipient2", event); other == created {
		t.Fatal("two recipients share a receipt")
	}
}
