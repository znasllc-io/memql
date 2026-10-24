package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
)

// The suite, version 1.
//
// Three families, drawn from the design record (D3): structured-output
// validity over five schemas taken from the platform's own prompts,
// tool-call correctness over three tool definitions, and throughput plus
// time to first token at an 8K and a 32K prompt.
//
// WHY THE SCHEMAS ARE HAND-CHECKED RATHER THAN VALIDATED. Each case
// carries the JSON Schema it SENDS to the model and a Go function that
// checks the answer against that exact schema. No general JSON Schema
// validator is imported, and that is a decision rather than a shortcut:
// a draft-2020 validator is a dependency and a second thing to be wrong
// about, while a partial one that checked only `required` would score a
// model returning the right keys with the wrong types as valid -- which
// is precisely the failure the structured figure exists to catch. The
// schemas here are fixed and ours; checking them exactly costs a
// function each.

// The figure names. Stable keys: the engine files measurements under
// them, so renaming one orphans every figure already stored.
const (
	FigureStructuredValidity = "structured_validity"
	FigureToolCorrectness    = "tool_correctness"
	FigureThroughput8K       = "throughput_8k"
	FigureTTFT8K             = "ttft_8k"
	FigureThroughput32K      = "throughput_32k"
	FigureTTFT32K            = "ttft_32k"
)

// caseOrder puts the figures in the order the suite runs them, which is
// also the order they are read. Map iteration or a sort by name would
// put ttft before throughput and 32k before 8k, both of which read as
// arbitrary to somebody scanning a column.
func caseOrder(name string) int {
	for i, n := range []string{
		FigureStructuredValidity, FigureToolCorrectness,
		FigureThroughput8K, FigureTTFT8K, FigureThroughput32K, FigureTTFT32K,
	} {
		if n == name {
			return i
		}
	}
	return 99
}

// probeCase is one measurement.
type probeCase struct {
	// Name is the stable figure key.
	Name string
	// Label is how the case is named in a sentence ("8K throughput"),
	// used by the timeout reason and the progress line. Lower case: it
	// appears mid-sentence.
	Label string
	// Names is every figure this case produces, in order. A case may
	// produce more than one -- the throughput cases report a rate AND
	// the wait before the first token, which are two answers to two
	// questions and must not be averaged into one.
	Names []string
	Run   func(ctx context.Context, model string, c modelcall.Client, now func() time.Time) []Figure
}

func suite() []probeCase {
	return []probeCase{
		{
			Name: FigureStructuredValidity, Label: "structured output",
			Names: []string{FigureStructuredValidity}, Run: runStructured,
		},
		{
			Name: FigureToolCorrectness, Label: "tool call",
			Names: []string{FigureToolCorrectness}, Run: runTools,
		},
		{
			Name: FigureThroughput8K, Label: "8K throughput",
			Names: []string{FigureThroughput8K, FigureTTFT8K},
			Run:   throughputCase(8_000, FigureThroughput8K, FigureTTFT8K),
		},
		{
			Name: FigureThroughput32K, Label: "32K throughput",
			Names: []string{FigureThroughput32K, FigureTTFT32K},
			Run:   throughputCase(32_000, FigureThroughput32K, FigureTTFT32K),
		},
	}
}

// -----------------------------------------------------------------------------
// Structured output
// -----------------------------------------------------------------------------

// schemaCase is one of the five schemas, with the check written against
// that exact schema.
type schemaCase struct {
	name   string
	prompt string
	schema string
	check  func(map[string]any) error
}

// The five, drawn from the platform's own prompt surfaces: triage,
// intake, symptom, factory decision, healing patches.
func schemaCases() []schemaCase {
	return []schemaCase{
		{
			name:   "triage",
			prompt: "A worker's disk filled and three runs failed. Triage it.",
			schema: `{"type":"object","required":["severity","summary","owner"],"properties":{` +
				`"severity":{"type":"string","enum":["low","medium","high"]},` +
				`"summary":{"type":"string"},"owner":{"type":"string"}},"additionalProperties":false}`,
			check: func(m map[string]any) error {
				if err := requireEnum(m, "severity", "low", "medium", "high"); err != nil {
					return err
				}
				if err := requireString(m, "summary"); err != nil {
					return err
				}
				return requireString(m, "owner")
			},
		},
		{
			name:   "intake",
			prompt: "Extract the fields from: name Ada, email ada@example.com. Nothing else was given.",
			schema: `{"type":"object","required":["fields","complete"],"properties":{` +
				`"fields":{"type":"array","items":{"type":"object","required":["name","value"],"properties":{` +
				`"name":{"type":"string"},"value":{"type":"string"}}}},"complete":{"type":"boolean"}}}`,
			check: func(m map[string]any) error {
				items, err := requireArray(m, "fields")
				if err != nil {
					return err
				}
				for i, it := range items {
					obj, ok := it.(map[string]any)
					if !ok {
						return fmt.Errorf("fields[%d] is not an object", i)
					}
					if err := requireString(obj, "name"); err != nil {
						return fmt.Errorf("fields[%d]: %w", i, err)
					}
					if err := requireString(obj, "value"); err != nil {
						return fmt.Errorf("fields[%d]: %w", i, err)
					}
				}
				return requireBool(m, "complete")
			},
		},
		{
			name:   "symptom",
			prompt: "The service returns 504 after 30 seconds under load. List the symptoms.",
			schema: `{"type":"object","required":["symptoms","confidence"],"properties":{` +
				`"symptoms":{"type":"array","items":{"type":"string"}},` +
				`"confidence":{"type":"number","minimum":0,"maximum":1}}}`,
			check: func(m map[string]any) error {
				items, err := requireArray(m, "symptoms")
				if err != nil {
					return err
				}
				for i, it := range items {
					if _, ok := it.(string); !ok {
						return fmt.Errorf("symptoms[%d] is not a string", i)
					}
				}
				return requireUnitNumber(m, "confidence")
			},
		},
		{
			name:   "factory",
			prompt: "The build cache is three weeks stale and the last two builds failed. Decide.",
			schema: `{"type":"object","required":["decision","reason"],"properties":{` +
				`"decision":{"type":"string","enum":["rebuild","reuse","fail"]},"reason":{"type":"string"}}}`,
			check: func(m map[string]any) error {
				if err := requireEnum(m, "decision", "rebuild", "reuse", "fail"); err != nil {
					return err
				}
				return requireString(m, "reason")
			},
		},
		{
			name:   "healing",
			prompt: "A nil dereference at a.go line 12. Propose the patch.",
			schema: `{"type":"object","required":["patches"],"properties":{` +
				`"patches":{"type":"array","items":{"type":"object","required":["path","hunk"],"properties":{` +
				`"path":{"type":"string"},"hunk":{"type":"string"}}}}}}`,
			check: func(m map[string]any) error {
				items, err := requireArray(m, "patches")
				if err != nil {
					return err
				}
				for i, it := range items {
					obj, ok := it.(map[string]any)
					if !ok {
						return fmt.Errorf("patches[%d] is not an object", i)
					}
					if err := requireString(obj, "path"); err != nil {
						return fmt.Errorf("patches[%d]: %w", i, err)
					}
					if err := requireString(obj, "hunk"); err != nil {
						return fmt.Errorf("patches[%d]: %w", i, err)
					}
				}
				return nil
			},
		},
	}
}

// runStructured measures the fraction of schemas the model held to.
//
// A model that answers PROSE scores zero for that schema rather than
// going absent, and the distinction is the whole point: it answered, and
// what it answered was wrong. An absent figure would say the case could
// not be run, which would hide exactly the failure being measured.
func runStructured(ctx context.Context, model string, c modelcall.Client, _ func() time.Time) []Figure {
	cases := schemaCases()
	held, failures := 0, []string{}

	for _, sc := range cases {
		// The answer arrives through emit, not on the Result: a Chat
		// result carries usage and tool calls, and the generated text
		// is the stream. Accumulating it here is what the engine does
		// with the deltas it accepts.
		var answer strings.Builder
		_, err := c.Chat(ctx, modelcall.ChatRequest{
			Model: model,
			Messages: []modelcall.Message{
				{Role: "system", Content: "Answer with JSON matching the schema. No prose, no code fence."},
				{Role: "user", Content: sc.prompt},
			},
			Schema: []byte(sc.schema),
		}, func(chunk string) error {
			answer.WriteString(chunk)
			return nil
		})
		if err != nil {
			// The RUNTIME failed, not the model. One schema's transport
			// error must not be scored as the model failing that
			// schema, so the whole case goes absent -- a validity rate
			// computed over a partial run is a number nobody can read.
			return []Figure{{
				Name:         FigureStructuredValidity,
				AbsentReason: fmt.Sprintf("the structured output case could not run: %v.", err),
			}}
		}
		if err := checkSchemaAnswer(answer.String(), sc.check); err != nil {
			failures = append(failures, sc.name)
			continue
		}
		held++
	}

	fig := Figure{
		Name:   FigureStructuredValidity,
		Value:  float64(held) / float64(len(cases)),
		Detail: fmt.Sprintf("%d of %d schemas held", held, len(cases)),
	}
	if len(failures) > 0 {
		// The failing schemas by NAME. "0.60" tells an operator the
		// model is unreliable; "triage, healing" tells them which
		// prompts to stop routing here.
		fig.Detail += " (missed: " + strings.Join(failures, ", ") + ")"
	}
	return []Figure{fig}
}

// checkSchemaAnswer parses and checks one answer.
//
// A leading code fence is STRIPPED before parsing, and that is a
// deliberate leniency in one direction only: fencing is a rendering
// habit rather than a schema violation, and scoring it as a failure
// would measure the model's chattiness instead of its structure. Prose
// around the JSON is NOT recovered -- there is no brace-hunting here,
// because a "first {" heuristic finds one inside an apology.
func checkSchemaAnswer(content string, check func(map[string]any) error) error {
	body := strings.TrimSpace(content)
	if strings.HasPrefix(body, "```") {
		if _, rest, ok := strings.Cut(body, "\n"); ok {
			body = rest
		}
		body = strings.TrimSuffix(strings.TrimSpace(body), "```")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &m); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	return check(m)
}

func requireString(m map[string]any, key string) error {
	v, ok := m[key]
	if !ok {
		return fmt.Errorf("missing %q", key)
	}
	if _, ok := v.(string); !ok {
		return fmt.Errorf("%q is not a string", key)
	}
	return nil
}

func requireBool(m map[string]any, key string) error {
	v, ok := m[key]
	if !ok {
		return fmt.Errorf("missing %q", key)
	}
	if _, ok := v.(bool); !ok {
		return fmt.Errorf("%q is not a boolean", key)
	}
	return nil
}

func requireArray(m map[string]any, key string) ([]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("missing %q", key)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%q is not an array", key)
	}
	return arr, nil
}

func requireEnum(m map[string]any, key string, allowed ...string) error {
	if err := requireString(m, key); err != nil {
		return err
	}
	got := m[key].(string)
	for _, a := range allowed {
		if got == a {
			return nil
		}
	}
	return fmt.Errorf("%q is %q, not one of %s", key, got, strings.Join(allowed, ", "))
}

func requireUnitNumber(m map[string]any, key string) error {
	v, ok := m[key]
	if !ok {
		return fmt.Errorf("missing %q", key)
	}
	// JSON numbers arrive as float64. A bool is not a number even
	// though some models emit one here, and accepting it would score a
	// wrong type as valid.
	n, ok := v.(float64)
	if !ok {
		return fmt.Errorf("%q is not a number", key)
	}
	if n < 0 || n > 1 {
		return fmt.Errorf("%q is %v, outside 0..1", key, n)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Tool calls
// -----------------------------------------------------------------------------

// toolCase is one tool definition and the call it should produce.
type toolCase struct {
	name     string
	prompt   string
	tool     modelcall.Tool
	wantName string
	wantKeys []string
}

func toolCases() []toolCase {
	return []toolCase{
		{
			name:     "read_file",
			prompt:   "Read the file at /etc/hostname and tell me what is in it.",
			wantName: "read_file",
			wantKeys: []string{"path"},
			tool: modelcall.Tool{
				Name:        "read_file",
				Description: "Read a file from disk.",
				ParametersJSON: `{"type":"object","required":["path"],"properties":{` +
					`"path":{"type":"string","description":"absolute path"}}}`,
			},
		},
		{
			name:     "search",
			prompt:   "Find every use of the symbol NewClient in this repository.",
			wantName: "search",
			wantKeys: []string{"query"},
			tool: modelcall.Tool{
				Name:        "search",
				Description: "Search the repository for a string.",
				ParametersJSON: `{"type":"object","required":["query"],"properties":{` +
					`"query":{"type":"string"},"limit":{"type":"integer"}}}`,
			},
		},
		{
			name:     "run_query",
			prompt:   "How many workers are registered? Use the query tool.",
			wantName: "run_query",
			wantKeys: []string{"statement"},
			tool: modelcall.Tool{
				Name:        "run_query",
				Description: "Run a read-only query against the graph.",
				ParametersJSON: `{"type":"object","required":["statement"],"properties":{` +
					`"statement":{"type":"string"}}}`,
			},
		},
	}
}

// runTools measures the fraction of tool definitions the model called
// correctly: the right tool, with arguments that parse and carry the
// required keys.
//
// Arguments that do not parse count as a MISS rather than an absence.
// A truncated arguments string is the single most consequential
// tool-calling failure there is -- the openai client's accumulator doc
// explains why -- and a model that produces them is one this fleet
// should rank below a model that does not.
func runTools(ctx context.Context, model string, c modelcall.Client, _ func() time.Time) []Figure {
	cases := toolCases()
	correct, misses := 0, []string{}

	for _, tc := range cases {
		res, err := c.Chat(ctx, modelcall.ChatRequest{
			Model: model,
			Messages: []modelcall.Message{
				{Role: "user", Content: tc.prompt},
			},
			Tools: []modelcall.Tool{tc.tool},
		}, func(string) error { return nil })
		if err != nil {
			return []Figure{{
				Name:         FigureToolCorrectness,
				AbsentReason: fmt.Sprintf("the tool call case could not run: %v.", err),
			}}
		}
		if toolCallMatches(res.ToolCalls, tc) {
			correct++
			continue
		}
		misses = append(misses, tc.name)
	}

	fig := Figure{
		Name:   FigureToolCorrectness,
		Value:  float64(correct) / float64(len(cases)),
		Detail: fmt.Sprintf("%d of %d definitions", correct, len(cases)),
	}
	if len(misses) > 0 {
		fig.Detail += " (missed: " + strings.Join(misses, ", ") + ")"
	}
	return []Figure{fig}
}

func toolCallMatches(calls []modelcall.ToolCall, tc toolCase) bool {
	for _, call := range calls {
		if call.Name != tc.wantName {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.ArgumentsJSON), &args); err != nil {
			return false
		}
		for _, k := range tc.wantKeys {
			if _, ok := args[k]; !ok {
				return false
			}
		}
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// Throughput and time to first token
// -----------------------------------------------------------------------------

// throughputCase measures generation speed at a prompt size, and the
// wait before the first token.
//
// The two are reported SEPARATELY because they answer different
// questions and a fleet is ranked on the first while a person waiting
// feels the second: a cold 27B model can be twenty seconds to first
// token and then faster than a warm 9B for the rest of the generation.
// Averaging them would hide both.
func throughputCase(promptTokens int, throughputName, ttftName string) func(context.Context, string, modelcall.Client, func() time.Time) []Figure {
	return func(ctx context.Context, model string, c modelcall.Client, now func() time.Time) []Figure {
		prompt := fillerPrompt(promptTokens)

		// absent reports BOTH figures with the same reason. They are
		// measured by one call, so one failure loses both -- and
		// reporting only the throughput would leave the time-to-first-
		// token row silently missing rather than explained.
		absent := func(reason string) []Figure {
			return []Figure{
				{Name: throughputName, AbsentReason: reason},
				{Name: ttftName, AbsentReason: reason},
			}
		}

		start := now()
		var firstToken time.Time

		res, err := c.Chat(ctx, modelcall.ChatRequest{
			Model: model,
			Messages: []modelcall.Message{
				{Role: "system", Content: "Answer in about two hundred words. Plain prose."},
				{Role: "user", Content: prompt},
			},
			// Bounded output: the case measures a RATE, and letting a
			// chatty model run to its context limit would measure how
			// long it likes to talk. 256 is enough tokens for the rate
			// to be a rate rather than a startup artefact.
			Params: modelcall.Params{MaxOutputTokens: 256},
		}, func(string) error {
			if firstToken.IsZero() {
				firstToken = now()
			}
			return nil
		})
		if err != nil {
			return absent(fmt.Sprintf("the %d-token case could not run: %v.", promptTokens, err))
		}

		elapsed := now().Sub(start)
		if elapsed <= 0 {
			return absent(fmt.Sprintf(
				"the %d-token case reported no elapsed time, so no rate could be computed.", promptTokens))
		}

		out := make([]Figure, 0, 2)

		// Output tokens come from the RUNTIME's own usage when it
		// reported any, and the figure goes absent when it did not.
		// Tokens are never estimated from character counts here: the
		// models package's rule is that usage is reported and never
		// inferred, and a throughput derived from a guessed numerator
		// is a rate that ranks machines on a tokeniser this code does
		// not have.
		switch {
		case !res.Usage.Known || res.Usage.OutputTokens <= 0:
			out = append(out, Figure{
				Name: throughputName,
				AbsentReason: fmt.Sprintf(
					"the runtime reported no token count for the %d-token case, so throughput could not be measured.",
					promptTokens),
			})
		default:
			out = append(out, Figure{
				Name:   throughputName,
				Value:  float64(res.Usage.OutputTokens) / elapsed.Seconds(),
				Unit:   "tokens/sec",
				Detail: fmt.Sprintf("%d tokens in %s", res.Usage.OutputTokens, elapsed.Round(time.Millisecond)),
			})
		}

		// TIME TO FIRST TOKEN IS REPORTED SEPARATELY, and it survives a
		// missing token count: it is measured from the clock alone, so
		// a runtime that reports no usage still answers this question.
		// The two figures answer different things -- a fleet is ranked
		// on the rate while a person waiting feels the wait, and a cold
		// 27B can be twenty seconds to first token and then faster than
		// a warm 9B for the rest.
		//
		// A call that never emitted a delta has no first token to time.
		// That is an absence rather than a zero: zero would be the
		// fastest possible machine, which is the opposite of the truth.
		if firstToken.IsZero() {
			out = append(out, Figure{
				Name: ttftName,
				AbsentReason: fmt.Sprintf(
					"the %d-token case produced no streamed output, so time to first token could not be measured.",
					promptTokens),
			})
		} else {
			out = append(out, Figure{
				Name:   ttftName,
				Value:  firstToken.Sub(start).Seconds(),
				Unit:   "sec",
				Detail: fmt.Sprintf("first token after %s", firstToken.Sub(start).Round(time.Millisecond)),
			})
		}
		return out
	}
}

// fillerPrompt builds a prompt of about n tokens.
//
// GENERATED rather than embedded: a 32,000-token prompt as a string
// literal is 130 KB of this repository that nobody will ever read, and
// the point of the case is the SIZE, not the words. Deterministic, so
// two runs measure the same work -- a prompt that varied between runs
// would put noise straight into the figure the fleet ranks on.
func fillerPrompt(n int) string {
	const seed = "the quick brown fox jumps over the lazy dog and then considers the matter settled "
	// Roughly four characters to a token across common tokenisers. The
	// constant is approximate and it does not need to be exact: both
	// throughput cases use the same one, so the 8K and the 32K case
	// stay in the stated 1:4 ratio to each other, which is what the
	// comparison between them depends on.
	var b strings.Builder
	b.Grow(n * 4)
	b.WriteString("Summarise the following log excerpt.\n\n")
	for b.Len() < n*4 {
		b.WriteString(seed)
	}
	return b.String()
}

// absent reports every figure this case owns with one reason.
//
// It exists because a case can own MORE THAN ONE figure and a failure
// belongs to all of them: a throughput case that timed out has no rate
// and no first-token time, and reporting only the first would leave the
// second as a row that silently never appeared.
func (c probeCase) absent(reason string) []Figure {
	names := c.Names
	if len(names) == 0 {
		names = []string{c.Name}
	}
	out := make([]Figure, 0, len(names))
	for _, n := range names {
		out = append(out, Figure{Name: n, AbsentReason: reason})
	}
	return out
}
