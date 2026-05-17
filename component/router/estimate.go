package router

import "github.com/znasllc-io/memql/core/common"

// EstimateMessageTokens returns a rough token count for a chat message
// array using a four-characters-per-token heuristic. It is deliberately
// crude: Phase 1 dashboards show approximate costs rather than relying
// on this to be exact. Phase 2 replaces this with provider-reported usage
// once the stream chunk types carry it.
//
// The heuristic charges ~3 tokens of chat-format overhead per message
// (role tag + separators) plus ~1 token per 4 characters of content.
// This tends to under-report on short messages and over-report on
// code-heavy text, but balances out across a typical conversation.
func EstimateMessageTokens(messages []common.ChatMessage) int {
	if len(messages) == 0 {
		return 0
	}
	total := 0
	for _, m := range messages {
		total += 3 // role + separator overhead
		total += tokensFromChars(len(m.Content))
		total += tokensFromChars(len(m.Name))
		for _, tc := range m.ToolCalls {
			total += 3
			total += tokensFromChars(len(tc.Name))
			total += tokensFromChars(len(tc.Arguments))
		}
	}
	return total
}

// EstimateTokensFromChars converts a raw character count to an
// approximate token count using the same four-chars-per-token ratio.
func EstimateTokensFromChars(chars int) int {
	return tokensFromChars(chars)
}

func tokensFromChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	tokens := chars / 4
	if chars%4 != 0 {
		tokens++
	}
	return tokens
}
