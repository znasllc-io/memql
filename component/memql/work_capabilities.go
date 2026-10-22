package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/kljensen/snowball/english"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func (e *MemQLEngine) workCapabilityAllowed(ctx context.Context, fn *Function) bool {
	if fn == nil || !fn.Enabled || fn.ServerOnly || (strings.HasPrefix(fn.Name, "ask") && fn.Name != "askConversationById") || fn.Name == "workAcknowledgementCache" || fn.Name == "indexConversationMemory" || fn.Name == "workCapabilities" || fn.Name == "workExecute" || fn.Name == "createGoal" || fn.Name == "workRetitleRun" || fn.Name == "runAgentTurn" || strings.HasSuffix(fn.Name, "AskConversation") {
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
	// Discovery cannot advertise unrestricted mutation/logic authority to a
	// Viewer. Organization mutations retain their target-specific executor
	// checks; personal mutations retain their owner checks.
	if (fn.FunctionKind == "mutation" || fn.FunctionKind == "logic") && !e.GlobalDataCapable(ctx, auth.VerbCreate) {
		organizationWrite := fn.FunctionKind == "mutation" && fn.MutationTemplate != nil && organizationDataWriteConcept(fn.MutationTemplate.Concept)
		if !organizationWrite && !e.PersonalAppCapable(ctx, personalMutationApp(fn)) {
			return false
		}
	}
	for _, field := range workCapabilityFields(fn) {
		if workCapabilitySecret(field) {
			return false
		}
	}
	if e.refuseBelowRequiredRank(ctx, fn, fn.Name) != nil || e.refuseBelowRequiredCapability(ctx, fn, fn.Name) != nil {
		return false
	}
	if app := workCapabilityApp(fn); app != "" {
		if app == "files" || app == "ask" {
			if !e.OrganizationCapable(ctx, "", auth.VerbRead, "app:"+app) {
				return false
			}
		} else {
			subject, ok := auth.SubjectFromContext(ctx)
			if !ok || !auth.CapableFor(ctx, subject, auth.VerbRead, "app:"+app) {
				return false
			}
		}
	}
	return true
}

func workCapabilitySecret(field *FunctionArgsField) bool {
	if field == nil {
		return false
	}
	if field.Secret || workCapabilitySecret(field.Items) {
		return true
	}
	for _, nested := range field.Nested {
		if workCapabilitySecret(nested) {
			return true
		}
	}
	return false
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
		return "email"
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
		purpose := fn.Description
		if purpose == "" {
			purpose = fn.DocComment
		}
		score := workCapabilityScore(search, name, haystack, purpose)
		if score == 0 {
			continue
		}
		scores[name] = score
		fields := []map[string]any{}
		for _, field := range workCapabilityFields(fn) {
			contract := jsonSchemaForArgsField(field)
			contract["name"], contract["optional"] = field.Name, field.Optional
			fields = append(fields, contract)
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
		if (matches[i]["kind"] == "query") != (matches[j]["kind"] == "query") {
			return matches[i]["kind"] == "query"
		}
		return a < b
	})
	total := len(matches)
	// Discovery is a shortlist, not the entire tool registry in each model
	// turn. Include complete small argument contracts so a useful discovery
	// can be executed immediately. Large contracts still need an exact lookup;
	// never silently omit required fields or enum alternatives to make one fit.
	if len(matches) > 6 {
		matches = matches[:6]
	}
	for _, match := range matches {
		if !strings.EqualFold(search, fmt.Sprint(match["name"])) {
			contract, _ := json.Marshal(match["arguments"])
			if len(contract) > 2048 {
				delete(match, "arguments")
				match["argumentsOmitted"] = true
			}
			match["description"] = boundedMemoryText(fmt.Sprint(match["description"]), 500)
		}
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
	if err := e.RecordWorkProgress(ctx, WorkEvent{Kind: "action", Phase: "completed", Name: "Discovered capability contracts", Arguments: map[string]any{"search": search, "matchedCapabilityCount": total, "returnedContracts": len(matches)}}); err != nil {
		return nil, err
	}
	fleet := e.workFleetCapabilities(ctx)
	if err := e.recordWorkCapabilityEvidence(ctx, search, matches, fleet, total); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"capabilities": matches, "fleet": fleet, "concepts": catalog, "matchedCapabilityCount": total, "truncated": total > len(matches), "note": "Counts here describe capability definitions, never stored records. Read contracts describe available data, not proof that rows exist. Returned arguments are complete and ready to use; an empty list means no arguments. Only when argumentsOmitted is true, search that qualified name exactly for its full contract. Use recallMemory for prior conversations and preferences. Empty semantic results do not prove absence."})
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
	// Execution identity is context, never an argument a model may
	// invent. Preserve the worker's standing grant, policy and safety gates.
	if strings.HasPrefix(fn.Name, "agentworker") || fn.Name == "workbenchDispatchHost" {
		run, ok := common.RunFromContext(ctx)
		ac, _ := auth.AccessFromContext(ctx)
		if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || ActingAgentIdFromContext(ctx) == "" {
			return nil, fmt.Errorf("Host execution requires an owned work run and acting agent")
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
	// Apply the direct request's data boundary before any work receipt/effect.
	// Ask keeps the person's authority on this replica, never a system actor.
	if !e.OrganizationDataPlaneEnforced(ctx, call) {
		if !e.GlobalDataCapable(ctx, auth.VerbRead) {
			return nil, fmt.Errorf("permission denied: this action requires data access")
		}
		if e.DataVerbFor(ctx, call) != auth.VerbRead && !e.GlobalDataCapable(ctx, auth.VerbCreate) && !e.PersonalDataPlaneEnforced(ctx, call) {
			return nil, fmt.Errorf("permission denied: this action requires data write access")
		}
	}
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
func workCapabilityScore(search, name, description string, purpose ...string) int {
	lowerSearch, lowerName := strings.ToLower(search), strings.ToLower(name)
	if lowerSearch == lowerName || strings.HasSuffix(lowerName, "."+lowerSearch) {
		return 10000
	}
	if strings.Contains(search, ".") && strings.IndexFunc(search, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' }) < 0 {
		return 0 // A qualified operation lookup is exact, including an absent one.
	}
	words := workCapabilityWords(search)
	for _, word := range workCapabilityTokens("a an the and or to for of in on at by from is are was were be been with without can could would should do does did have has had i me my we our you your it its this that these those what which who how when where") {
		delete(words, word)
	}
	if len(words) == 0 {
		return 0
	}
	intent := ""
	for _, word := range workCapabilityTokens(search) {
		if words[word] {
			intent = word
			break
		}
	}
	names, body := workCapabilityWords(name), workCapabilityWords(description)
	lead := map[string]bool{}
	directPurpose := false
	if len(purpose) > 0 {
		// The operation's stated purpose outranks incidental mentions in its
		// implementation history or argument descriptions. No app-specific verbs.
		first := strings.FieldsFunc(purpose[0], func(r rune) bool { return r == '.' || r == '\n' })
		if len(first) > 0 {
			lead = workCapabilityWords(first[0])
			firstWords := workCapabilityTokens(first[0])
			directPurpose = len(firstWords) > 0 && firstWords[0] == intent
		}
	}
	score, matched, purposeMatches := 0, 0, 0
	for word := range words {
		if body[word] || names[word] || lead[word] {
			matched++
			score++
			if names[word] {
				score += 5
			}
			if lead[word] {
				purposeMatches++
			}
		}
	}
	// Coherent matches in the purpose matter more than several unrelated
	// words scattered through implementation notes or generic field names.
	score += 20 * purposeMatches * purposeMatches
	// A purpose beginning with the requested action is more direct than a
	// list whose prose happens to mention that action or ordering words.
	if directPurpose {
		score += 40
	}
	if matched == len(words) {
		score += 20
	}
	return score
}

func workCapabilityWords(text string) map[string]bool {
	result := map[string]bool{}
	for _, word := range workCapabilityTokens(text) {
		result[word] = true
	}
	return result
}

func workCapabilityTokens(text string) []string {
	// Normalize both the search and the declared English contracts with the
	// same word stemmer: generation/generate and archived/archive must match
	// across every domain, without per-operation aliases or substring matches.
	// Qualified names bypass this normalization in workCapabilityScore.
	runes := []rune(text)
	var separated strings.Builder
	for n, r := range runes {
		if n > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[n-1]) || (n+1 < len(runes) && unicode.IsLower(runes[n+1]))) {
			separated.WriteByte(' ')
		}
		separated.WriteRune(unicode.ToLower(r))
	}
	result := []string{}
	for _, word := range strings.FieldsFunc(separated.String(), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		result = append(result, english.Stem(word, false))
	}
	return result
}
