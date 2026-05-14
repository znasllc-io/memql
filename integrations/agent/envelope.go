package agent

import (
	"encoding/json"
	"strings"

	"github.com/visionarys-io/memql/core/common"
)

// =============================================================================
// Structured-output envelope for user-facing agent replies
// =============================================================================
//
// Every user-facing reply from an agent's chat path comes back through a
// SINGLE structured envelope, delivered via a sentinel tool call to the
// virtual `respondToUser` tool. The envelope is:
//
//   {
//     "response":  "<the text the user sees>",
//     "citations": [{ "domainId": "...", "matchedPhrase": "..." }, ...]
//   }
//
// Why a tool, not free text:
//
//   - We need the citation list as structured data the frontend can map
//     to clickable chips. Asking the LLM to embed markdown links in
//     prose works ~most of the time but Sonnet has a habit of stripping
//     URI-scheme links it doesn't recognise as "real," which kills the
//     chip experience whenever it triggers. Structured output via a
//     tool with a JSON schema is the part of the API the model is most
//     reliable at -- it's how we already drive routing, suggest, and
//     conductor decisions.
//
//   - It future-proofs the envelope: adding `confidence`, `followups`,
//     `uiActions`, etc. is a schema change, not a prose-parsing
//     overhaul.
//
//   - The streaming tool loop already has all the machinery: tool args
//     stream incrementally, the loop accumulates them, parses the args
//     JSON, and dispatches. We just intercept the dispatch step for
//     this one tool name.
//
// `respondToUser` is a SENTINEL: it has no executor on the engine side.
// runStreamingToolLoop recognises the name, parses the args as
// Envelope, sets that as the turn's final text + citations, and breaks
// the loop -- it never calls engine.ExecuteToolByName. The model's
// system prompt instructs it to end EVERY turn with a respondToUser
// call (the only way to surface text to the user).

// RespondToUserToolName is the sentinel tool the agent must call to
// emit a user-facing reply. The streaming loop intercepts calls with
// this name.
const RespondToUserToolName = "respondToUser"

// CitationKindKnowledge is the default citation kind: the agent quoted
// or relied on a chunk from a knowledge domain. Frontend renders this
// as a clickable chip linking to the named domain.
const CitationKindKnowledge = "knowledge"

// CitationKindGroupThreadUtterance is the chat-architecture Phase 5
// citation kind: the agent quoted a specific utterance from the GROUP
// thread (read via the copresentConversation tool) into their per-user
// TEAM-tab reply. Frontend renders this as a click-to-jump chip that
// scrolls the Group tab to the source utterance. UtteranceId identifies
// the source row.
const CitationKindGroupThreadUtterance = "group_thread_utterance"

// Citation is a single source-attribution entry in the envelope. The
// frontend wraps `MatchedPhrase` (a verbatim substring of the response
// text) in a chip whose target is determined by Kind:
//
//   - Kind == "" or "knowledge": chip links to the knowledge domain
//     identified by DomainId. The default for trained-source citations.
//
//   - Kind == "group_thread_utterance": chip jumps to the specific
//     group-thread utterance identified by UtteranceId (Phase 5 of the
//     chat-architecture plan). DomainId is empty for this kind.
//
// Defaulting Kind to "" preserves the legacy on-the-wire shape: existing
// callers never set Kind and existing renderers treat empty as
// knowledge.
type Citation struct {
	DomainId      string `json:"domainId"`
	MatchedPhrase string `json:"matchedPhrase"`
	Kind          string `json:"kind,omitempty"`
	UtteranceId   string `json:"utteranceId,omitempty"`
}

// Envelope is the structured shape of every user-facing reply.
type Envelope struct {
	Response  string     `json:"response"`
	Citations []Citation `json:"citations,omitempty"`
}

// RespondToUserToolDefinition returns the tool definition injected into
// every agent turn's tools list. The schema is tight: required
// `response` string, optional `citations` array of {domainId, matchedPhrase}.
//
// The description is the LLM-facing contract -- keep it explicit about
// "you MUST call this exactly once at the end of every turn." The
// agentReply.tmpl prompt repeats the rule so it survives the long
// system-prompt context.
func RespondToUserToolDefinition() common.ToolDefinition {
	return common.ToolDefinition{
		Name: RespondToUserToolName,
		Description: "Deliver your final user-facing reply for this turn. " +
			"You MUST call this exactly once at the end of EVERY turn -- it is " +
			"the only way the user sees your response. Do not produce free-form " +
			"assistant text; route everything through this tool. The `response` " +
			"field carries the prose the user reads. The `citations` field lists " +
			"the trained-knowledge sources (if any) you drew from -- one entry per " +
			"distinct source -- so the UI can render clickable source chips. Each " +
			"citation's `matchedPhrase` MUST be a verbatim substring of `response` " +
			"that the chip will wrap (e.g. \"your Customer Relations training\"). " +
			"If you used no trained sources for this reply, pass an empty " +
			"citations array.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"response"},
			"properties": map[string]any{
				"response": map[string]any{
					"type":        "string",
					"description": "The text shown to the user. Plain prose -- no markdown link syntax for citations; use the `citations` field instead.",
				},
				"citations": map[string]any{
					"type":        "array",
					"description": "Knowledge-source citations. Empty array when no trained source was used.",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"domainId", "matchedPhrase"},
						"properties": map[string]any{
							"domainId": map[string]any{
								"type":        "string",
								"description": "The knowledge domain id (bare slug, e.g. \"customer_relations\"). Use the id shown alongside each KNOWLEDGE passage.",
							},
							"matchedPhrase": map[string]any{
								"type":        "string",
								"description": "Verbatim substring of `response` to wrap with the source chip. Pick the natural phrase that names the source in your reply (e.g. \"your Customer Relations training\").",
							},
						},
					},
				},
			},
		},
	}
}

// ParseEnvelope parses a respondToUser tool call's arguments JSON.
// Trims response. Filters out citations with empty domainId or
// matchedPhrase (defensive -- the model occasionally emits stubs).
func ParseEnvelope(argsJSON string) (Envelope, error) {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return Envelope{}, nil
	}
	var env Envelope
	if err := json.Unmarshal([]byte(argsJSON), &env); err != nil {
		return Envelope{}, err
	}
	env.Response = strings.TrimSpace(env.Response)
	if len(env.Citations) > 0 {
		out := make([]Citation, 0, len(env.Citations))
		for _, c := range env.Citations {
			p := strings.TrimSpace(c.MatchedPhrase)
			if p == "" {
				continue
			}
			kind := strings.TrimSpace(c.Kind)
			if kind == "" {
				kind = CitationKindKnowledge
			}
			d := strings.TrimSpace(c.DomainId)
			u := strings.TrimSpace(c.UtteranceId)
			switch kind {
			case CitationKindKnowledge:
				if d == "" {
					continue
				}
				out = append(out, Citation{DomainId: d, MatchedPhrase: p, Kind: kind})
			case CitationKindGroupThreadUtterance:
				if u == "" {
					continue
				}
				out = append(out, Citation{UtteranceId: u, MatchedPhrase: p, Kind: kind})
			default:
				// Unknown kind: keep what the model sent so future kinds
				// don't get silently dropped during a rolling upgrade.
				out = append(out, Citation{
					DomainId:      d,
					MatchedPhrase: p,
					Kind:          kind,
					UtteranceId:   u,
				})
			}
		}
		env.Citations = out
	}
	return env, nil
}
