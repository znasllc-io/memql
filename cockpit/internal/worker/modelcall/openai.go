package modelcall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// The OpenAI-compatible client: LM Studio, vLLM, llamafile, and Ollama's
// own /v1 surface. Chat streams server-sent events; embeddings come back
// whole.

type openAIClient struct {
	transcription string
	voices        map[string]string
	baseURL       string
	apiKey        string
	http          *http.Client
}

type openAIChatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string                `json:"content"`
			Reasoning        string                `json:"reasoning"`
			ReasoningContent string                `json:"reasoning_content"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// openAIToolCallDelta is one FRAGMENT of a tool call on the stream.
// Every field but `index` is optional and arrives whenever the server
// chooses to send it: the id on the first fragment, or several fragments
// later; the name once; the arguments a few characters at a time, split
// wherever the tokeniser happened to split them.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallAccumulator reassembles the tool calls an OpenAI-compatible
// stream sends in pieces.
//
// THE INDEX IS THE ONLY USABLE KEY, and this is the single most
// breakable thing in the file. A server sends
// {"index":0,"id":"call_a","function":{"name":"x"}} on one chunk and
// {"index":0,"function":{"arguments":"{\"a\":"}} on the next, so the id
// is missing from most fragments and the name from nearly all of them;
// two parallel calls are index 0 and index 1 and their fragments
// interleave freely. Keying on anything else -- position in the array,
// the last id seen, the name -- concatenates two different calls'
// arguments into ONE STRING THAT STILL PARSES, and the tool is then
// dispatched with arguments the model never asked for. Nothing
// downstream can detect that.
type toolCallAccumulator struct {
	byIndex map[int]*toolCallParts
}

type toolCallParts struct {
	id   string
	name string
	args strings.Builder
}

func (a *toolCallAccumulator) add(d openAIToolCallDelta) {
	if a.byIndex == nil {
		a.byIndex = make(map[int]*toolCallParts)
	}
	p := a.byIndex[d.Index]
	if p == nil {
		p = &toolCallParts{}
		a.byIndex[d.Index] = p
	}
	// Each field is written only when the fragment CARRIED one, which is
	// why these are guarded rather than plain assignments: every
	// argument fragment repeats the index and omits the id and the name,
	// so an unguarded assignment erases what the header fragment
	// established and the finished call goes out anonymous.
	if d.ID != "" {
		p.id = d.ID
	}
	if d.Function.Name != "" {
		p.name = d.Function.Name
	}
	p.args.WriteString(d.Function.Arguments)
}

// result returns the finished calls in INDEX order -- the order the
// server assigned, and therefore the order the model asked in. Map
// iteration would reshuffle a parallel call set on every run, which
// reads downstream as the model changing its mind between identical
// generations.
func (a *toolCallAccumulator) result() []ToolCall {
	if len(a.byIndex) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(a.byIndex))
	for i := range a.byIndex {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	out := make([]ToolCall, 0, len(indexes))
	for _, i := range indexes {
		p := a.byIndex[i]
		out = append(out, ToolCall{ID: p.id, Name: p.name, ArgumentsJSON: p.args.String()})
	}
	return out
}

func (c *openAIClient) Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": openAIMessages(req.Messages),
		"stream":   true,
		// Without this, a streaming response carries no usage at all and
		// every local call would be billed "unknown" -- servers that do
		// not implement the option ignore it rather than refusing.
		"stream_options": map[string]any{"include_usage": true},
	}
	applyOpenAIParams(body, req.Params)
	if len(req.Schema) > 0 {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": json.RawMessage(req.Schema),
				"strict": true,
			},
		}
	}
	if len(req.Tools) > 0 {
		body["tools"] = openAITools(req.Tools)
	}

	return c.chatRaw(ctx, body, emit)
}

// chatRaw posts an already-assembled chat body and reads the stream.
//
// Split out of Chat so the TRANSCRIPTION path shares this exact reader
// rather than growing a second one. A transcribe call is a chat call
// whose single user turn carries an input_audio part -- same route,
// same server-sent events, same usage handling -- and a second reader
// beside this one would be a second place for the [DONE] terminator
// rule and the tool-call accumulator to drift.
func (c *openAIClient) chatRaw(ctx context.Context, body map[string]any, emit emitFunc) (Result, error) {
	resp, err := c.post(ctx, "/chat/completions", body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	out := Result{FinishReason: FinishStop}
	var tools toolCallAccumulator
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawTerminator := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if string(payload) == "[DONE]" {
			sawTerminator = true
			break
		}
		var chunk openAIChatChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			out.Usage.Model = chunk.Model
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Reasoning != "" || choice.Delta.ReasoningContent != "" {
				reportRuntimeProgress(ctx)
			}
			if choice.Delta.Content != "" {
				if err := emit(choice.Delta.Content); err != nil {
					return out, err
				}
			}
			for _, frag := range choice.Delta.ToolCalls {
				if frag.ID != "" || frag.Function.Name != "" || frag.Function.Arguments != "" {
					reportRuntimeProgress(ctx)
				}
				tools.add(frag)
			}
			if choice.FinishReason != "" {
				out.FinishReason = openAIFinishReason(choice.FinishReason)
			}
		}
		// Usage arrives on its own final frame when include_usage was
		// honoured. Absent, it stays absent.
		if chunk.Usage != nil {
			out.Usage.InputTokens = chunk.Usage.PromptTokens
			out.Usage.OutputTokens = chunk.Usage.CompletionTokens
			out.Usage.Known = true
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if !sawTerminator {
		return out, fmt.Errorf("openai-compatible: stream ended without [DONE]")
	}
	// The tool calls are attached ONLY on the clean path, and the two
	// error returns above deliberately leave them behind. A stream cut
	// mid-arguments holds half a JSON object in the accumulator, and
	// handing that up as a call the model asked for would have the
	// caller dispatch a tool with arguments the model never finished
	// writing. Reporting none is the honest answer for a call that also
	// reports an error.
	out.ToolCalls = tools.result()
	return out, nil
}

type openAIEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int64 `json:"prompt_tokens"`
	} `json:"usage"`
}

func (c *openAIClient) Embed(ctx context.Context, req EmbedRequest) (Result, error) {
	resp, err := c.post(ctx, "/embeddings", map[string]any{
		"model": req.Model,
		"input": req.Input,
	})
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	var parsed openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, err
	}
	// The envelope says "one vector per input, in input order". The API
	// carries an explicit index precisely because the array order is not
	// promised, so the vectors are placed by index rather than appended.
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		at := d.Index
		if at < 0 || at >= len(out) {
			at = i
		}
		out[at] = d.Embedding
	}
	res := Result{FinishReason: FinishStop, Embeddings: out, Usage: Usage{Model: parsed.Model}}
	if parsed.Usage != nil {
		res.Usage.InputTokens = parsed.Usage.PromptTokens
		res.Usage.Known = true
	}
	return res, nil
}

func (c *openAIClient) post(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-compatible: %s returned %d%s", path, resp.StatusCode, detail)
	}
	return resp, nil
}

// openAIMessages maps the envelope's turns onto the chat-completions
// message shape.
//
// The tool on a role="tool" turn is named `name` here, where Ollama calls
// the same thing `tool_name` -- see ollamaMessages for the failure the
// split spelling prevents.
func openAIMessages(in []Message) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		msg := map[string]any{"role": m.Role, "content": m.Content}
		// A turn carrying IMAGES renders its content as an array of
		// parts, which is the only shape an OpenAI-compatible server
		// accepts an inline image in. The text part goes FIRST and is
		// emitted even when empty, because a content array with no text
		// element is rejected by some servers as a malformed message --
		// and a vision call that 400s on punctuation is the least
		// debuggable failure in this file.
		if len(m.Images) > 0 {
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": img.dataURL()},
				})
			}
			msg["content"] = parts
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = openAIToolCallsOut(m.ToolCalls)
		}
		out = append(out, msg)
	}
	return out
}

// openAIToolCallsOut replays an assistant turn's tool calls.
//
// `arguments` goes back as a STRING -- the JSON text, quoted -- which is
// the whole reason ToolCall stores text rather than a decoded object: the
// same stored value leaves here as a string and leaves the Ollama client
// as raw JSON, and neither client has to re-marshal what the model wrote.
func openAIToolCallsOut(in []ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, c := range in {
		out = append(out, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.ArgumentsJSON,
			},
		})
	}
	return out
}

// openAITools renders the offered tools for the request. The schema is
// forwarded verbatim as json.RawMessage, and an empty one omits the key
// -- both for the reasons spelled out on ollamaTools, which this mirrors
// because the request shape is the same on both surfaces.
func openAITools(in []Tool) []map[string]any {
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

func applyOpenAIParams(body map[string]any, p Params) {
	if p.TemperatureSet {
		body["temperature"] = p.Temperature
	}
	if p.TopPSet {
		body["top_p"] = p.TopP
	}
	if p.MaxOutputTokens > 0 {
		body["max_tokens"] = p.MaxOutputTokens
	}
	if len(p.Stop) > 0 {
		body["stop"] = p.Stop
	}
	if p.SeedSet {
		body["seed"] = p.Seed
	}
}

// openAIFinishReason maps this surface's finish reason onto the
// envelope's closed set. "tool_calls" lands on FinishStop along with
// everything unrecognised, and deliberately: the generation FINISHED, its
// output simply happens to be a call rather than prose. The envelope has
// no separate reason for that, and inventing one here would be a wire
// change the engine cannot read -- it would arrive as an unknown finish
// reason on a call that succeeded.
func openAIFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return FinishLength
	default:
		return FinishStop
	}
}

// readErrorBody returns a short, quoted fragment of a failed response for
// the error message.
//
// It is BOUNDED and it is the only place a runtime's body reaches a log.
// A local runtime echoes the prompt back in some error shapes, and a
// prompt is the user's data -- so what lands in a log line is a fragment
// sized for a status explanation, never a body this code streamed whole.
func readErrorBody(resp *http.Response) string {
	const maxErrorBody = 256
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(raw))
}
