package memql

// The Anthropic Messages API REQUIRES at least one entry in `messages`;
// `system` is a top-level parameter, not a message. A system-only
// []common.ChatMessage therefore cannot be forwarded as-is: before the guard
// under test, toAnthropicMessages returned an empty message list and the API
// answered `400 invalid_request_error: "messages: Field required"`. That is
// not hypothetical: InvokeAIStructured's last-resort fallback built exactly
// that shape, so on a cluster with only an Anthropic key every structured
// prompt (agentFactoryAnalyze first among them) failed at the first goal --
// observed in production 2026-08-26, request id req_011CeQjVK9KPC1AW48Wy1bnQ.

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/znasllc-io/memql/core/common"
)

func TestToAnthropicMessagesSystemOnlyStillProducesAUserTurn(t *testing.T) {
	messages, systemBlocks := toAnthropicMessages([]common.ChatMessage{
		{Role: "system", Content: "You decide the specialist for a goal."},
	})

	if len(systemBlocks) != 1 {
		t.Fatalf("system content must stay a system block: got %d blocks", len(systemBlocks))
	}
	if len(messages) == 0 {
		t.Fatal("a system-only conversation must still carry a user turn: Anthropic rejects an empty messages list with 400 \"messages: Field required\"")
	}
	if messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("the synthesized turn must be a user message, got role %q", messages[0].Role)
	}
}

func TestToAnthropicMessagesRealConversationGetsNoSynthesizedTurn(t *testing.T) {
	messages, systemBlocks := toAnthropicMessages([]common.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})

	if len(systemBlocks) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(systemBlocks))
	}
	if len(messages) != 2 {
		t.Fatalf("expected exactly the user+assistant turns, got %d messages", len(messages))
	}
}

func TestStructuredFallbackMessagesCarryAUserTurn(t *testing.T) {
	msgs := structuredFallbackMessages("rendered instructions", []byte(`{"type":"object"}`))

	var sawUser bool
	for _, m := range msgs {
		if m.Role == "user" {
			sawUser = true
			if !strings.Contains(m.Content, `{"type":"object"}`) {
				t.Fatal("the user turn must carry the schema directive")
			}
		}
	}
	if !sawUser {
		t.Fatal("the structured fallback must include a user turn: a system-only list cannot be sent to Anthropic")
	}
}
