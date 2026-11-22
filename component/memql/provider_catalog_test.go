package memql

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// provider_catalog_test.go -- the shipped vendor catalog (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// R3). Its numbers and model ids were read from the two vendors' own
// documentation on 2026-11-22 and are pinned nowhere here: what is pinned is
// the SHAPE a refresh must keep.

// shippedCatalog loads the real tree's provider records without building a
// client or resolving a credential.
func shippedCatalog(t *testing.T) *ProviderRegistry {
	t.Helper()
	reg := newProviderRegistry()
	report := newLoadReport()
	if _, err := loadUnifiedProvidersFromTree(memqldsl.Tree(), false, nil, reg, report); err != nil {
		t.Fatalf("load: %v", err)
	}
	if report.HasProblems() {
		t.Fatalf("the shipped provider records do not load cleanly: %s", report.Detail())
	}
	return reg
}

// namesKeptForBundles are the shipped vendor text records that declare NO job,
// on purpose. Each is a second name for a model another record already offers
// to the selectors, kept so a product bundle that pins the name keeps
// resolving, or a record the engine cannot call. A selector that offered the
// same model under two names would retry a failing model.
var namesKeptForBundles = map[string]string{
	"chat53Latest":   "its model was shut down by the vendor; the name now calls the vendor's stated replacement, which chat56Sol offers",
	"codex51Max":     "its model was shut down by the vendor; the name now calls the vendor's stated replacement, which chat56Sol offers",
	"reasoning4Mini": "its model was shut down by the vendor; the name now calls the vendor's stated replacement, which chat56Terra offers",
	"chat54Pro":      "a second name for gpt-5.4, which chat54 offers",
	"codex53":        "its model is served by an API this engine does not speak, so it cannot be called",
}

func vendorTextRecords(reg *ProviderRegistry) []*ProviderConfigEntry {
	var out []*ProviderConfigEntry
	for _, name := range reg.Names() {
		if e, ok := reg.Entry(name); ok && e != nil && isVendorTextRecord(e.Config) {
			out = append(out, e)
		}
	}
	return out
}

func TestEveryShippedVendorTextRecordDeclaresAJob(t *testing.T) {
	reg := shippedCatalog(t)
	records := vendorTextRecords(reg)
	if len(records) < 10 {
		t.Fatalf("read %d vendor text records from the shipped tree; the catalog moved or this is not measuring it", len(records))
	}
	kept := map[string]bool{}
	for _, e := range records {
		cfg := e.Config
		if _, bundleName := namesKeptForBundles[cfg.Name]; bundleName {
			kept[cfg.Name] = true
			if len(cfg.Levels()) != 0 || cfg.Rank() != 0 {
				t.Errorf("%s is kept for bundles and declares a job: a selector would offer its model under two names", cfg.Name)
			}
			continue
		}
		if len(cfg.Levels()) == 0 {
			t.Errorf("%s declares no level, so no selector will ever choose it", cfg.Name)
		}
		if cfg.Rank() == 0 {
			t.Errorf("%s declares no rank", cfg.Name)
		}
		if cfg.ContextWindow() <= 0 {
			t.Errorf("%s declares no contextWindow, so it cannot be shown to hold any call's floor", cfg.Name)
		}
		if _, ok := intParam(cfg.Params["maxTokens"]); !ok {
			if _, ok := intParam(cfg.Params["maxCompletionTokens"]); !ok {
				t.Errorf("%s declares no output limit", cfg.Name)
			}
		}
		if p := cfg.Pricing(); p.InputPerMillion <= 0 || p.OutputPerMillion <= 0 {
			t.Errorf("%s declares no price, so its cost reads as unknown and it sorts last under cheapest", cfg.Name)
		}
	}
	for name := range namesKeptForBundles {
		if !kept[name] {
			t.Errorf("%s is listed as kept for bundles and is not a shipped vendor text record", name)
		}
	}
}

func TestEveryLevelHasARecordAtBothVendors(t *testing.T) {
	reg := shippedCatalog(t)
	for _, level := range []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning} {
		for _, selector := range []string{VendorSelectorStrongest, VendorSelectorCheapest} {
			ordered, _ := reg.VendorRecordsFor(level, selector)
			vendors := map[string]bool{}
			for _, name := range ordered {
				e, _ := reg.Entry(name)
				vendors[vendorOfType(e.Config.Type)] = true
			}
			if !vendors[VendorAnthropic] || !vendors[VendorOpenAI] {
				t.Errorf("%s at %s offers %v: a cluster with one vendor set up would have no model for the job", selector, level, ordered)
			}
		}
	}
}

// Two records that serve the same level never share a rank: the order a
// selector answers in is then somebody's decision at every step, not the
// alphabet's.
func TestNoTwoRecordsOfALevelShareARank(t *testing.T) {
	reg := shippedCatalog(t)
	for _, level := range []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning} {
		ordered, _ := reg.VendorRecordsFor(level, VendorSelectorStrongest)
		byRank := map[int]string{}
		for _, name := range ordered {
			e, _ := reg.Entry(name)
			if other, taken := byRank[e.Config.Rank()]; taken {
				t.Errorf("%s and %s both rank %d at %s", other, name, e.Config.Rank(), level)
			}
			byRank[e.Config.Rank()] = name
		}
	}
}

// A product bundle pins record names. Every name the catalog shipped before
// the 2026-11-22 refresh still resolves; a name is repointed or kept, never
// removed.
func TestEveryRecordNameShippedBeforeTheRefreshStillResolves(t *testing.T) {
	reg := shippedCatalog(t)
	for _, name := range []string{
		"anthropic", "openai", "fleet", "app",
		"chat53Latest", "chat54", "chat54Mini", "chat54Nano", "chat54Pro",
		"claudeHaiku", "claudeOpus", "claudeSonnet", "reasoningClaudeOpus",
		"codex51Max", "codex53", "reasoning4Mini",
		"embedding3Large", "embedding3Small", "askTranscription", "askSpeech",
	} {
		if _, ok := reg.Entry(name); !ok {
			t.Errorf("%s no longer resolves: a bundle that pins it now fails", name)
		}
	}
}

// The model ids OpenAI's deprecations page listed as shut down on 2026-11-22.
// A record that still names one fails every call it is given.
func TestNoShippedRecordCallsAModelItsVendorShutDown(t *testing.T) {
	shutDown := map[string]bool{"gpt-5.3-chat-latest": true, "gpt-5.1-codex-max": true, "o4-mini": true}
	reg := shippedCatalog(t)
	for _, e := range vendorTextRecords(reg) {
		if shutDown[strings.TrimSpace(e.Config.Model)] {
			t.Errorf("%s still calls %s, which its vendor shut down", e.Config.Name, e.Config.Model)
		}
	}
}

// ---- what a current Claude model is sent (the claude-api skill and the
// vendor's model pages, read 2026-11-22; not run against the vendor) ----

// The current models refuse three things an older record sends. A record for
// one declares none of them, and asks for structured output by format.
func TestTheCurrentClaudeRecordsDeclareOnlyWhatTheirModelsAccept(t *testing.T) {
	reg := shippedCatalog(t)
	current := []string{"claudeOpus55", "claudeSonnet55", "claudeHaiku55"}
	for _, name := range current {
		e, ok := reg.Entry(name)
		if !ok {
			t.Errorf("%s is not in the catalog", name)
			continue
		}
		for _, refused := range []string{"temperature", "topP", "topK", "thinkingBudgetTokens"} {
			if _, declared := e.Config.Params[refused]; declared {
				t.Errorf("%s declares %s, which its model answers with a 400", name, refused)
			}
		}
		if got := anthropicStructuredMode(e.Config.Params); got != anthropicStructuredByFormat {
			t.Errorf("%s asks for structured output by %q: its model refuses a forced tool choice", name, got)
		}
	}
	// And the records for the models that still accept a forced tool choice
	// keep the request they always sent.
	for _, name := range []string{"claudeOpus", "claudeSonnet", "claudeHaiku", "reasoningClaudeOpus"} {
		e, ok := reg.Entry(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if got := anthropicStructuredMode(e.Config.Params); got != anthropicStructuredByTool {
			t.Errorf("%s asks for structured output by %q, want the forced tool it always used", name, got)
		}
	}
}

func TestAStructuredRequestByFormatCarriesTheSchemaAndForcesNoTool(t *testing.T) {
	params, err := anthropicStructuredParams(
		"claude-opus-5-5",
		map[string]any{"maxTokens": 20000, "structuredOutput": "format"},
		[]common.ChatMessage{{Role: "system", Content: "decide the specialist"}},
		factorySchema(),
	)
	if err != nil {
		t.Fatalf("assembly failed: %v", err)
	}
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	// What the model refuses, and what an old record sends.
	for _, refused := range []string{"tool_choice", "tools", "temperature", "top_p", "thinking"} {
		if _, present := sent[refused]; present {
			t.Errorf("the request carries %s: %s", refused, body)
		}
	}
	format, _ := sent["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config.format.type = %v, want json_schema: %s", format["type"], body)
	}
	schema, _ := format["schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("the schema did not ride the format whole: %v", schema)
	}
	if _, ok := schema["properties"].(map[string]any)["action"]; !ok {
		t.Fatalf("the schema's properties are missing: %v", schema)
	}
	if sent["max_tokens"] != float64(20000) || len(params.Messages) == 0 || len(params.System) == 0 {
		t.Fatalf("the conversation or the output limit did not survive: %s", body)
	}
}

// The vendor's structured outputs accept a subset of JSON Schema: every object
// closed, and no numeric, length or array-size constraints. A schema written
// for the forced-tool path is made to fit, and what was removed is kept in the
// field's description so the model still reads it.
func TestASchemaIsMadeToFitTheFormatTheVendorAccepts(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"score": {"type": "integer", "minimum": 1, "maximum": 5, "description": "How sure."},
			"title": {"type": "string", "minLength": 1, "maxLength": 80},
			"tags": {"type": "array", "maxItems": 3, "items": {"type": "object", "properties": {"name": {"type": "string"}}}},
			"kind": {"type": "string", "enum": ["a", "b"]}
		},
		"required": ["score"]
	}`)
	fitted, err := anthropicFormatSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fitted["additionalProperties"] != false {
		t.Errorf("the top object is not closed: %v", fitted)
	}
	props := fitted["properties"].(map[string]any)
	score := props["score"].(map[string]any)
	for _, gone := range []string{"minimum", "maximum"} {
		if _, kept := score[gone]; kept {
			t.Errorf("score keeps %s, which the vendor refuses", gone)
		}
	}
	if desc, _ := score["description"].(string); !strings.Contains(desc, "How sure.") || !strings.Contains(desc, "minimum 1") || !strings.Contains(desc, "maximum 5") {
		t.Errorf("score's description lost its words or the limits removed from it: %q", desc)
	}
	title := props["title"].(map[string]any)
	if _, kept := title["maxLength"]; kept {
		t.Error("title keeps maxLength")
	}
	if desc, _ := title["description"].(string); !strings.Contains(desc, "maxLength 80") {
		t.Errorf("title's description does not carry the limit removed from it: %q", desc)
	}
	tags := props["tags"].(map[string]any)
	if _, kept := tags["maxItems"]; kept {
		t.Error("tags keeps maxItems")
	}
	if item := tags["items"].(map[string]any); item["additionalProperties"] != false {
		t.Errorf("an object inside an array is not closed: %v", item)
	}
	if kind := props["kind"].(map[string]any); len(kind["enum"].([]any)) != 2 {
		t.Errorf("an enum was touched: %v", kind)
	}
	// The caller's schema is not edited in place.
	var original map[string]any
	_ = json.Unmarshal(raw, &original)
	if _, ok := original["properties"].(map[string]any)["score"].(map[string]any)["minimum"]; !ok {
		t.Error("the test's own copy lost a field, so nothing was measured")
	}
	// Deterministic: the same schema fits the same way, key for key.
	again, _ := anthropicFormatSchema(raw)
	a, _ := json.Marshal(fitted)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Error("the same schema was fitted two ways")
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "kind,score,tags,title" {
		t.Errorf("properties changed: %v", keys)
	}
}

// The vendor names what its format accepts, not what it refuses, so the fit is
// a list of what is KEPT: a keyword outside it moves into the description
// rather than riding a request that would come back as a 400.
func TestOnlyTheKeywordsTheFormatAcceptsRideTheSchema(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"slug": {"type": "string", "pattern": "^[a-z]+$", "default": "x"},
			"when": {"type": "string", "format": "date-time"},
			"phone": {"type": "string", "format": "phone"},
			"one": {"type": "array", "minItems": 1, "items": {"type": "string"}},
			"three": {"type": "array", "minItems": 3, "items": {"type": "string"}},
			"either": {"oneOf": [{"type": "string"}, {"type": "object", "properties": {"n": {"type": "integer", "minimum": 0}}}]},
			"ref": {"$ref": "#/$defs/thing"}
		},
		"$defs": {"thing": {"type": "object", "properties": {"size": {"type": "integer", "maximum": 9}}, "maxProperties": 4}}
	}`)
	fitted, err := anthropicFormatSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	props := fitted["properties"].(map[string]any)

	slug := props["slug"].(map[string]any)
	for _, gone := range []string{"pattern", "default"} {
		if _, kept := slug[gone]; kept {
			t.Errorf("slug keeps %s, which the format does not list as accepted", gone)
		}
	}
	if desc, _ := slug["description"].(string); !strings.Contains(desc, "pattern ^[a-z]+$") || !strings.Contains(desc, "default x") {
		t.Errorf("slug's description does not carry what was removed: %q", desc)
	}
	if when := props["when"].(map[string]any); when["format"] != "date-time" {
		t.Errorf("a format the vendor accepts was removed: %v", when)
	}
	phone := props["phone"].(map[string]any)
	if _, kept := phone["format"]; kept {
		t.Errorf("a format the vendor does not accept rides the schema: %v", phone)
	}
	if desc, _ := phone["description"].(string); !strings.Contains(desc, "format phone") {
		t.Errorf("phone's description does not carry its format: %q", desc)
	}
	if one := props["one"].(map[string]any); one["minItems"] != float64(1) {
		t.Errorf("minItems 1 is accepted and was removed: %v", one)
	}
	three := props["three"].(map[string]any)
	if _, kept := three["minItems"]; kept {
		t.Errorf("minItems above 1 rides the schema: %v", three)
	}
	either := props["either"].(map[string]any)
	if _, kept := either["oneOf"]; kept {
		t.Errorf("oneOf rides the schema; the format accepts anyOf: %v", either)
	}
	branches, _ := either["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("oneOf was not carried as anyOf: %v", either)
	}
	inner := branches[1].(map[string]any)
	if inner["additionalProperties"] != false {
		t.Errorf("an object inside a branch is not closed: %v", inner)
	}
	if n := inner["properties"].(map[string]any)["n"].(map[string]any); n["minimum"] != nil {
		t.Errorf("a limit inside a branch rides the schema: %v", n)
	}
	if ref := props["ref"].(map[string]any); ref["$ref"] != "#/$defs/thing" {
		t.Errorf("a reference was touched: %v", ref)
	}
	thing := fitted["$defs"].(map[string]any)["thing"].(map[string]any)
	if thing["additionalProperties"] != false {
		t.Errorf("a definition's object is not closed: %v", thing)
	}
	if _, kept := thing["maxProperties"]; kept {
		t.Errorf("maxProperties rides the schema: %v", thing)
	}
	if size := thing["properties"].(map[string]any)["size"].(map[string]any); size["maximum"] != nil {
		t.Errorf("a limit inside a definition rides the schema: %v", size)
	}
}

// The vendor's SDK refuses a request it expects to run past ten minutes unless
// the request streams, and it decides that from the output limit alone. The
// engine's structured, tool and plain chat calls do not stream, so a record
// whose limit crosses that line would fail every one of them before anything
// was sent.
func TestNoClaudeRecordDeclaresAnOutputLimitTheSdkRefusesWithoutStreaming(t *testing.T) {
	reg := shippedCatalog(t)
	checked := 0
	for _, e := range vendorTextRecords(reg) {
		if vendorOfType(e.Config.Type) != VendorAnthropic {
			continue
		}
		limit, ok := intParam(e.Config.Params["maxTokens"])
		if !ok {
			limit, ok = intParam(e.Config.Params["maxCompletionTokens"])
		}
		if !ok {
			continue
		}
		checked++
		if _, err := anthropic.CalculateNonStreamingTimeout(limit, anthropic.Model(e.Config.Model), nil); err != nil {
			t.Errorf("%s declares an output limit of %d: %v", e.Config.Name, limit, err)
		}
	}
	if checked < 4 {
		t.Fatalf("checked %d Claude records; the catalog moved or this is measuring nothing", checked)
	}
}

func TestAStructuredReplyByFormatIsTheTextTheModelWrote(t *testing.T) {
	if got, ok := anthropicFormatReply([]string{"", `  {"action":"match","reasoning":"same"}  `}); !ok || got != `{"action":"match","reasoning":"same"}` {
		t.Fatalf("reply = %q, %v", got, ok)
	}
	if _, ok := anthropicFormatReply([]string{"", "  "}); ok {
		t.Fatal("an empty reply was taken for a structured answer")
	}
}

// The whole chain for a current Claude record: the record as shipped, the real
// provider, and what reaches the vendor on each of the engine's three paths.
// The stand-in answers; nothing here reaches Anthropic.
func TestACurrentClaudeRecordSendsOnlyWhatItsModelAccepts(t *testing.T) {
	reg := shippedCatalog(t)
	for _, name := range []string{"claudeOpus55", "claudeSonnet55", "claudeHaiku55"} {
		t.Run(name, func(t *testing.T) {
			entry, ok := reg.Entry(name)
			if !ok {
				t.Fatalf("%s is not in the catalog", name)
			}
			v := newAccountVendor(t, VendorAnthropic)
			cfg := entry.Config
			cfg.Auth = v.config(name, true).Auth
			provider, err := newAIProvider(cfg)
			if err != nil {
				t.Fatalf("build the provider from the shipped record: %v", err)
			}
			keys := newFakeVendorKeys()
			keys.secrets["k1"] = standInKey
			client, ok := provider.(VendorAccountClient)
			if !ok {
				t.Fatalf("%T cannot be paid by an account", provider)
			}
			bound := client.WithVendorAccount(keyAccount(VendorAnthropic, "k1", "ana"), keys)
			ctx := context.Background()

			if _, err := bound.(common.ChatAIProvider).CallChat(ctx, hello()); err != nil {
				t.Fatalf("chat: %v", err)
			}
			tool := common.ToolDefinition{Name: "lookup", Description: "Look a thing up.", InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
			}}
			if _, err := bound.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx, hello(), []common.ToolDefinition{tool}); err != nil {
				t.Fatalf("chat with tools: %v", err)
			}
			v.answerNext(accountVendorAnswer{Status: http.StatusOK, Body: `{"id":"msg_2","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"text","text":"{\"action\":\"match\",\"reasoning\":\"same\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`})
			reply, err := bound.(common.ChatStructuredProvider).CallChatStructured(ctx, hello(), factorySchema())
			if err != nil {
				t.Fatalf("structured: %v", err)
			}
			if reply != `{"action":"match","reasoning":"same"}` {
				t.Errorf("the structured reply = %q, want the JSON the model wrote", reply)
			}

			requests := v.seen()
			if len(requests) != 3 {
				t.Fatalf("the vendor saw %d requests, want one per path", len(requests))
			}
			wantLimit, _ := intParam(cfg.Params["maxTokens"])
			bodies := make([]map[string]any, len(requests))
			for i, request := range requests {
				if err := json.Unmarshal(request.Body, &bodies[i]); err != nil {
					t.Fatalf("request %d did not carry JSON: %v", i, err)
				}
				sent := bodies[i]
				for _, refused := range []string{"temperature", "top_p", "top_k", "thinking"} {
					if _, present := sent[refused]; present {
						t.Errorf("request %d carries %s, which this model answers with a 400: %s", i, refused, request.Body)
					}
				}
				if sent["model"] != cfg.Model {
					t.Errorf("request %d names the model %v, want %s", i, sent["model"], cfg.Model)
				}
				if sent["max_tokens"] != float64(wantLimit) {
					t.Errorf("request %d carries max_tokens %v, want the record's %d", i, sent["max_tokens"], wantLimit)
				}
			}
			// With tools: the model may choose, and is never forced.
			if choice, _ := bodies[1]["tool_choice"].(map[string]any); choice["type"] != "auto" {
				t.Errorf("the tool call's tool_choice = %v, want auto: a forced choice is refused", bodies[1]["tool_choice"])
			}
			// Structured: by format, with no tool at all.
			for _, forced := range []string{"tools", "tool_choice"} {
				if _, present := bodies[2][forced]; present {
					t.Errorf("the structured request carries %s: %s", forced, requests[2].Body)
				}
			}
			format, _ := bodies[2]["output_config"].(map[string]any)["format"].(map[string]any)
			if format["type"] != "json_schema" {
				t.Errorf("the structured request's output_config.format = %v", format)
			}
		})
	}
}

// How a Claude record asks for structured output is one of two words, on a
// Claude text record. Anything else is refused when the record is loaded: a
// misspelled word would fall back to the forced tool, which is exactly what
// the models this param exists for refuse.
func TestARecordThatAsksForStructuredOutputAWayNobodyReadsIsRefused(t *testing.T) {
	tree := fstest.MapFS{
		"providers/memql.toml": &fstest.MapFile{Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"providers/providers.memql": &fstest.MapFile{Data: []byte(`
@base
@vendor("Anthropic")
provider anthropic {
  auth { }
}

@base
@vendor("OpenAI")
provider openai {
  auth { }
}

@extends("anthropic")
@model("claude-by-format")
provider claudeByFormat {
  params { structuredOutput "format" }
}

@extends("anthropic")
@model("claude-by-tool")
provider claudeByTool {
  params { structuredOutput "tool" }
}

@extends("anthropic")
@model("claude-misspelled")
provider claudeMisspelled {
  params { structuredOutput "fromat" }
}

@extends("openai")
@model("gpt-test")
provider gptWithTheParam {
  params { structuredOutput "format" }
}
`)},
	}
	reg := newProviderRegistry()
	report := newLoadReport()
	if _, err := loadUnifiedProvidersFromTree(tree, false, nil, reg, report); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, name := range []string{"claudeByFormat", "claudeByTool"} {
		if _, ok := reg.Entry(name); !ok {
			t.Errorf("%s declares a word the provider reads and was refused: %s", name, report.Detail())
		}
	}
	for _, name := range []string{"claudeMisspelled", "gptWithTheParam"} {
		if _, ok := reg.Entry(name); ok {
			t.Errorf("%s was registered", name)
		}
	}
	detail := report.Detail()
	for _, says := range []string{"claudeMisspelled", "gptWithTheParam", "structuredOutput", `"tool"`, `"format"`} {
		if !strings.Contains(detail, says) {
			t.Errorf("the report does not say %s: %s", says, detail)
		}
	}
}
