package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// ai_anthropic_declined_test.go -- two answers a Claude model can give that
// are not an answer (design record 2026-11-21-vendor-accounts-and-api-keys,
// part 3): a declined request, and a structured reply the format cannot
// express or that was cut off. Both must fail the call, so the route's next
// source serves and the record says why. Each talks to a stand-in.

// declinedBody is what the vendor answers when the model, or a safety
// classifier in front of it, declines: HTTP 200, stop reason "refusal", and
// usually no content at all.
const declinedBody = `{"id":"msg_r","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":"refusal","usage":{"input_tokens":3,"output_tokens":0}}`

func declinedStream() string {
	var b strings.Builder
	event := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",`+
		`"model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I can"}}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":2}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// currentClaude is a record the way the shipped current Claude records are:
// structured output by format, no sampling parameter, no thinking budget.
func currentClaude(t *testing.T, v *accountVendor) any {
	t.Helper()
	cfg := v.config("claudeCurrent", true)
	cfg.Params["structuredOutput"] = "format"
	cfg.Params["maxTokens"] = 16384
	provider, err := newAIProvider(cfg)
	if err != nil {
		t.Fatalf("build the provider: %v", err)
	}
	keys := newFakeVendorKeys()
	keys.secrets["k1"] = standInKey
	return provider.(VendorAccountClient).WithVendorAccount(keyAccount(VendorAnthropic, "k1", "ana"), keys)
}

func TestADeclinedRequestIsAFailureOnEveryPath(t *testing.T) {
	ctx := context.Background()
	tool := common.ToolDefinition{Name: "lookup", Description: "Look a thing up.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}
	calls := map[string]func(provider any) error{
		"a chat turn": func(p any) error {
			_, err := p.(common.ChatAIProvider).CallChat(ctx, hello())
			return err
		},
		"a turn with tools": func(p any) error {
			_, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx, hello(), []common.ToolDefinition{tool})
			return err
		},
		"a structured answer, by format": func(p any) error {
			_, err := p.(common.ChatStructuredProvider).CallChatStructured(ctx, hello(), factorySchema())
			return err
		},
		"a single prompt": func(p any) error {
			_, err := p.(AIProvider).Call(ctx, "hello")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			v := newAccountVendor(t, VendorAnthropic)
			answer := accountVendorAnswer{Status: http.StatusOK, Body: declinedBody}
			if name == "a single prompt" {
				// A streaming record answers a single prompt by gathering its
				// own stream, so the vendor's answer to it is a stream.
				answer = accountVendorAnswer{Status: http.StatusOK, ContentType: sseContentType, Body: declinedStream()}
			}
			v.answerNext(answer)
			err := call(currentClaude(t, v))
			if err == nil {
				t.Fatal("the model declined and the call returned an answer: an empty reply recorded as served, with no fallback")
			}
			if !errors.Is(err, ErrModelDeclined) {
				t.Fatalf("err = %v, want it to say the model declined", err)
			}
			// It says nothing about the account that paid: a decline is the
			// model's, and the next account would be declined the same.
			if kind, about := VendorAccountFailure(err); about {
				t.Fatalf("a decline was read as an account failure (%s)", kind)
			}
		})
	}
}

// A stream that ends in a decline says so at its end: what arrived before it
// is not a complete answer.
func TestAStreamThatEndsInADeclineEndsInAnError(t *testing.T) {
	for name, call := range map[string]func(p any) (ended bool, streamErr error, err error){
		"CallChatStream": func(p any) (bool, error, error) {
			ch, err := p.(common.ChatStreamProvider).CallChatStream(context.Background(), hello())
			if err != nil {
				return false, nil, err
			}
			var streamErr error
			_, ended := drainText(ch, func(c common.StreamChunk) (string, bool, error) {
				if c.Error != nil {
					streamErr = c.Error
				}
				return c.Content, c.Done, c.Error
			})
			return ended, streamErr, nil
		},
		"CallChatStreamWithTools": func(p any) (bool, error, error) {
			ch, err := p.(common.ChatStreamWithToolsProvider).CallChatStreamWithTools(context.Background(), hello(), nil)
			if err != nil {
				return false, nil, err
			}
			var streamErr error
			_, ended := drainText(ch, func(c common.StreamToolChunk) (string, bool, error) {
				if c.Error != nil {
					streamErr = c.Error
				}
				return c.Content, c.Done, c.Error
			})
			return ended, streamErr, nil
		},
		"CallStream": func(p any) (bool, error, error) {
			ch, err := p.(StreamingAIProvider).CallStream(context.Background(), "hello")
			if err != nil {
				return false, nil, err
			}
			var streamErr error
			_, ended := drainText(ch, func(c StreamChunk) (string, bool, error) {
				if c.Error != nil {
					streamErr = c.Error
				}
				return c.Content, c.Done, c.Error
			})
			return ended, streamErr, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := newAccountVendor(t, VendorAnthropic)
			v.answerNext(accountVendorAnswer{Status: http.StatusOK, ContentType: sseContentType, Body: declinedStream()})
			ended, streamErr, err := call(currentClaude(t, v))
			if err != nil {
				t.Fatalf("the call itself failed: %v", err)
			}
			if ended || !errors.Is(streamErr, ErrModelDeclined) {
				t.Fatalf("the stream ended cleanly=%v with error %v; want it to end saying the model declined", ended, streamErr)
			}
		})
	}
}

// A structured reply cut off at the output limit is not the JSON that was
// asked for, whatever text arrived.
func TestAStructuredReplyCutOffAtTheLimitIsAFailure(t *testing.T) {
	v := newAccountVendor(t, VendorAnthropic)
	v.answerNext(accountVendorAnswer{Status: http.StatusOK, Body: `{"id":"msg_c","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"{\"action\":\"mat"}],"stop_reason":"max_tokens","usage":{"input_tokens":3,"output_tokens":16384}}`})
	reply, err := currentClaude(t, v).(common.ChatStructuredProvider).CallChatStructured(context.Background(), hello(), factorySchema())
	if err == nil {
		t.Fatalf("a reply cut off at the limit was returned as the structured answer: %q", reply)
	}
	if !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("err = %v, want it to say the reply was cut off at the output limit", err)
	}
}

// The vendor's format accepts closed objects only. An object that is open in
// the caller's schema -- a map of anything, a map of one type, an object with
// no properties named -- cannot be asked for that way: closing it would make
// the vendor enforce an EMPTY object and hand the caller nothing, with the
// call recorded as served. It is refused here, naming the field, so the
// route's next source serves.
func TestAnOpenObjectIsRefusedByTheFormatPathAndNamesTheField(t *testing.T) {
	for name, tc := range map[string]struct {
		schema string
		names  string
	}{
		"a map of strings":                      {`{"type":"object","properties":{"when":{"type":"object","additionalProperties":{"type":"string"}}}}`, "when"},
		"a map of anything":                     {`{"type":"object","properties":{"extra":{"type":"object","additionalProperties":true}}}`, "extra"},
		"an object with no properties named":    {`{"type":"object","properties":{"payload":{"type":"object"}}}`, "payload"},
		"an open object inside an array":        {`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","additionalProperties":{"type":"number"}}}}}`, "rows"},
		"an open object inside a branch":        {`{"type":"object","properties":{"either":{"anyOf":[{"type":"string"},{"type":"object","additionalProperties":true}]}}}`, "either"},
		"the root itself":                       {`{"type":"object"}`, "the schema"},
		"no schema at all":                      {``, "the schema"},
		"an open object inside a definition":    {`{"type":"object","properties":{"a":{"$ref":"#/$defs/bag"}},"$defs":{"bag":{"type":"object","additionalProperties":{"type":"string"}}}}`, "bag"},
		"additionalProperties set to an object": {`{"type":"object","properties":{"n":{"type":"string"}},"additionalProperties":{"type":"string"}}`, "the schema"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := anthropicFormatSchema(json.RawMessage(tc.schema))
			if err == nil {
				t.Fatal("an object the format cannot express was fitted: the vendor would enforce an empty object")
			}
			if !strings.Contains(err.Error(), tc.names) || !strings.Contains(err.Error(), "open") {
				t.Fatalf("err = %v, want it to name %q and say the object is open", err, tc.names)
			}
			// And no request is assembled for it.
			_, err = anthropicStructuredParams("claude-opus-5-5", map[string]any{"maxTokens": 16384, "structuredOutput": "format"},
				hello(), common.StructuredSchema{Name: "bag", Schema: json.RawMessage(tc.schema)})
			if err == nil {
				t.Fatal("a request was assembled for a schema the format cannot express")
			}
		})
	}

	// What it CAN express is still fitted: named properties with nothing said
	// about extras are closed, and an object declared empty stays empty.
	for name, schema := range map[string]string{
		"named properties, extras unsaid": `{"type":"object","properties":{"a":{"type":"string"}}}`,
		"declared closed and empty":       `{"type":"object","properties":{"empty":{"type":"object","additionalProperties":false}}}`,
		"already closed":                  `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer"}},"additionalProperties":false}},"additionalProperties":false}`,
	} {
		if _, err := anthropicFormatSchema(json.RawMessage(schema)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	// The forced-tool path, for the records that use it, is untouched by any
	// of this: it takes an open object as it always did.
	if _, err := anthropicStructuredParams("claude-sonnet-4-6", map[string]any{"maxTokens": 4096},
		hello(), common.StructuredSchema{Name: "bag", Schema: json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`)}); err != nil {
		t.Errorf("the forced-tool path refused an open object: %v", err)
	}
}
