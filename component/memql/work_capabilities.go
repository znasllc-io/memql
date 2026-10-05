package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func (e *MemQLEngine) workCapabilityAllowed(ctx context.Context, fn *Function) bool {
	if fn == nil || !fn.Enabled || fn.ServerOnly || (strings.HasPrefix(fn.Name, "ask") && fn.Name != "askConversationById") || fn.Name == "workAcknowledgementCache" || fn.Name == "indexConversationMemory" || fn.Name == "workCapabilities" || fn.Name == "workExecute" || fn.Name == "createGoal" || fn.Name == "runAgentTurn" || strings.HasSuffix(fn.Name, "AskConversation") {
		return false
	}
	// Human gates must use their dedicated tools so the agent loop persists
	// its continuation and parks. Wrapping them in executeCapability would
	// turn an awaiting-user receipt into an ordinary result and keep acting.
	if fn.Name == "agentworkerRequestScope" || fn.Name == "requestScope" || fn.Name == "requestUserFeedback" {
		return false
	}
	if fn.FunctionKind != "query" && fn.FunctionKind != "mutation" && fn.FunctionKind != "logic" && fn.FunctionKind != "builtin" {
		return false
	}
	for _, field := range workCapabilityFields(fn) {
		if field.Secret {
			return false
		}
	}
	if e.refuseBelowRequiredRank(ctx, fn, fn.Name) != nil || e.refuseBelowRequiredCapability(ctx, fn, fn.Name) != nil {
		return false
	}
	if app := workCapabilityApp(fn); app != "" {
		subject, ok := auth.SubjectFromContext(ctx)
		if !ok || !auth.CapableFor(ctx, subject, "read", "app:"+app) {
			return false
		}
	}
	return true
}

func workCapabilityApp(fn *Function) string {
	if fn.Name == "workSearchConversations" || fn.Name == "workRecallMemory" || fn.Name == "workViewerContext" || fn.Name == "askConversationById" {
		return "ask"
	}
	if fn.Name == "workNavigate" {
		return ""
	} // The handler records the requested app, not Nexus.
	ns := ConstructNamespaceForOrigin(fn.Origin)
	switch ns {
	case "identity":
		if strings.HasPrefix(fn.Name, "my") || strings.HasPrefix(fn.Name, "own") {
			return "identity"
		}
		return "users"
	case "groups", "access":
		return "users"
	case "worker", "providers", "policies", "rules", "router":
		return "fleet"
	case "library":
		return "files"
	case "accounts":
		return "accounts"
	case "campaigns":
		return "campaigns"
	case "platform":
		return "deployables"
	case "agents", "node", "cluster", "automations":
		return "cluster"
	case "work", "goals":
		return "nexus"
	case "training", "knowledge":
		return "training"
	}
	return ""
}

func (e *MemQLEngine) workCapabilitiesBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("sign in to discover capabilities")
	}
	search := strings.ToLower(strings.TrimSpace(stringArg(args, "search")))
	if len(search) > 300 {
		return nil, fmt.Errorf("provide a short capability search")
	}
	matches := []map[string]any{}
	scores := map[string]int{}
	concepts := map[string][]string{}
	for _, fn := range e.functions.List() {
		if !e.workCapabilityAllowed(ctx, fn) {
			continue
		}
		name := QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name)
		concept := fn.BoundConcept
		if fn.Name == "workSearchConversations" {
			concept = "v1:os:askConversation"
		}
		if search == "" {
			if concept != "" && (fn.FunctionKind == "query" || fn.Name == "workSearchConversations") {
				concepts[concept] = append(concepts[concept], name)
			}
			continue
		}
		haystack := strings.ToLower(name + " " + fn.Description + " " + fn.DocComment + " " + fn.BoundConcept)
		if fn.Name == "workNavigate" {
			haystack += " " + string(osNavigationJSON)
		}
		for _, field := range workCapabilityFields(fn) {
			haystack += " " + strings.ToLower(field.Name+" "+field.Description+" "+fmt.Sprint(field.Enum))
		}
		score := workCapabilityScore(search, name, haystack)
		if score == 0 {
			continue
		}
		scores[name] = score
		fields := []map[string]any{}
		for _, field := range workCapabilityFields(fn) {
			fields = append(fields, map[string]any{"name": field.Name, "type": field.Type, "optional": field.Optional, "description": field.Description, "enum": field.Enum})
		}
		description := fn.Description
		if description == "" {
			description = fn.DocComment
		}
		matches = append(matches, map[string]any{"name": name, "kind": fn.FunctionKind, "description": description, "arguments": fields, "app": workCapabilityApp(fn), "concept": concept})
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i]["name"].(string), matches[j]["name"].(string)
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		return a < b
	})
	total := len(matches)
	if len(matches) > 30 {
		matches = matches[:30]
	}
	catalog := []map[string]any{}
	for concept, reads := range concepts {
		sort.Strings(reads)
		count := len(reads)
		if len(reads) > 3 {
			reads = reads[:3]
		}
		catalog = append(catalog, map[string]any{"concept": concept, "reads": reads, "readCount": count})
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i]["concept"].(string) < catalog[j]["concept"].(string) })
	raw, _ := json.Marshal(map[string]any{"capabilities": matches, "concepts": catalog, "total": total, "truncated": total > len(matches), "note": "Read contracts describe available data, not proof that rows exist. Search a returned read name to inspect its arguments. Use common.similarTo for semantic search where indexed; empty semantic results do not prove absence."})
	return []memorynodes.MemoryNode{{ID: "capabilities", Payload: raw}}, nil
}

func (e *MemQLEngine) workExecuteBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	name := stringArg(args, "name")
	fn, err := e.functions.Get(name)
	if err != nil || !e.workCapabilityAllowed(ctx, fn) {
		return nil, fmt.Errorf("capability is unavailable for this person")
	}
	arguments, ok := args["arguments"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be an object")
	}
	// Fleet identity is execution context, never an argument a model may
	// invent. Preserve the worker's standing grant, policy and safety gates.
	if strings.HasPrefix(fn.Name, "agentworker") {
		run, ok := common.RunFromContext(ctx)
		ac, _ := auth.AccessFromContext(ctx)
		if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || ActingAgentIdFromContext(ctx) == "" {
			return nil, fmt.Errorf("Fleet execution requires an owned work run and acting agent")
		}
		copy := make(map[string]any, len(arguments))
		for key, value := range arguments {
			copy[key] = value
		}
		arguments = copy
		for _, field := range workCapabilityFields(fn) {
			switch field.Name {
			case "agentId":
				arguments[field.Name] = ActingAgentIdFromContext(ctx)
			case "ownerUserId":
				arguments[field.Name] = run.OwnerUserId
			case "runId":
				arguments[field.Name] = run.RunId
			case "stepId":
				arguments[field.Name] = run.StepKey
			}
		}
	}
	call, err := parser.RenderCall(QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name), arguments)
	if err != nil {
		return nil, err
	}
	// Runtime calls accept qualified names; `use` belongs to declarations,
	// not executable expressions. Keep the namespace to avoid ambiguous names.
	call = fn.FunctionKind + " " + call
	event := WorkEvent{ID: id.NewShortId(), Kind: "action", Phase: "running", Name: name, App: workCapabilityApp(fn), Navigate: workCapabilityApp(fn) != "" && fn.FunctionKind == "mutation"}
	// Only resource identifiers are navigation hints. Never mirror arbitrary
	// content or credentials into desktop events.
	event.Arguments = map[string]any{}
	for key, value := range arguments {
		if strings.HasSuffix(key, "Id") {
			if v, ok := value.(string); ok {
				event.Arguments[key] = BareShortId(v)
			}
		}
	}
	if err := e.RecordWorkProgress(ctx, event); err != nil {
		return nil, fmt.Errorf("could not record action before execution: %w", err)
	}
	result, err := e.Execute(ctx, call)
	if err == nil && strings.HasPrefix(fn.Name, "agentworkerDispatch") {
		for _, row := range MaterializeRows(result.OutputPayload()) {
			if ok, present := row["ok"].(bool); present && !ok {
				err = fmt.Errorf("Fleet operation failed: %v (%v)", row["errorMessage"], row["errorCode"])
				break
			}
		}
	}
	event.Phase = "completed"
	if err != nil {
		event.Phase = "failed"
		event.Error = err.Error()
	}
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if recordErr := e.RecordWorkProgress(receiptCtx, event); recordErr != nil {
		return nil, fmt.Errorf("action result could not be recorded; check current state before retrying: %w", recordErr)
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result.OutputPayload())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "result", Payload: raw}}, nil
}

// Builtins declare their contract on the body, other constructs in args {}.
// Keep both paths visible so discovery never advertises an empty contract and
// Fleet identity cannot become a model-supplied field on the builtin path.
func workCapabilityFields(fn *Function) []*FunctionArgsField {
	if fn.ArgsSchema != nil {
		return fn.ArgsSchema.Fields
	}
	if fn.BuiltinArgs != nil {
		return fn.BuiltinArgs.Fields
	}
	return nil
}

// Natural-language discovery ranks partial term matches. Requiring every word
// made extra descriptors turn a relevant capability into an empty result.
func workCapabilityScore(search, name, description string) int {
	search = strings.ToLower(search)
	name = strings.ToLower(name)
	if search == name || strings.HasSuffix(name, "."+search) {
		return 10000
	}
	score, matched := 0, 0
	for _, word := range strings.Fields(search) {
		if strings.Contains(description, word) {
			matched++
			score++
			if strings.Contains(name, word) {
				score += 5
			}
		}
	}
	if matched == len(strings.Fields(search)) {
		score += 20
	}
	return score
}
