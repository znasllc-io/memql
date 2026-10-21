package modelcall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// A structured call on an Ollama runner that does not implement `format`
// (memql-cockpit#551).
//
// ===========================================================================
// THE SCHEMA STILL DECIDES; ONLY THE WAY IT IS ASKED FOR CHANGES
// ===========================================================================
// Ollama 0.40.6 runs qwen3.8:27b (NVFP4) on a runner that answers every
// request carrying `format` -- "json", a schema, any schema -- with
//
//	501 {"error":"structured output is unavailable"}
//
// before it generates anything, while the same request without `format`
// answers normally. The engine had sent that machine a structured call
// because the model advertises `structured` (models.ollamaStructuredOutput),
// the call failed, and with no other door able to take it the reply that
// depended on it failed too.
//
// The wire contract stands: a worker returns schema-valid JSON for a
// structured call or fails it, never prose (worker.proto,
// response_format_schema). So on that 501 the call is made once more with the
// schema in the system prompt and without `format`, the whole answer is held
// back, and it is sent only when one JSON value in it validates against the
// request's schema. Anything else fails the call, as the 501 would have.

// ollamaFormatUnsupported reports the runtime refusing `format` itself: 501
// Not Implemented, which Ollama answers before it generates anything.
func ollamaFormatUnsupported(err error) bool {
	var status *ollamaStatusError
	return errors.As(err, &status) && status.status == http.StatusNotImplemented
}

// schemaInstruction is how the schema reaches a model whose runner cannot
// enforce it.
const schemaInstruction = "Answer with one JSON value that validates against the JSON Schema below. " +
	"Write only the JSON: no prose, no code fences, nothing before or after it.\n\nJSON Schema:\n"

func (c *ollamaClient) chatSchemaInPrompt(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error) {
	schema, err := compileResponseSchema(req.Schema)
	if err != nil {
		return Result{}, fmt.Errorf("ollama: this runner does not implement structured output, and the response schema cannot be checked here: %w", err)
	}
	resp, err := c.post(ctx, "/api/chat", c.chatBody(withSchemaInstruction(req), false))
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	var answer strings.Builder
	out, err := c.readChat(ctx, req, resp, func(delta string) error {
		answer.WriteString(delta)
		return nil
	})
	if err != nil {
		return out, err
	}
	value, ok := schemaAnswer(answer.String(), schema)
	if !ok {
		return out, fmt.Errorf("ollama: this runner does not implement structured output, and the model's answer does not match the requested schema")
	}
	if err := emit(value); err != nil {
		return out, err
	}
	return out, nil
}

// withSchemaInstruction puts the schema in the system prompt: appended to
// the first system message, or as a new one ahead of the turns. Chat
// templates read the system message from the start of the conversation, so
// a second system message later on is not one every model is shown.
func withSchemaInstruction(req ChatRequest) ChatRequest {
	instruction := schemaInstruction + string(req.Schema)
	messages := make([]Message, 0, len(req.Messages)+1)
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		first := req.Messages[0]
		first.Content = strings.TrimSpace(first.Content) + "\n\n" + instruction
		messages = append(messages, first)
		messages = append(messages, req.Messages[1:]...)
	} else {
		messages = append(messages, Message{Role: "system", Content: instruction})
		messages = append(messages, req.Messages...)
	}
	req.Messages = messages
	return req
}

// compileResponseSchema compiles the request's schema for checking answers.
func compileResponseSchema(raw []byte) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("response.json", bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return compiler.Compile("response.json")
}

// schemaAnswer finds the one JSON value a model was asked for in its answer
// and returns it when it validates: the whole answer, the inside of a code
// fence, or the span from the first opening brace or bracket to the last
// closing one. Nothing else in the answer is ever passed on.
func schemaAnswer(answer string, schema *jsonschema.Schema) (string, bool) {
	for _, candidate := range jsonCandidates(answer) {
		value, ok := oneJSONValue(candidate)
		if !ok {
			continue
		}
		if schema.Validate(value) == nil {
			return candidate, true
		}
	}
	return "", false
}

func jsonCandidates(answer string) []string {
	text := strings.TrimSpace(answer)
	out := []string{text}
	if fenced, ok := insideFence(text); ok {
		out = append(out, fenced)
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(text, pair[0]), strings.LastIndex(text, pair[1])
		if start >= 0 && end > start {
			out = append(out, text[start:end+1])
		}
	}
	return out
}

// insideFence is the body of a ``` fence, with an optional language tag.
func insideFence(text string) (string, bool) {
	start := strings.Index(text, "```")
	if start < 0 {
		return "", false
	}
	rest := text[start+3:]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		return "", false
	}
	rest = rest[newline+1:]
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

// oneJSONValue decodes exactly one JSON value, numbers kept exact.
func oneJSONValue(text string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return value, true
}
