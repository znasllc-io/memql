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
		if m.Role == "user" && !isWorkMemory(m) && !common.IsWorkInstruction(m) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last
}

// The current recorded decision may precede reattached goal guidance. Position
// alone would then archive the approval/feedback while retaining its prepared
// call, forcing a fresh Apps session to recover the answer from quoted history.
// Keep only the newest receipt independently of first/last human text; older
// decisions remain archivable. This is context retention, never permission:
// execution still validates the owned, unrevoked, unconsumed approval record.
func workContextCurrentDecision(messages []common.ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.HasPrefix(messages[i].Content, "[Recorded answer for this ") {
			return i
		}
	}
	return -1
}

// WorkContextTarget is a proactive history-compaction preference, not a model
// window. A document and its tool contracts can exceed that preference before
// any history exists. A prepared current call is likewise indivisible, even
// when human approval arrives after it. Preserve those required inputs plus
// bounded working room; completed result bodies still use exact archival.
// the router still enforces the complete request and output reserve against the
// actual provider window. Do not use this floor for recovery from a real overflow.
func WorkContextTarget(messages []common.ChatMessage, tools []common.ToolDefinition, preferred int) int {
	if preferred <= 0 {
		return preferred
	}
	first, last := workContextHumanBounds(messages)
	decision := workContextCurrentDecision(messages)
	freshStart, _ := workContextLatestToolExchange(messages)
	requiredCall := make(map[int]bool)
	for _, unit := range workContextUnits(messages) {
		call := messages[unit.start]
		if call.Role != "assistant" || len(call.ToolCalls) == 0 || (unit.complete && unit.start != freshStart) {
			continue
		}
		// The current call's exact arguments cannot be compacted or recreated
		// from prose. Count its envelope, plus any already returned results
		// while the protocol unit is incomplete. A complete result can instead
		// be referenced from the journal, so its body must not inflate this
		// floor. Older consumed calls remain ordinary compactable history.
		end := unit.start + 1
		if !unit.complete {
			end = unit.end
		}
		for i := unit.start; i < end; i++ {
			requiredCall[i] = true
		}
	}
	pinned := make([]common.ChatMessage, 0, 4)
	for i, m := range messages {
		if i == first || i == last || i == decision || m.Role == "system" || m.Role == "developer" || common.IsWorkInstruction(m) || requiredCall[i] {
			pinned = append(pinned, m)
		}
	}
	return max(preferred, WorkContextSize(pinned, tools)+preferred/5)
}

// Incoming human guidance can follow a completed tool exchange before its
// first reasoning call. It does not make that evidence old. A later assistant
// response or incomplete exchange does: do not pin already-consumed results.
func workContextLatestToolExchange(messages []common.ChatMessage) (start, end int) {
	units := workContextUnits(messages)
	for i := len(units) - 1; i >= 0; i-- {
		unit := units[i]
		m := messages[unit.start]
		switch m.Role {
		case "user", "system", "developer":
			continue
		case "assistant":
			if unit.complete && len(m.ToolCalls) > 0 {
				return unit.start, unit.end
			}
		}
		return -1, -1
	}
	return -1, -1
}

// Give the latest complete exchange a chance to reach reasoning when older
// history can make room. Count every pinned or incomplete unit and the actual
// tool contracts; a tail that cannot fit still needs immediate archival.
func workContextCanRetainTail(messages []common.ChatMessage, tools []common.ToolDefinition, target int) bool {
	first, last := workContextHumanBounds(messages)
	decision := workContextCurrentDecision(messages)
	freshStart, freshEnd := workContextLatestToolExchange(messages)
	var retained []common.ChatMessage
	for _, unit := range workContextUnits(messages) {
		pinned := !unit.complete || unit.end == len(messages) || (unit.start == freshStart && unit.end == freshEnd)
		for i := unit.start; i < unit.end; i++ {
			m := messages[i]
			pinned = pinned || i == first || i == last || i == decision || m.Role == "system" || m.Role == "developer" || common.IsWorkInstruction(m)
		}
		if pinned {
			retained = append(retained, messages[unit.start:unit.end]...)
		}
	}
	return WorkContextSize(retained, tools) <= target
}

// Keep authority, the active execution instruction, the original request, the
// newest human correction and the latest exchange verbatim. The instruction
// may sit in the middle after an approval; its stage boundary is still current.
func workContextArchiveEligibility(messages []common.ChatMessage) func(workContextUnit) bool {
	firstUser, lastUser := workContextHumanBounds(messages)
	decision := workContextCurrentDecision(messages)
	freshStart, freshEnd := workContextLatestToolExchange(messages)
	return func(u workContextUnit) bool {
		if !u.complete || u.end == len(messages) || (u.start == freshStart && u.end == freshEnd) {
			return false
		}
		for i := u.start; i < u.end; i++ {
			if i == firstUser || i == lastUser || i == decision || messages[i].Role == "system" || messages[i].Role == "developer" || common.IsWorkInstruction(messages[i]) {
				return false
			}
		}
		return true
	}
}

// Six recent messages is a preference, never a cut through a parallel call
// group. Short transcripts can still retire an older complete exchange when
// the preferred tail occupies the entire conversation.
func workContextChunk(messages []common.ChatMessage, sourceLimit int, excluded map[string]bool) (int, int) {
	return workContextSelectChunk(messages, sourceLimit, excluded, true)
}

// Semantic summaries need fresh source, otherwise repeated preparation can
// recursively summarize the same derived claims. Exact archival may consolidate
// memory-only spans instead: callers persist every byte before replacing them,
// require a strictly smaller context, and retain the same authority/protocol
// boundaries and bounded attempt count. No checkpoint becomes a permission.
func workContextSelectChunk(messages []common.ChatMessage, sourceLimit int, excluded map[string]bool, requireFresh bool) (int, int) {
	units := workContextUnits(messages)
	eligible := workContextArchiveEligibility(messages)
	usable := func(start, end int, fresh bool) bool {
		if end <= start || (requireFresh && !fresh) {
			return false
		}
		if len(excluded) == 0 {
			return true
		}
		raw, _ := json.Marshal(messages[start:end])
		return !excluded[workContextHash("chunk", raw)]
	}
	for _, cutoff := range []int{len(messages) - 6, len(messages)} {
		start, end, fresh := -1, -1, false
		for _, u := range units {
			if !eligible(u) || u.start >= cutoff {
				if usable(start, end, fresh) {
					return start, end
				}
				start, end, fresh = -1, -1, false
				continue
			}
			if start < 0 {
				start = u.start
			}
			// Include a whole first exchange even when it crosses the preferred
			// size; the caller archives oversized units without an LLM call.
			raw, _ := json.Marshal(messages[start:u.end])
			if (fresh || (!requireFresh && end > start)) && len(raw) > sourceLimit {
				if usable(start, end, fresh) {
					return start, end
				}
				start, fresh = u.start, false
			}
			end = u.end
			fresh = fresh || !isWorkMemory(messages[u.start])
		}
		if usable(start, end, fresh) {
			return start, end
		}
	}
	return 0, 0
}
