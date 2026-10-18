package modelcall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// The Ollama client. Chat streams newline-delimited JSON from /api/chat;
// embeddings come back whole from /api/embed.

type ollamaClient struct {
	baseURL string
	http    *http.Client

	// keepAlive and workingContext are this model's residency settings
	// (policy.yaml models.resident), put on the client by NewClient. The
	// zero values send nothing and change nothing.
	keepAlive      models.KeepAlive
	workingContext int
}

// stampKeepAlive puts the model's keep_alive on a request body.
//
// EVERY ROUTE THAT SENDS THE MODEL WORK CALLS THIS. Ollama applies the
// keep_alive a request STATES from that moment, in either direction -- a
// request saying 90s shortens a residency of hours -- and what a request
// that OMITS it does depends on the release. 0.34.2 leaves the expiry alone
// (measured 2026-10-18), but a model whose first request omitted it loads
// with the five-minute default, and an older release may fall back to the
// default on every omission. A residency that depends on which route
// touched the model first, or on the Ollama version, is not the one the
// owner asked for. Chat (and vision, which is a chat turn), embeddings and
// image generation are the routes today.
func (c *ollamaClient) stampKeepAlive(body map[string]any) {
	if wire := c.keepAlive.Wire(); wire != nil {
		body["keep_alive"] = wire
	}
}

// window is the num_ctx to send for a call that needs `requested` tokens
// (zero when the engine named none).
//
// Ollama reloads a model whenever the requested num_ctx differs from the
// one it loaded with, larger or smaller, even by a token. The engine rounds
// each call up to a power of two (contextStartForCandidate in
// integrations/agent/worker/fleet_context_allocation.go), so the same model
// is asked for 8192, 16384 and 32768 in turn. With a working window set,
// every call that fits in it runs at exactly the window and finds the
// runner the last one left. A call that needs MORE runs at what it asked
// for: clamping it down would truncate a prompt.
func (c *ollamaClient) window(requested int64) int64 {
	if w := int64(c.workingContext); w > 0 && requested <= w {
		return w
	}
	return requested
}

// ollamaChatChunk is one NDJSON line from /api/chat. The same shape
// carries both the incremental content and, on the final line, the
// outcome and the counts.
type ollamaChatChunk struct {
	Error   string `json:"error"`
	Model   string `json:"model"`
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
}

// ollamaToolCall mirrors Ollama's own api.ToolCall.
//
// `arguments` is a JSON OBJECT on this surface, so it is captured as raw
// bytes and handed on as text -- see ToolCall.ArgumentsJSON for why the
// two runtimes' shapes are normalised in the clients rather than above
// them. `id` is omitempty there and usually absent: the models Ollama
// runs do not mint one.
type ollamaToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (c *ollamaClient) Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": ollamaMessages(req.Messages),
		"stream":   true,
		// The engine estimates a working window; only the runtime tokenizes
		// the rendered prompt. Refuse overflow instead of dropping history
		// before generation or shifting it away during generation.
		// Ollama v0.33.3 api.ChatRequest supports both fields.
		"truncate": false,
		"shift":    false,
	}
	params := req.Params
	params.ContextTokens = c.window(params.ContextTokens)
	if opts := ollamaOptions(params); len(opts) > 0 {
		body["options"] = opts
	}
	c.stampKeepAlive(body)
	if len(req.Schema) > 0 {
		// Ollama takes the JSON Schema directly as `format`. It is
		// passed through verbatim rather than re-marshalled: the engine
		// validated it, and a round trip through a map would reorder
		// keys the model was shown.
		body["format"] = json.RawMessage(req.Schema)
	}
	if len(req.Tools) > 0 {
		body["tools"] = ollamaTools(req.Tools)
	}
	if !ollamaThink(req) {
		body["think"] = false
	}

	resp, err := c.post(ctx, "/api/chat", body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	out := Result{FinishReason: FinishStop}
	answered, thought := false, 0
	scanner := bufio.NewScanner(resp.Body)
	// A single token is small, but a non-streaming fallback response can
	// be the whole generation on one line. 4 MB is well past any answer a
	// local model produces and still bounded.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			return out, fmt.Errorf("ollama: invalid stream frame: %w", err)
		}
		if chunk.Error != "" {
			// Ollama can fail after HTTP 200, notably when its tool parser
			// rejects model output or generation exhausts the context. Keep
			// that cause so the engine can repair or compact appropriately.
			return out, fmt.Errorf("ollama: %s", chunk.Error)
		}
		if chunk.Message.Thinking != "" || len(chunk.Message.ToolCalls) > 0 {
			reportRuntimeProgress(ctx)
		}
		thought += len(chunk.Message.Thinking)
		if len(chunk.Message.ToolCalls) > 0 {
			answered = true
		}
		if chunk.Message.Content != "" {
			answered = true
			if err := emit(chunk.Message.Content); err != nil {
				return out, err
			}
		}
		// OLLAMA NEVER STREAMS A PARTIAL TOOL CALL. Each call arrives
		// complete on the frame that carries it -- the name and the whole
		// arguments object together -- unlike the OpenAI-compatible
		// stream, which sends an id in one chunk and the arguments a few
		// characters at a time across several more. So the calls are
		// appended whole and there is no accumulator here. Its absence is
		// not an oversight: someone looking for the streaming path should
		// stop at this comment rather than conclude the decoder is
		// broken and add one that reassembles frames Ollama never sends.
		for _, tc := range chunk.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:            tc.ID,
				Name:          tc.Function.Name,
				ArgumentsJSON: strings.TrimSpace(string(tc.Function.Arguments)),
			})
		}
		if chunk.Done {
			out.FinishReason = ollamaFinishReason(chunk.DoneReason)
			out.Usage = Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
				// Ollama reports counts on the final frame. Absent
				// counts stay absent rather than becoming a confident
				// zero.
				Known: chunk.PromptEvalCount > 0 || chunk.EvalCount > 0,
				Model: chunk.Model,
			}
			if len(req.Schema) > 0 && !answered && thought > 0 {
				// An empty structured answer after hidden thinking is the
				// budget spent on thinking, not a clean stop. Named here, it
				// reaches the engine as a model failure rather than as an
				// empty draft the caller then fails to parse.
				return out, fmt.Errorf("ollama: the model spent its output on thinking (%d characters) and gave no answer", thought)
			}
			return out, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	// The stream ended without a done frame. That is the runtime being
	// killed mid-generation, and it is reported as such rather than as a
	// clean stop -- a caller that parses a truncated structured answer
	// would otherwise blame the model.
	return out, fmt.Errorf("ollama: stream ended without a completion frame")
}

// ollamaThink reports whether a chat call leaves the model's thinking at
// its default. Thinking is hidden from the caller and counts against the
// output budget: on qwen3.5:4b a structured triage spent 1,482 tokens (39 s)
// thinking before a 216-character answer, and 41 tokens (1.2 s) without.
// A structured answer is parsed from the content alone, and a fast call
// must not spend minutes on it, so both send think:false. Free text above
// fast keeps the model's own default. think:false is accepted by models
// that cannot think; only think:true is refused by them.
func ollamaThink(req ChatRequest) bool {
	return len(req.Schema) == 0 && req.Level != "fast"
}

// ollamaEmbedResponse is /api/embed.
type ollamaEmbedResponse struct {
	Model           string      `json:"model"`
	Embeddings      [][]float32 `json:"embeddings"`
	PromptEvalCount int64       `json:"prompt_eval_count"`
}

// embedWorkingContext is the context every embedding call is sent with.
//
// Ollama sizes a model's cache at load for the context it will run at,
// and without a request value that is its VRAM-tier default -- 32K on a
// 24 GB card -- for an embedder whose every layer is full attention. For
// qwen3-embedding:0.6b that is a 3.8 GB cache in front of 639 MB of
// weights, which is what stops the embedder sitting beside a 27B on a
// 24 GB card and turns every embed-then-chat pair into a reload. The
// usual inputs are chunks and queries, so 8K avoids the larger cache.
// Inputs exceeding this working window or the model's own ceiling are
// refused via truncate:false rather than embedded as a successful prefix
// (2026-09-08 record, D6).
const embedWorkingContext = 8192

func (c *ollamaClient) Embed(ctx context.Context, req EmbedRequest) (Result, error) {
	options := map[string]any{"num_ctx": embedWorkingContext}
	// This curated causal embedder can process its full 8K window in smaller
	// batches. On a 24 GB RTX 4090, Ollama 0.33.3's default 2048 batch ran out
	// of compute-buffer memory beside the 32K chat model. A 512 batch kept
	// both resident (with CPU offload) and embedded all 8162 acceptance tokens.
	// Keep other models' defaults: noncausal embedders can require the whole
	// input to fit in one physical batch, so this is not a generic input cap.
	if req.Model == "qwen3-embedding:0.6b" {
		options["num_batch"] = 512
	}
	body := map[string]any{
		"model":    req.Model,
		"input":    req.Input,
		"truncate": false,
		"options":  options,
	}
	c.stampKeepAlive(body)
	resp, err := c.post(ctx, "/api/embed", body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	var parsed ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, err
	}
	return Result{
		FinishReason: FinishStop,
		Embeddings:   parsed.Embeddings,
		Usage: Usage{
			InputTokens: parsed.PromptEvalCount,
			Known:       parsed.PromptEvalCount > 0,
			Model:       parsed.Model,
		},
	}, nil
}

func (c *ollamaClient) post(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: %s returned %d%s", path, resp.StatusCode, detail)
	}
	return resp, nil
}

// ollamaMessages maps the envelope's turns onto Ollama's api.Message.
//
// THE TOOL NAME IS THE FIELD THE TWO RUNTIMES DISAGREE ABOUT: Ollama
// reads it from `tool_name` on a role="tool" turn where the
// OpenAI-compatible surface reads `name`. The disagreement is absorbed
// here so Message stays one shape, and getting it wrong is silent -- the
// runtime's decoder drops the unknown key, the request succeeds, and the
// model is simply never told which tool answered.
func ollamaMessages(in []Message) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		msg := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["tool_name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = ollamaToolCallsOut(m.ToolCalls)
		}
		// Ollama's NATIVE shape takes images as a flat array of bare
		// base64 strings on the message -- no media type, no data URL,
		// which is where it differs from the OpenAI-compatible surface
		// (see openAIMessages). Sending a data: URL here is accepted
		// and then decoded as image bytes that start with the literal
		// text "data:image/png;base64,", so the model is shown noise
		// and answers about it confidently.
		if len(m.Images) > 0 {
			images := make([]string, 0, len(m.Images))
			for _, img := range m.Images {
				images = append(images, base64Of(img.Data))
			}
			msg["images"] = images
		}
		out = append(out, msg)
	}
	return out
}

// ollamaToolCallsOut replays an assistant turn's tool calls.
//
// The arguments go back as RAW JSON, not as the string the
// OpenAI-compatible surface wants: Ollama's `arguments` is an object, and
// a quoted string there reaches the model as a blob it cannot read as
// arguments. Nothing rejects it -- the request succeeds and the model
// answers as though it had asked for something else.
func ollamaToolCallsOut(in []ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, c := range in {
		fn := map[string]any{"name": c.Name}
		if args := strings.TrimSpace(c.ArgumentsJSON); args != "" {
			fn["arguments"] = json.RawMessage(args)
		}
		call := map[string]any{"function": fn}
		if c.ID != "" {
			call["id"] = c.ID
		}
		out = append(out, call)
	}
	return out
}

// ollamaTools renders the offered tools in the shape Ollama's /api/chat
// takes (its api.Tool: type / function / name / description /
// parameters -- the same shape the OpenAI-compatible surface uses).
//
// The schema goes on as json.RawMessage, verbatim, for the reason on
// Tool.ParametersJSON. A tool with an EMPTY schema omits the key rather
// than sending it: an empty json.RawMessage fails json.Marshal outright,
// which would turn "this tool takes no arguments" into a whole request
// that never leaves the machine -- reported to the operator as a runtime
// failure for a request no runtime ever saw.
func ollamaTools(in []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, t := range in {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if params := strings.TrimSpace(t.ParametersJSON); params != "" {
			fn["parameters"] = json.RawMessage(params)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// ollamaOptions maps the envelope's params onto Ollama's option names.
// A knob the caller did not set is OMITTED rather than sent as zero --
// sending temperature 0 for "no preference" would pin every unspecified
// call to greedy decoding.
func ollamaOptions(p Params) map[string]any {
	out := map[string]any{}
	if p.TemperatureSet {
		out["temperature"] = p.Temperature
	}
	if p.TopPSet {
		out["top_p"] = p.TopP
	}
	if p.ContextTokens > 0 {
		out["num_ctx"] = p.ContextTokens
	}
	if p.MaxOutputTokens > 0 {
		out["num_predict"] = p.MaxOutputTokens
	}
	if len(p.Stop) > 0 {
		out["stop"] = p.Stop
	}
	if p.SeedSet {
		out["seed"] = p.Seed
	}
	return out
}

// ollamaFinishReason maps Ollama's done_reason onto the envelope's closed
// set. Anything unrecognised is "stop", because the generation did in
// fact finish and inventing an error class for a new Ollama spelling
// would fail calls that succeeded.
func ollamaFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return FinishLength
	default:
		return FinishStop
	}
}
