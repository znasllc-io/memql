package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

var templateIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// Templates are files in the editor and reusable records in Campaigns. One
// shared lock protects both changes of copy and the explicit review decision.
// Publishing is always on the exact saved bytes the person just reviewed.
func (w *Worker) handleSaveTemplate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	templateID, accountID := bare(argString(args, "templateId")), bare(argString(args, "accountId"))
	name, expected, action := strings.TrimSpace(argString(args, "name")), argString(args, "expectedRevision"), argString(args, "action")
	if action == "" {
		action = "save"
	}
	if !templateIDPattern.MatchString(templateID) || accountID == "" || name == "" || len(name) > 200 || strings.IndexFunc(name, unicode.IsControl) >= 0 || callerUserID(ctx) == "" {
		return nil, fmt.Errorf("choose an organization and a template name (up to 200 characters)")
	}
	if action != "save" && action != "publish" && action != "archive" {
		return nil, fmt.Errorf("unknown template action")
	}
	encoded, err := json.Marshal(args["content"])
	if err != nil {
		return nil, err
	}
	content, err := pure.ParseEmailTemplate(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	if w.templateGate == nil {
		return nil, fmt.Errorf("template write coordination is unavailable")
	}
	release, err := w.templateGate(ctx, templateID)
	if err != nil {
		return nil, err
	}
	defer release()
	accounts, err := w.store.rows(ctx, call("query", "clientAccountById", arg{"accountId", accountID}))
	if err != nil {
		return nil, err
	}
	if len(accounts) != 1 || str(accounts[0], "status") != "active" {
		return nil, fmt.Errorf("organization is not active or accessible")
	}
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", templateID}))
	if err != nil {
		return nil, err
	}
	verb := auth.VerbCreate
	if len(rows) > 0 {
		verb = auth.VerbUpdate
	}
	if !w.store.engine.OrganizationCapable(ctx, accountID, verb, auth.ResourceData) {
		return nil, fmt.Errorf("template write permission is required in this organization")
	}
	var prior time.Time
	status, mutation := "draft", "createTemplate"
	if len(rows) > 0 {
		row := rows[0]
		if !sameOrganization(accountID, str(row, "accountId")) {
			return nil, fmt.Errorf("a template cannot move between organizations; create a copy instead")
		}
		prior = templateTime(row["createdAt"])
		if prior.IsZero() {
			return nil, fmt.Errorf("template has no readable revision")
		}
		same := name == str(row, "name") && content.Subject == str(row, "subject") && content.TextBody == str(row, "textBody") && content.HTMLBody == str(row, "htmlBody")
		// An uncertain response can be retried without writing another version.
		// This also makes file autosave of unchanged bytes inert.
		target := map[string]string{"save": "draft", "publish": "ready", "archive": "archived"}[action]
		if same && str(row, "status") == target {
			return templateReceipt(templateID, prior, target)
		}
		wanted, parseErr := time.Parse(time.RFC3339Nano, expected)
		if parseErr != nil || !prior.Equal(wanted) {
			return nil, fmt.Errorf("this template changed; compare the latest revision before saving or publishing")
		}
		if action != "save" && !same {
			return nil, fmt.Errorf("save and review this revision before publishing or archiving")
		}
		status, mutation = target, "updateTemplate"
	} else if expected != "" || action != "save" {
		return nil, fmt.Errorf("template not found; create and save it before publishing")
	}
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	revision := memql.VersionTimeAfter(prior, now)
	values := []arg{{"templateId", templateID}, {"name", name}, {"subject", content.Subject}, {"textBody", content.TextBody}, {"htmlBody", content.HTMLBody}, {"versionTime", revision.Format(time.RFC3339Nano)}}
	if mutation == "createTemplate" {
		values = append(values, arg{"accountId", accountID})
	} else {
		values = append(values, arg{"status", status})
	}
	if err := w.store.execServerOnly(ctx, call("mutation", mutation, values...)); err != nil {
		return nil, err
	}
	return templateReceipt(templateID, revision, status)
}

func templateTime(value any) time.Time {
	if instant, ok := value.(time.Time); ok {
		return instant
	}
	instant, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(value))
	return instant
}

func templateReceipt(templateID string, revision time.Time, status string) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignTemplateSaved", map[string]any{"templateId": templateID, "revision": revision.UTC().Format(time.RFC3339Nano), "status": status, "saved": true})
}
