package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// A vendor's refusal of a streaming call arrives BEFORE the stream (design
// record 2026-11-21-vendor-accounts-and-api-keys, K10).
//
// Both SDKs send a streaming request inside NewStreaming and keep its failure
// on the stream they hand back. A provider that returned its channel without
// asking delivered a 401 as the stream's first chunk. The router acts on the
// error a call RETURNS: it moves to the next account for the same model, or to
// the next model. An error that arrives as a chunk reaches the person instead,
// with a key that would have worked sitting unused. Every streaming method
// therefore returns the request's failure as its own (streamRequestRefused),
// without waiting for a first event.

const sseContentType = "text/event-stream"

func openaiSSE(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-test\","+
			"\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", part)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func anthropicSSE(parts ...string) string {
	var b strings.Builder
	event := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",`+
		`"model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, part := range parts {
		event("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, part))
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
	event("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

// keyPaidStreamProvider is a streaming record of the vendor, paid by a stand-in
// key so that no cluster account is needed.
func keyPaidStreamProvider(t *testing.T, v *accountVendor) any {
	t.Helper()
	keys := newFakeVendorKeys()
	keys.secrets["k1"] = standInKey
	client, ok := textProvider(t, v, true).(VendorAccountClient)
	if !ok {
		t.Fatal("the streaming record cannot be paid by an account")
	}
	return client.WithVendorAccount(keyAccount(v.vendor, "k1", "ana"), keys)
}

// streamCall is one streaming method: it reports the text that arrived, in
// order, whether the stream ended cleanly, and the error the CALL returned.
type streamCall func(provider any) (text []string, ended bool, returned error)

func drainText[T any](ch <-chan T, read func(T) (content string, done bool, err error)) ([]string, bool) {
	var text []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return text, false
			}
			content, done, err := read(chunk)
			if content != "" {
				text = append(text, content)
			}
			if err != nil {
				return text, false
			}
			if done {
				return text, true
			}
		case <-deadline:
			return text, false
		}
	}
}

var streamCalls = map[string]streamCall{
	"CallChatStream": func(provider any) ([]string, bool, error) {
		ch, err := provider.(common.ChatStreamProvider).CallChatStream(context.Background(), hello())
		if err != nil || ch == nil {
			return nil, false, err
		}
		text, ended := drainText(ch, func(c common.StreamChunk) (string, bool, error) { return c.Content, c.Done, c.Error })
		return text, ended, nil
	},
	"CallChatStreamWithTools": func(provider any) ([]string, bool, error) {
		ch, err := provider.(common.ChatStreamWithToolsProvider).CallChatStreamWithTools(context.Background(), hello(), nil)
		if err != nil || ch == nil {
			return nil, false, err
		}
		text, ended := drainText(ch, func(c common.StreamToolChunk) (string, bool, error) { return c.Content, c.Done, c.Error })
		return text, ended, nil
	},
	"CallStream": func(provider any) ([]string, bool, error) {
		ch, err := provider.(StreamingAIProvider).CallStream(context.Background(), "hello")
		if err != nil || ch == nil {
			return nil, false, err
		}
		text, ended := drainText(ch, func(c StreamChunk) (string, bool, error) { return c.Content, c.Done, c.Error })
		return text, ended, nil
	},
}

func refusalOf(vendor string) accountVendorAnswer {
	if vendor == VendorAnthropic {
		return accountVendorAnswer{Status: 401, Body: anthropicBody("authentication_error", "invalid x-api-key")}
	}
	return accountVendorAnswer{Status: 401, Body: openaiBody("invalid_request_error", "invalid_api_key", "Incorrect API key provided.")}
}

func streamOf(vendor string, parts ...string) accountVendorAnswer {
	if vendor == VendorAnthropic {
		return accountVendorAnswer{Status: 200, ContentType: sseContentType, Body: anthropicSSE(parts...)}
	}
	return accountVendorAnswer{Status: 200, ContentType: sseContentType, Body: openaiSSE(parts...)}
}

func testStreamRefusalIsReturned(t *testing.T, vendor string) {
	for name, call := range streamCalls {
		t.Run(name, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			v.answerNext(refusalOf(vendor))

			text, _, err := call(keyPaidStreamProvider(t, v))
			if err == nil {
				t.Fatalf("the vendor answered 401 and the call returned no error (it streamed %q): the refusal "+
					"arrives as a chunk, where the router cannot move to the next account", text)
			}
			if kind, ok := VendorAccountFailure(err); !ok || kind != accountFailureRefused {
				t.Fatalf("VendorAccountFailure = (%q, %v) for %v, want refused", kind, ok, err)
			}
		})
	}
}

func testStreamStillDeliversEveryChunk(t *testing.T, vendor string) {
	for name, call := range streamCalls {
		t.Run(name, func(t *testing.T) {
			v := newAccountVendor(t, vendor)
			v.answerNext(streamOf(vendor, "one", " two", " three"))

			text, ended, err := call(keyPaidStreamProvider(t, v))
			if err != nil {
				t.Fatalf("a served stream returned an error: %v", err)
			}
			if got := strings.Join(text, "|"); got != "one| two| three" {
				t.Fatalf("the stream delivered %q, want every chunk, the first included, in order", got)
			}
			if !ended {
				t.Fatal("the stream did not end with a clean Done")
			}
		})
	}
}

func TestOpenAIStreamRefusalIsReturnedBeforeTheStream(t *testing.T) {
	testStreamRefusalIsReturned(t, VendorOpenAI)
}

func TestOpenAIStreamStillDeliversEveryChunk(t *testing.T) {
	testStreamStillDeliversEveryChunk(t, VendorOpenAI)
}

func TestAnthropicStreamRefusalIsReturnedBeforeTheStream(t *testing.T) {
	testStreamRefusalIsReturned(t, VendorAnthropic)
}

func TestAnthropicStreamStillDeliversEveryChunk(t *testing.T) {
	testStreamStillDeliversEveryChunk(t, VendorAnthropic)
}
