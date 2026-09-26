package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func (e *MemQLEngine) workCapabilityAllowed(ctx context.Context, fn *Function) bool {
	if fn == nil || !fn.Enabled || fn.ServerOnly || strings.HasPrefix(fn.Name, "ask") || fn.Name == "workCapabilities" || fn.Name == "workExecute" || fn.Name == "createGoal" || fn.Name == "runAgentTurn" || strings.HasSuffix(fn.Name, "AskConversation") {
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
	if search == "" || len(search) > 300 {
		return nil, fmt.Errorf("provide a short capability search")
	}
	var matches []map[string]any
	for _, fn := range e.functions.List() {
		if !e.workCapabilityAllowed(ctx, fn) {
			continue
		}
		name := QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name)
		haystack := strings.ToLower(name + " " + fn.Description + " " + fn.DocComment + " " + fn.BoundConcept)
		for _, field := range workCapabilityFields(fn) {
			haystack += " " + strings.ToLower(field.Name+" "+field.Description+" "+fmt.Sprint(field.Enum))
		}
		found := true
		for _, word := range strings.Fields(search) {
			if !strings.Contains(haystack, word) {
				found = false
				break
			}
		}
		if !found {
			continue
		}
		fields := []map[string]any{}
		for _, field := range workCapabilityFields(fn) {
			fields = append(fields, map[string]any{"name": field.Name, "type": field.Type, "optional": field.Optional, "description": field.Description, "enum": field.Enum})
		}
		matches = append(matches, map[string]any{"name": name, "kind": fn.FunctionKind, "description": fn.Description, "arguments": fields, "app": workCapabilityApp(fn)})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i]["name"].(string) < matches[j]["name"].(string) })
	if len(matches) > 30 {
		matches = matches[:30]
	}
	raw, _ := json.Marshal(map[string]any{"capabilities": matches})
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
	event := WorkEvent{ID: id.NewShortId(), Kind: "action", Phase: "running", Name: name, App: workCapabilityApp(fn), Navigate: workCapabilityApp(fn) != ""}
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
	if recordErr := e.RecordWorkProgress(ctx, event); recordErr != nil {
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

// Opening a view requests presentation only; it never replays a data mutation.
func (e *MemQLEngine) workNavigateBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	app := stringArg(args, "app")
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, "read", "app:"+app) {
		return nil, fmt.Errorf("this app is unavailable for this person")
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId == "" {
		return nil, fmt.Errorf("navigation requires a work run")
	}
	event := WorkEvent{ID: id.NewShortId(), Kind: "action", Phase: "completed", Name: "Open " + app, App: app, Navigate: true, Arguments: map[string]any{}}
	if section := stringArg(args, "section"); section != "" {
		event.Arguments["section"] = section
	}
	if err := e.RecordWorkProgress(ctx, event); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"requested": true, "app": app, "note": "Navigation was requested in the connected OS. This is not a data change or a receipt that a browser displayed it."})
	return []memorynodes.MemoryNode{{ID: event.ID, Payload: raw}}, nil
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
