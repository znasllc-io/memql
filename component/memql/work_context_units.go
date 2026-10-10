package memql

import (
	"encoding/json"
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

func workContextHumanBounds(messages []common.ChatMessage) (first, last int) {
	first, last = -1, -1
	for i, m := range messages {
		if m.Role == "user" && !isWorkMemory(m) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last
}

// WorkContextTarget is a proactive history-compaction preference, not a model
// window. A document and its tool contracts can exceed that preference before
// any history exists. Preserve those pinned inputs plus bounded working room;
// the router still enforces the complete request and output reserve against the
// actual provider window. Do not use this floor for recovery from a real overflow.
func WorkContextTarget(messages []common.ChatMessage, tools []common.ToolDefinition, preferred int) int {
	if preferred <= 0 {
		return preferred
	}
	first, last := workContextHumanBounds(messages)
	pinned := make([]common.ChatMessage, 0, 4)
	for i, m := range messages {
		if i == first || i == last || m.Role == "system" || m.Role == "developer" {
			pinned = append(pinned, m)
		}
	}
	return max(preferred, WorkContextSize(pinned, tools)+preferred/5)
}

// Keep authority, the original request, the newest human correction and the
// latest exchange verbatim. Six recent messages is a preference, never a cut
// through a parallel call group. Short transcripts can still retire an older
// complete exchange when the preferred tail occupies the entire conversation.
func workContextChunk(messages []common.ChatMessage, sourceLimit int) (int, int) {
	units := workContextUnits(messages)
	firstUser, lastUser := workContextHumanBounds(messages)
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
			raw, _ := json.Marshal(messages[start:u.end])
			if fresh && len(raw) > sourceLimit {
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
