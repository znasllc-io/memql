package memql

import (
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

type workContextUnit struct {
	start, end int
	complete   bool
}

// An assistant call and ALL its results are one indivisible protocol unit.
// A missing, duplicate or mismatched result leaves that unit uncompactable.
func workContextUnits(messages []common.ChatMessage) []workContextUnit {
	units := make([]workContextUnit, 0, len(messages))
	for i := 0; i < len(messages); {
		start := i
		m := messages[i]
		i++
		complete := m.Role != "tool"
		if len(m.ToolCalls) > 0 {
			pending := make(map[string]bool, len(m.ToolCalls))
			complete = m.Role == "assistant"
			for _, call := range m.ToolCalls {
				if call.ID == "" || pending[call.ID] {
					complete = false
				}
				pending[call.ID] = true
			}
			for i < len(messages) && messages[i].Role == "tool" {
				id := messages[i].ToolCallId
				if !pending[id] {
					complete = false
				}
				delete(pending, id)
				i++
			}
			complete = complete && len(pending) == 0
		}
		units = append(units, workContextUnit{start, i, complete})
	}
	return units
}

func isWorkMemory(m common.ChatMessage) bool {
	return m.Role == "user" && strings.HasPrefix(m.Content, "[Memory checkpoint ")
}

// Keep authority, the original request, the newest human correction and the
// latest exchange verbatim. Six recent messages is a preference, never a cut
// through a parallel call group. Short transcripts can still retire an older
// complete exchange when the preferred tail occupies the entire conversation.
func workContextChunk(messages []common.ChatMessage) (int, int) {
	units := workContextUnits(messages)
	firstUser, lastUser := -1, -1
	for i, m := range messages {
		if m.Role == "user" && !isWorkMemory(m) {
			if firstUser < 0 {
				firstUser = i
			}
			lastUser = i
		}
	}
	eligible := func(u workContextUnit) bool {
		if !u.complete || u.end == len(messages) {
			return false
		}
		for i := u.start; i < u.end; i++ {
			if i == firstUser || i == lastUser || messages[i].Role == "system" || messages[i].Role == "developer" {
				return false
			}
		}
		return true
	}
	for _, cutoff := range []int{len(messages) - 6, len(messages)} {
		start, end, fresh := -1, -1, false
		for _, u := range units {
			if !eligible(u) || u.start >= cutoff {
				if fresh {
					return start, end
				}
				start, end = -1, -1
				continue
			}
			if start < 0 {
				start = u.start
			}
			// Include a whole first exchange even when it crosses the preferred
			// size; the caller archives oversized units without an LLM call.
			if fresh && WorkContextSize(messages[start:u.end], nil) > 4500 {
				return start, end
			}
			end = u.end
			fresh = fresh || !isWorkMemory(messages[u.start])
		}
		if fresh {
			return start, end
		}
	}
	return 0, 0
}
