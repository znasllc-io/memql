package campaigns

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// One bounded delivery effect. The authored rule selects when it runs; the
// native boundary proves its current authority, recipient set and template.
func (w *Worker) handleStageOperationalEmail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	ruleID, templateID := bare(argString(args, "emailRuleId")), bare(argString(args, "templateId"))
	rows, err := w.store.rows(ctx, call("query", "emailRuleById", arg{"emailRuleId", ruleID}))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || str(rows[0], "status") != "active" || str(rows[0], "recipientMode") != "cluster_roles" || bare(str(rows[0], "templateId")) != templateID {
		return nil, fmt.Errorf("an active operational rule must name this template")
	}
	rule := rows[0]
	if err := w.requireSendAuthority(ctx, str(rule, "accountId")); err != nil {
		return nil, err
	}
	tmpl, found, err := w.store.TemplateByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if !found || !sameOrganization(tmpl.AccountID, str(rule, "accountId")) {
		return nil, fmt.Errorf("choose the rule's template in its organization")
	}
	if tmpl.Status != "ready" {
		return nil, fmt.Errorf("publish the rule's template before it can send")
	}
	// The builtin is directly callable. It cannot trust an arbitrary target supplied
	// alongside a valid rule: resolve the selected cluster user again at this seam.
	users, err := w.store.rows(auth.ContextWithInternalOrigin(ctx), call("query", "activeUsers"))
	if err != nil {
		return nil, err
	}
	wanted := []string{}
	switch values := rule["recipientRoles"].(type) {
	case []any:
		for _, v := range values {
			if s, ok := v.(string); ok {
				wanted = append(wanted, s)
			}
		}
	case []string:
		wanted = values
	}
	for i := range wanted {
		wanted[i] = strings.ToLower(strings.TrimSpace(wanted[i]))
	}
	target := NormalizeEmail(argString(args, "to"))
	var recipient Recipient
	for _, u := range users {
		addr := str(u, "primaryEmail")
		if addr == "" {
			addr = str(u, "email")
		}
		role := strings.ToLower(str(u, "role"))
		allowed := len(wanted) == 0 && role == "owner"
		for _, v := range wanted {
			allowed = allowed || role == v
		}
		if allowed && target != "" && NormalizeEmail(addr) == target {
			recipient = Recipient{Email: target, DisplayName: str(u, "displayName")}
			break
		}
	}
	if recipient.Email == "" {
		return nil, fmt.Errorf("the recipient is outside this rule's selected cluster roles")
	}
	accountName, err := w.store.AccountName(ctx, tmpl.AccountID)
	if err != nil {
		return nil, err
	}
	subject, text, html := renderOperational(tmpl, recipient, accountName, str(rule, "name"))
	requestID := argString(args, "requestId")
	if strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("an operational email needs a request id")
	}
	err = w.store.execServerOnly(ctx, call("mutation", "stageServerOutboundRequest", arg{"requestId", requestID}, arg{"medium", "email"}, arg{"target", target}, arg{"subject", subject}, arg{"body", text}, arg{"htmlBody", html}, arg{"dedupeKey", argString(args, "dedupeKey")}, arg{"requestedBy", str(rule, "ownerUserId")}))
	if err != nil {
		return nil, err
	}
	return resultNode("campaignOperationalEmail", map[string]any{"staged": true, "requestId": requestID})
}

func renderOperational(t Template, r Recipient, accountName, campaignName string) (subject, text, html string) {
	plain, escaped := mergeReplacers(Campaign{Name: campaignName}, r, accountName, MessageLinks{})
	return plain.Replace(t.Subject), plain.Replace(t.TextBody), escaped.Replace(t.HTMLBody)
}
