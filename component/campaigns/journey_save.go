package campaigns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type journeyTrigger struct {
	Kind       string `json:"kind"`
	AudienceID string `json:"audienceId"`
}
type journeyStep struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	TemplateID       string `json:"templateId,omitempty"`
	SenderIdentityID string `json:"senderIdentityId,omitempty"`
	Days             int    `json:"days,omitempty"`
	Hours            int    `json:"hours,omitempty"`
	TimeoutDays      int    `json:"timeoutDays,omitempty"`
}
type journeyDraft struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Trigger     journeyTrigger `json:"trigger"`
	Steps       []journeyStep  `json:"steps"`
}
type JourneyIssue struct {
	StepID string `json:"stepId"`
	Text   string `json:"text"`
}
type journeyRow struct {
	ID, OwnerUserID, AccountID, Name, Description, Status, LastError string
	Trigger                                                          journeyTrigger
	Steps                                                            []journeyStep
	ActivatedAt, RetiredAt, Revision                                 time.Time
}

var journeyStepIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func decodeJourneyValue(v any, out any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func parseJourneyDraft(args map[string]any) (journeyDraft, error) {
	d := journeyDraft{Name: strings.TrimSpace(argString(args, "name")), Description: strings.TrimSpace(argString(args, "description"))}
	refuse := func(message string) (journeyDraft, error) { return journeyDraft{}, visitorMessage(message) }
	if d.Name == "" || utf8.RuneCountInString(d.Name) > 200 || strings.IndexFunc(d.Name, unicode.IsControl) >= 0 {
		return refuse("Name the automation (up to 200 characters).")
	}
	if utf8.RuneCountInString(d.Description) > 1000 {
		return refuse("Keep the description under 1000 characters.")
	}
	if err := decodeJourneyValue(args["trigger"], &d.Trigger); err != nil {
		return refuse("The trigger names only its kind and audience.")
	}
	if d.Trigger.Kind != "joined" && d.Trigger.Kind != "confirmed" {
		return refuse("Choose when the automation starts: when someone joins or when someone confirms.")
	}
	d.Trigger.AudienceID = bare(strings.TrimSpace(d.Trigger.AudienceID))
	var raw []map[string]any
	if err := decodeJourneyValue(args["steps"], &raw); err != nil || len(raw) < 1 || len(raw) > 20 {
		return refuse("An automation has 1 to 20 steps.")
	}
	seen := map[string]bool{}
	for i, r := range raw {
		kind := str(r, "kind")
		allowed := map[string]bool{"id": true, "kind": true}
		switch kind {
		case "send":
			allowed["templateId"], allowed["senderIdentityId"] = true, true
		case "confirm":
			allowed["templateId"], allowed["senderIdentityId"], allowed["timeoutDays"] = true, true, true
		case "wait":
			allowed["days"], allowed["hours"] = true, true
		default:
			return refuse("A step sends an email, asks for confirmation, or waits.")
		}
		for key := range r {
			if !allowed[key] {
				return refuse(fmt.Sprintf("Step %d has a field it does not use: %s.", i+1, key))
			}
		}
		var step journeyStep
		if err := decodeJourneyValue(r, &step); err != nil {
			return refuse("Use whole numbers for days and hours.")
		}
		if !journeyStepIDPattern.MatchString(step.ID) || seen[step.ID] {
			return refuse("Give each step a unique id of up to 40 letters, digits, hyphens or underscores.")
		}
		seen[step.ID] = true
		step.TemplateID, step.SenderIdentityID = bare(strings.TrimSpace(step.TemplateID)), bare(strings.TrimSpace(step.SenderIdentityID))
		if step.Kind == "wait" && (step.Days < 0 || step.Days > 365 || step.Hours < 0 || step.Hours > 23 || step.Days*24+step.Hours < 1 || step.Days*24+step.Hours > 365*24) {
			return refuse("A wait lasts from one hour to 365 days.")
		}
		if step.Kind == "confirm" {
			if _, exists := r["timeoutDays"]; !exists {
				step.TimeoutDays = 7
			}
			if step.TimeoutDays < 1 || step.TimeoutDays > 30 {
				return refuse("A confirmation waits 1 to 30 days.")
			}
		}
		d.Steps = append(d.Steps, step)
	}
	return d, nil
}

func journeyFromRow(row map[string]any) (journeyRow, error) {
	var trigger journeyTrigger
	var steps []journeyStep
	if err := decodeJourneyValue(row["trigger"], &trigger); err != nil {
		return journeyRow{}, err
	}
	if err := decodeJourneyValue(row["steps"], &steps); err != nil {
		return journeyRow{}, err
	}
	return journeyRow{ID: bare(str(row, "id")), OwnerUserID: bare(str(row, "ownerUserId")), AccountID: bare(str(row, "accountId")), Name: str(row, "name"), Description: str(row, "description"), Status: str(row, "status"), LastError: str(row, "lastError"), Trigger: trigger, Steps: steps, ActivatedAt: templateTime(row["activatedAt"]), RetiredAt: templateTime(row["retiredAt"]), Revision: templateTime(row["createdAt"])}, nil
}

func (w *Worker) journeyIssues(ctx context.Context, accountID string, d journeyDraft) ([]JourneyIssue, error) {
	issues := []JourneyIssue{}
	add := func(id, text string) { issues = append(issues, JourneyIssue{StepID: id, Text: text}) }
	audience, found, err := w.store.AudienceByID(ctx, d.Trigger.AudienceID)
	if err != nil {
		return nil, err
	}
	if d.Trigger.AudienceID == "" {
		add("", "Choose an audience.")
	} else if !found || !sameOrganization(accountID, audience.AccountID) {
		add("", "Choose an audience in this organization.")
	} else if audience.Status == "archived" {
		add("", fmt.Sprintf("The audience “%s” is archived.", audience.Name))
	}
	emails, confirms := 0, 0
	for _, step := range d.Steps {
		if step.Kind == "wait" {
			continue
		}
		emails++
		if emails == 1 && d.Trigger.Kind == "joined" && audience.Confirmation == "required" && step.Kind != "confirm" {
			add(step.ID, "This audience asks people to confirm, so the first email must ask them to confirm.")
		}
		if step.Kind == "confirm" {
			confirms++
			if d.Trigger.Kind == "confirmed" {
				add(step.ID, "People in this automation have already confirmed. Remove this step.")
			}
			if confirms > 1 {
				add(step.ID, "An automation asks people to confirm once.")
			}
		}
		if step.TemplateID == "" {
			add(step.ID, "Choose a template.")
		} else {
			t, ok, err := w.store.TemplateByID(ctx, step.TemplateID)
			if err != nil {
				return nil, err
			}
			if !ok || !sameOrganization(accountID, t.AccountID) {
				add(step.ID, "Choose a template in this organization.")
			} else {
				name := t.Name
				if name == "" {
					name = t.Subject
				}
				if t.Status != "ready" {
					add(step.ID, fmt.Sprintf("Publish “%s” before turning this on.", name))
				}
				if step.Kind == "confirm" && !strings.Contains(t.TextBody, "{{confirmUrl}}") && !strings.Contains(t.HTMLBody, "{{confirmUrl}}") {
					add(step.ID, fmt.Sprintf("Add {{confirmUrl}} to “%s” so people can confirm.", name))
				}
			}
		}
		if step.SenderIdentityID == "" {
			if bare(accountID) != "self" {
				add(step.ID, "Choose a sender.")
			}
		} else {
			sender, ok, err := w.store.SenderIdentityByID(ctx, step.SenderIdentityID)
			if err != nil {
				return nil, err
			}
			if !ok || !sameOrganization(accountID, sender.AccountID) {
				add(step.ID, "Choose a sender in this organization.")
			} else if sender.Disabled() {
				add(step.ID, fmt.Sprintf("The sender %s is turned off.", sender.Address))
			}
		}
	}
	if emails == 0 {
		add("", "Add at least one email.")
	}
	return issues, nil
}
func (w *Worker) requireJourneyOrganization(ctx context.Context, account string) error {
	if err := w.requireSendAuthority(ctx, account); err != nil {
		return err
	}
	// Organization status is operational state. Read it with the system actor
	// only AFTER resolving the real owner's current scoped write authority.
	rows, err := w.store.rows(memql.ContextWithFreshRead(w.systemActorContext(ctx)), call("query", "clientAccountById", arg{"accountId", account}))
	if err != nil {
		return err
	}
	if len(rows) != 1 || str(rows[0], "status") != "active" {
		return fmt.Errorf("organization is not active or accessible")
	}
	return nil
}
func journeyDraftValues(d journeyDraft) map[string]any {
	encoded, _ := json.Marshal(d)
	var values map[string]any
	_ = json.Unmarshal(encoded, &values)
	if _, ok := values["description"]; !ok {
		values["description"] = ""
	}
	return values
}
func (w *Worker) handleSaveJourney(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	id, account := bare(argString(args, "journeyId")), bare(argString(args, "accountId"))
	if !templateIDPattern.MatchString(id) || account == "" || callerUserID(ctx) == "" {
		return nil, fmt.Errorf("choose an organization and an automation identifier")
	}
	d, err := parseJourneyDraft(args)
	if err != nil {
		return nil, err
	}
	if w.journeyGate == nil {
		return nil, fmt.Errorf("automation coordination is unavailable")
	}
	release, err := w.journeyGate(ctx, "journey:"+id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err = w.requireJourneyOrganization(ctx, account); err != nil {
		return nil, err
	}
	prior, err := w.newsletterRow(ctx, "journeyById", "journeyId", id)
	if err != nil {
		return nil, err
	}
	values := journeyDraftValues(d)
	mutation := "createJourney"
	status := "draft"
	if prior != nil {
		if !sameOrganization(account, str(prior, "accountId")) {
			return nil, fmt.Errorf("an automation cannot change organizations")
		}
		status = str(prior, "status")
		if status == "active" {
			return nil, visitorMessage("Pause the automation before editing it.")
		}
		if status == "archived" {
			return nil, visitorMessage("An archived automation can't be changed.")
		}
		current, parseErr := journeyFromRow(prior)
		if parseErr != nil {
			return nil, parseErr
		}
		currentDraft := journeyDraft{Name: current.Name, Description: current.Description, Trigger: current.Trigger, Steps: current.Steps}
		a, _ := json.Marshal(currentDraft)
		b, _ := json.Marshal(d)
		if bytes.Equal(a, b) {
			issues, err := w.journeyIssues(ctx, account, d)
			if err != nil {
				return nil, err
			}
			return journeyReceipt(id, current.Revision, status, issues)
		}
		if !current.Revision.Equal(templateTime(args["expectedRevision"])) || current.Revision.IsZero() {
			return nil, visitorMessage("This automation changed; reload before saving.")
		}
		mutation = "updateJourney"
	} else if argString(args, "expectedRevision") != "" {
		return nil, visitorMessage("This automation changed; reload before saving.")
	}
	issues, err := w.journeyIssues(ctx, account, d)
	if err != nil {
		return nil, err
	}
	revision := memql.VersionTimeAfter(templateTime(prior["createdAt"]), w.nowUTC())
	values["journeyId"], values["versionTime"] = id, revision.Format(time.RFC3339Nano)
	if mutation == "createJourney" {
		values["accountId"] = account
	}
	if err = w.newsletterWrite(ctx, mutation, values); err != nil {
		return nil, err
	}
	return journeyReceipt(id, revision, status, issues)
}
func journeyReceipt(id string, revision time.Time, status string, issues []JourneyIssue) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignJourneySaved", map[string]any{"saved": true, "journeyId": id, "revision": revision.Format(time.RFC3339Nano), "status": status, "issues": issues})
}
func (w *Worker) handleSetJourneyState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	id := bare(argString(args, "journeyId"))
	action := argString(args, "action")
	if !templateIDPattern.MatchString(id) || w.journeyGate == nil {
		return nil, fmt.Errorf("select an automation while connected to the cluster")
	}
	release, err := w.journeyGate(ctx, "journey:"+id)
	if err != nil {
		return nil, err
	}
	defer release()
	row, err := w.newsletterRow(ctx, "journeyById", "journeyId", id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("automation is not readable")
	}
	j, err := journeyFromRow(row)
	if err != nil {
		return nil, err
	}
	if err = w.requireJourneyOrganization(ctx, j.AccountID); err != nil {
		return nil, err
	}
	if j.Status == "archived" {
		return nil, visitorMessage("An archived automation can't be changed.")
	}
	if j.Revision.IsZero() || !j.Revision.Equal(templateTime(args["expectedRevision"])) {
		return nil, visitorMessage("This automation changed; reload before saving.")
	}
	target := ""
	issues := []JourneyIssue{}
	values := map[string]any{"journeyId": id}
	switch action {
	case "activate":
		if j.Status != "draft" && j.Status != "paused" {
			return nil, visitorMessage("Only a draft or paused automation can be turned on.")
		}
		issues, err = w.journeyIssues(ctx, j.AccountID, journeyDraft{Name: j.Name, Description: j.Description, Trigger: j.Trigger, Steps: j.Steps})
		if err != nil {
			return nil, err
		}
		if len(issues) > 0 {
			text := []string{}
			for _, issue := range issues {
				text = append(text, issue.Text)
			}
			return nil, visitorMessage(strings.Join(text, " "))
		}
		target = "active"
		if j.ActivatedAt.IsZero() {
			values["activatedAt"] = w.nowUTC().Format(time.RFC3339Nano)
		}
		values["lastError"] = ""
	case "pause":
		if j.Status != "active" {
			return nil, visitorMessage("Only an active automation can be paused.")
		}
		target = "paused"
	case "archive":
		target = "archived"
	default:
		return nil, fmt.Errorf("choose activate, pause or archive")
	}
	revision := memql.VersionTimeAfter(j.Revision, w.nowUTC())
	values["status"], values["versionTime"] = target, revision.Format(time.RFC3339Nano)
	if err = w.newsletterWrite(ctx, "setJourneyStatus", values); err != nil {
		return nil, err
	}
	return journeyReceipt(id, revision, target, issues)
}
