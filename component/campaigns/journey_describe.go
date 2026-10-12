package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type structuredAI interface {
	InvokeAIStructured(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error)
}

type journeyProposal struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Explanation string `json:"explanation"`
	Trigger     struct {
		Kind     string `json:"kind"`
		Audience string `json:"audience"`
	} `json:"trigger"`
	Steps []struct {
		Kind        string `json:"kind"`
		Template    string `json:"template"`
		Sender      string `json:"sender"`
		Days        int    `json:"days"`
		Hours       int    `json:"hours"`
		TimeoutDays int    `json:"timeoutDays"`
	} `json:"steps"`
}

// The catalog contains only authorized organization resources. Names are data;
// a model cannot manufacture a resource id or widen the caller's authority.
func (w *Worker) journeyCatalog(ctx context.Context, account string) (map[string][]map[string]any, error) {
	catalog := map[string][]map[string]any{}
	for _, name := range []string{"audiences", "templates", "senderIdentities"} {
		catalog[name] = []map[string]any{}
		cursor := ""
		seen := map[string]bool{}
		for {
			rows, next, err := w.store.rowsPage(ctx, cursor, call("query", name))
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if !sameOrganization(account, str(row, "accountId")) {
					continue
				}
				if name == "audiences" && str(row, "status") != "active" {
					continue
				}
				item := map[string]any{"id": bare(str(row, "id")), "status": str(row, "status")}
				switch name {
				case "audiences":
					item["name"], item["confirmation"] = str(row, "name"), str(row, "confirmation")
				case "templates":
					// The list deliberately carries no compiled bodies on newer hosts.
					tmpl, found, err := w.store.TemplateByID(ctx, str(row, "id"))
					if err != nil {
						return nil, err
					}
					if !found {
						continue
					}
					item["name"], item["confirmUrl"] = tmpl.Name, strings.Contains(tmpl.TextBody, "{{confirmUrl}}") || strings.Contains(tmpl.HTMLBody, "{{confirmUrl}}")
				case "senderIdentities":
					item["address"], item["fromName"] = str(row, "address"), str(row, "fromName")
				}
				catalog[name] = append(catalog[name], item)
			}
			if next == "" {
				break
			}
			if seen[next] {
				return nil, fmt.Errorf("automation catalog repeated a cursor")
			}
			seen[next] = true
			cursor = next
		}
	}
	return catalog, nil
}

func (w *Worker) handleDescribeJourney(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	account, sentence := bare(argString(args, "accountId")), strings.TrimSpace(argString(args, "sentence"))
	if sentence == "" || utf8.RuneCountInString(sentence) > 2000 {
		return nil, visitorMessage("Describe the automation in up to 2000 characters.")
	}
	if err := w.requireJourneyOrganization(ctx, account); err != nil {
		return nil, err
	}
	ai, ok := w.store.engine.(structuredAI)
	if !ok {
		return nil, visitorMessage("Describing an automation isn't available on this node.")
	}
	var catalog map[string][]map[string]any
	var response string
	_, err := workflowhost.Run(ctx, "campaignJourneyProposalWorkflow", map[string]any{"sentence": sentence}, workflowhost.Options{Logger: w.logger, Operations: map[string]workflowhost.Operation{
		"campaignJourneyCatalog": func(ctx context.Context, _ map[string]any) (any, error) {
			var err error
			catalog, err = w.journeyCatalog(ctx, account)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(catalog)
			return map[string]any{"encoded": string(encoded)}, err
		},
		"campaignGenerateJourneyProposal": func(ctx context.Context, a map[string]any) (any, error) {
			if catalog == nil {
				return nil, fmt.Errorf("read the automation catalog before drafting")
			}
			data, ok := a["data"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("automation proposal data is missing")
			}
			var err error
			response, err = ai.InvokeAIStructured(ctx, str(a, "prompt"), data, "email_journey_proposal", journeyProposalSchema, true)
			return nil, err
		},
	}})
	if err != nil {
		return nil, err
	}
	var proposal journeyProposal
	if err = json.Unmarshal([]byte(response), &proposal); err != nil {
		return nil, visitorMessage("The proposed automation was refused: the response was not a structured automation.")
	}
	if proposal.Action == "unsupported" {
		return nil, visitorMessage("Couldn't draft this automation: " + proposal.Explanation)
	}
	if proposal.Action != "draft" {
		return nil, visitorMessage("The proposed automation was refused: choose a draft action.")
	}
	resolve := func(kind, id string) string {
		for _, r := range catalog[kind] {
			if bare(id) == str(r, "id") {
				return str(r, "id")
			}
		}
		return ""
	}
	draftArgs := map[string]any{"name": proposal.Name, "trigger": map[string]any{"kind": proposal.Trigger.Kind, "audienceId": resolve("audiences", proposal.Trigger.Audience)}}
	steps := []map[string]any{}
	extra := []JourneyIssue{}
	for i, s := range proposal.Steps {
		stepID := fmt.Sprintf("step-%d", i+1)
		step := map[string]any{"id": stepID, "kind": s.Kind}
		switch s.Kind {
		case "wait":
			step["days"], step["hours"] = s.Days, s.Hours
		case "confirm", "send":
			step["templateId"], step["senderIdentityId"] = resolve("templates", s.Template), resolve("senderIdentities", s.Sender)
			if step["templateId"] == "" && strings.TrimSpace(s.Template) != "" {
				extra = append(extra, JourneyIssue{StepID: stepID, Text: fmt.Sprintf("Create a template named “%s”.", s.Template)})
			}
			if s.Kind == "confirm" {
				step["timeoutDays"] = s.TimeoutDays
			}
		}
		steps = append(steps, step)
	}
	draftArgs["steps"] = steps
	draft, err := parseJourneyDraft(draftArgs)
	if err != nil {
		return nil, visitorMessage("The proposed automation was refused: " + err.Error())
	}
	issues, err := w.journeyIssues(ctx, account, draft)
	if err != nil {
		return nil, err
	}
	issues = append(issues, extra...)
	return resultNode("campaignJourneyProposal", map[string]any{"proposal": journeyDraftValues(draft), "explanation": proposal.Explanation, "issues": issues})
}

var journeyProposalSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action","name","trigger","steps","explanation"],"properties":{
 "action":{"type":"string","enum":["draft","unsupported"]},
 "name":{"type":"string"},"explanation":{"type":"string"},
 "trigger":{"type":"object","additionalProperties":false,"required":["kind","audience"],"properties":{"kind":{"type":"string","enum":["joined","confirmed"]},"audience":{"type":"string"}}},
 "steps":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["kind","template","sender","days","hours","timeoutDays"],"properties":{
  "kind":{"type":"string","enum":["send","confirm","wait"]},"template":{"type":"string"},"sender":{"type":"string"},
  "days":{"type":"integer"},"hours":{"type":"integer"},"timeoutDays":{"type":"integer"}}}}}}`)
