package emailrules

// transactional_test.go -- an event rule never sends a template about one
// person's own request (X10). Such a template carries no unsubscribe, and a
// rule mails whoever its trigger and its recipients name: an audience, a row's
// address, the cluster's people. Arming refuses one, and a rule armed while its
// template was marketing refuses at fire time, on every lane, before it claims
// a row or reads anybody to mail.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

const transactionalSentence = "This template is about one person's own request and has no unsubscribe, so it cannot be sent to a list."

func transactionalTemplateRow() map[string]any {
	return map[string]any{"id": "v1:campaigns:template:t1", "subject": "s", "textBody": "b", "status": "ready", "purpose": "transactional"}
}

func TestAnEventRuleRefusesATransactionalTemplateWhenArmed(t *testing.T) {
	if errTransactionalTemplate.Error() != transactionalSentence {
		t.Fatalf("the refusal reads %q", errTransactionalTemplate)
	}
	audience := ruleRow()
	audience["recipientMode"] = ModeAudience
	audience["audienceId"] = "v1:campaigns:audience:a1"
	for name, rule := range map[string]map[string]any{"operational": ruleRow(), "marketing": audience} {
		armed := false
		e := &fakeEngine{rule: rule, template: transactionalTemplateRow(), activate: func(string, string) error { armed = true; return nil }}
		_, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), "v1:identity:user:owner1", "v1:campaigns:emailRule:ab12cd34")
		if !errors.Is(err, errTransactionalTemplate) {
			t.Errorf("%s rule: err = %v, want the transactional refusal", name, err)
		}
		if armed {
			t.Errorf("%s rule: a rule with a transactional template was armed", name)
		}
		if _, _, found := e.find("mutation createAuthoringConstruct"); found {
			t.Errorf("%s rule: a construct was written for a refused rule", name)
		}
		call, _, found := e.find("mutation recordEmailRuleGeneration")
		if !found || !strings.Contains(call, "has no unsubscribe, so it cannot be sent to a list") {
			t.Errorf("%s rule: the refusal is not on the rule row: %q", name, call)
		}
	}
}

// firingEngine answers one active rule and its template, and records every
// call. Anything else is answered empty.
type firingEngine struct {
	mu       sync.Mutex
	rule     map[string]any
	template map[string]any
	calls    []string
}

func (e *firingEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	e.calls = append(e.calls, q)
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query emailRuleById"):
		return rowsEnvelope([]map[string]any{e.rule}), nil
	case strings.HasPrefix(q, "query templateById"):
		return rowsEnvelope([]map[string]any{e.template}), nil
	case strings.HasPrefix(q, "query activeUsers"):
		return rowsEnvelope([]map[string]any{{"id": "v1:identity:user:owner1", "role": "owner", "primaryEmail": "owner@example.com"}}), nil
	}
	return rowsEnvelope(nil), nil
}

func (e *firingEngine) called(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, q := range e.calls {
		if strings.HasPrefix(q, prefix) {
			out = append(out, q)
		}
	}
	return out
}

func TestAnEventRuleRefusesATransactionalTemplateWhenItFires(t *testing.T) {
	for _, c := range []struct {
		name string
		mode string
	}{{"operational lane", ModeClusterRoles}, {"marketing lane, an audience", ModeAudience}, {"marketing lane, a row's address", ModeRowAddress}} {
		t.Run(c.name, func(t *testing.T) {
			rule := activeRule("created")
			rule["recipientMode"] = c.mode
			rule["audienceId"] = "v1:campaigns:audience:a1"
			rule["recipientField"] = "email"
			e := &firingEngine{rule: rule, template: transactionalTemplateRow()}
			claims := 0
			firer := NewFirer(e).WithFirstFireClaim(func(context.Context, string, string) bool { claims++; return true })

			out, err := firer.Fire(context.Background(), "v1:campaigns:emailRule:ab12cd34", "v1:identity:user:eve",
				map[string]any{"timestamp": "2026-09-27T10:00:00Z", "payload": map[string]any{"email": "eve@example.com"}})
			if !errors.Is(err, errTransactionalTemplate) {
				t.Fatalf("err = %v, want the transactional refusal", err)
			}
			if claims != 0 {
				t.Errorf("the row was claimed %d times for a firing that cannot send", claims)
			}
			if out.Sent != 0 || out.Recipients != 0 {
				t.Errorf("outcome = %+v, want nobody reached", out)
			}
			for _, prefix := range []string{"builtin campaignStageOperationalEmail", "builtin campaignSendToRecipient", "query activeUsers", "mutation addRecipient", "builtin campaignAddRecipient"} {
				if calls := e.called(prefix); len(calls) != 0 {
					t.Errorf("%s was called: %v", prefix, calls)
				}
			}
			records := e.called("mutation recordEmailRuleFiring")
			if len(records) != 1 || !strings.Contains(records[0], "has no unsubscribe, so it cannot be sent to a list") {
				t.Errorf("the firing was not recorded with the refusal: %v", records)
			}
		})
	}
}
