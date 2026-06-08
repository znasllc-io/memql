package agent

import (
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/agentdef"
)

// buildPromptData converts the proto AgentGenerateTurnMsg into the
// map[string]any shape the agentReply prompt template expects.
//
// The prompt schema (prompts/v1/agent/agentReply.memql) declares
// trigger / assistant / space / history as @required, so those four
// keys must always be present in the returned map -- even when the
// forwarder ships an empty / minimal routing context. Optional keys
// (participants, spaceContext, peerActivity, attachmentSummaries,
// selectionReason, handoffFrom, textOnlyFallback, surveyProgress,
// tourProgress) are populated only when the forwarder provides them.
func buildPromptData(msg *memqlv1.AgentGenerateTurnMsg) map[string]any {
	data := map[string]any{
		// historyInMessages signals to the template that the history
		// is already on the wire as LLM messages; the template uses
		// this to pick the "continue naturally" branch rather than
		// rendering the history list inline.
		"historyInMessages": true,
		// Defaults for @required fields. Each is overwritten below
		// when the forwarder supplies a value; the defaults keep the
		// schema validator happy when the forwarder sends a minimal
		// routing context (typical at session start).
		"trigger": "utterance",
		"space":   map[string]any{},
		"history": []map[string]any{},
	}

	routing := msg.Routing
	if routing != nil {
		if t := strings.TrimSpace(routing.Trigger); t != "" {
			data["trigger"] = t
		}
		if r := strings.TrimSpace(routing.SelectionReason); r != "" {
			data["selectionReason"] = r
		}
		if h := strings.TrimSpace(routing.HandoffFrom); h != "" {
			data["handoffFrom"] = h
		}
		// Guardrail turn mode drives the template's three branches
		// (answer / fallback_attempt / escalation_notice). Empty defaults
		// to "answer" in the template.
		if tm := strings.TrimSpace(routing.TurnMode); tm != "" {
			data["turnMode"] = tm
		}
		if routing.FitScore > 0 {
			data["fitScore"] = routing.FitScore
		}

		// space -- identity / type / description / goal plus the
		// current human's display name. The agentReply template
		// reads `.space.id`, `.space.spaceType`, `.space.goal`,
		// `.space.description`, and `.space.currentUserDisplayName`.
		// (template uses `spaceType` for the type field; we map the
		// proto's `Type` onto that key for compat.) The retired
		// `architecture` field is intentionally ignored; all spaces
		// are multi-participant now.
		space := map[string]any{}
		if routing.Space != nil {
			if id := strings.TrimSpace(routing.Space.Id); id != "" {
				space["id"] = id
			}
			if t := strings.TrimSpace(routing.Space.Type); t != "" {
				space["type"] = t
				space["spaceType"] = t
			}
			if d := strings.TrimSpace(routing.Space.Description); d != "" {
				space["description"] = d
			}
			if g := routing.Space.GetGoal(); g != nil {
				if s := strings.TrimSpace(g.GetStatement()); s != "" {
					goal := map[string]any{"statement": s}
					if tf := strings.TrimSpace(g.GetTimeframe()); tf != "" {
						goal["timeframe"] = tf
					}
					space["goal"] = goal
				}
			}
			if u := strings.TrimSpace(routing.Space.CurrentUserDisplayName); u != "" {
				space["currentUserDisplayName"] = u
			}
			for k, v := range routing.Space.Extra {
				space[k] = v
			}
		}
		data["space"] = space

		// humans -- the people in the room. The agent prompt's
		// PARTICIPANTS section lists everyone (humans + AI peers);
		// distinct from `peers` which is just AI peers. The
		// `currentSpeaker` flag tells the agent whose utterance
		// triggered this turn without needing to match names.
		if len(routing.Humans) > 0 {
			humans := make([]map[string]any, 0, len(routing.Humans))
			for _, h := range routing.Humans {
				if h == nil {
					continue
				}
				entry := map[string]any{
					"id":              strings.TrimSpace(h.Id),
					"displayName":     strings.TrimSpace(h.DisplayName),
					"participantType": "human",
				}
				if h.IsCurrentSpeaker {
					entry["currentSpeaker"] = true
				}
				humans = append(humans, entry)
			}
			if len(humans) > 0 {
				data["humans"] = humans
			}
		}

		// peerActivity
		if len(routing.PeerActivity) > 0 {
			acts := make([]map[string]any, 0, len(routing.PeerActivity))
			for _, pa := range routing.PeerActivity {
				if pa == nil {
					continue
				}
				acts = append(acts, map[string]any{
					"name":        pa.Name,
					"role":        pa.Role,
					"lastMessage": pa.LastMessage,
					"lastTopic":   pa.LastTopic,
					"messagesAgo": pa.MessagesAgo,
				})
			}
			if len(acts) > 0 {
				data["peerActivity"] = acts
			}
		}
	}

	// history -- convert proto messages into []map[string]any that
	// the template validator accepts. We still ALSO pass these as
	// LLM messages in replier.go; the template treats them as an
	// out-of-band prompt signal, not the primary rendering input.
	if len(msg.History) > 0 {
		history := make([]map[string]any, 0, len(msg.History))
		for _, m := range msg.History {
			if m == nil {
				continue
			}
			history = append(history, map[string]any{
				"role":    strings.TrimSpace(m.Role),
				"content": strings.TrimSpace(m.Content),
			})
		}
		data["history"] = history
	}

	// assistant block -- agent identity + tools + roster + workspace.
	// Populated from AgentGenerateTurnMsg.acting_agent (filled by the
	// cognition forwarder from the v1:agents:agent record) so the
	// template has everything it needs to render the scope fence, tool
	// list, and turn-mode branch. Falls back to the agent ID only when
	// the forwarder omits acting_agent (minimal routing at session start).
	assistant := map[string]any{
		"name": strings.TrimSpace(msg.AgentId),
	}
	if acting := msg.ActingAgent; acting != nil {
		// Project the agent identity through the converged generation
		// contract (#476/#480) so the text path reads the SAME definition the
		// voice path will read (#478) -- neither can render a field the other
		// can't. BuildGenerationContract applies the same trim/omit-empty
		// normalization this block did inline, so the rendered prompt is
		// byte-identical; the new `identityBlock` is the shared region the
		// voice path adopts in #478.
		contract := agentdef.BuildGenerationContract(agentdef.ContractInput{
			AgentID:      acting.Id,
			Name:         acting.Name,
			Description:  acting.Description,
			Personality:  acting.Personality,
			SystemPrompt: acting.SystemPrompt,
			Role:         acting.Role,
			Domains:      acting.Domains,
			Keywords:     acting.Keywords,
			ToolSlugs:    acting.Tools,
		})
		if contract.Name != "" {
			assistant["name"] = contract.Name
		}
		if contract.AgentID != "" {
			assistant["id"] = contract.AgentID
		}
		if contract.Description != "" {
			assistant["description"] = contract.Description
		}
		if contract.Personality != "" {
			assistant["personality"] = contract.Personality
		}
		if contract.SystemPrompt != "" {
			assistant["systemPrompt"] = contract.SystemPrompt
		}
		if contract.Role != "" {
			assistant["role"] = contract.Role
		}
		if len(contract.Domains) > 0 {
			assistant["domains"] = contract.Domains
		}
		if len(contract.Keywords) > 0 {
			assistant["keywords"] = contract.Keywords
		}
		if len(contract.ToolSlugs) > 0 {
			assistant["tools"] = contract.ToolSlugs
		}
		assistant["identityBlock"] = agentdef.RenderIdentityBlock(contract)
	}
	if routing != nil {
		if roster := formatPeerRoster(routing.Peers); roster != "" {
			assistant["peerRoster"] = roster
		}
	}
	data["assistant"] = assistant

	// attachments
	if len(msg.Attachments) > 0 {
		atts := make([]map[string]any, 0, len(msg.Attachments))
		for _, a := range msg.Attachments {
			if a == nil {
				continue
			}
			atts = append(atts, map[string]any{
				"id":      a.Id,
				"kind":    a.Kind,
				"summary": a.Summary,
			})
		}
		if len(atts) > 0 {
			data["attachmentSummaries"] = atts
		}
	}

	// hints -> top-level flags the template cares about
	if v, ok := msg.Hints["text_only_fallback"]; ok && strings.EqualFold(v, "true") {
		data["textOnlyFallback"] = true
	}

	// hints["trigger"] = "plan_approved" is set by cognition's
	// plan-execution subscriber when the user clicked Allow on a
	// scope-elevation canvas card and the corresponding Plan
	// transitioned to status=running. The agent prompt branches
	// on this to skip the per-task approval gate (the user just
	// approved -- requesting elevation again would loop) and
	// dispatch the worker tool directly using the elevated scope.
	// The plan_id rides alongside so the agent can stamp
	// references back to the Plan in the canvas task-done card
	// or any follow-up mutation.
	if msg.Hints != nil {
		if t := strings.TrimSpace(msg.Hints["trigger"]); t == "plan_approved" {
			data["planApprovedTrigger"] = true
			if pid := strings.TrimSpace(msg.Hints["plan_id"]); pid != "" {
				data["planApprovedPlanId"] = pid
			}
		}
		// production_directive (memql#1102) carries the planner's TRUSTED
		// produce-flow scaffolding for a produceArtifact post-approval turn
		// (e.g. "PRODUCE THIS DELIVERABLE NOW ... write the file with the
		// workbench tool, do NOT call produceArtifact"). It is the SYSTEM's
		// own instruction -- the template renders it as an un-bracketed,
		// authoritative block, NOT inside the untrusted history. Folding it
		// into the user-role history message instead made the injection
		// guard refuse it as an embedded "abandon the produce flow"
		// directive, so the deliverable never landed. Surfacing it here, out
		// of band, keeps it trusted.
		if pd := strings.TrimSpace(msg.Hints["production_directive"]); pd != "" {
			data["productionDirective"] = pd
		}
	}

	// Conductor directive (phase 1 transmits via hints, phase 2
	// consumes here). Decoded into a `directive` map the prompt
	// template can branch on. Each field is rendered separately
	// so the template can use simple {{if .directive.skipSelfIntro}}
	// guards rather than parsing modes inline.
	if msg.Hints != nil {
		dirMap := buildDirectiveMap(msg.Hints)
		if len(dirMap) > 0 {
			data["directive"] = dirMap
		}
	}

	return data
}

// buildDirectiveMap mirrors cognition.DecodeDirectiveFromHints but
// produces a map[string]any the template can consume directly. We
// don't import the cognition package here (it would create a cycle);
// the keys are duplicated as constants matching cognition's
// hintDirective* names.
func buildDirectiveMap(hints map[string]string) map[string]any {
	const (
		hintMode              = "directive_mode"
		hintReason            = "directive_reason"
		hintContentHint       = "directive_content_hint"
		hintBrevity           = "directive_brevity"
		hintSkipHandoffOpener = "directive_skip_handoff_opener"
		hintSkipSelfIntro     = "directive_skip_self_intro"
		hintSkipRoomAnnounce  = "directive_skip_room_announce"
	)
	mode := strings.TrimSpace(hints[hintMode])
	if mode == "" {
		// No directive present (older dispatch path or single-binary
		// build). Returning nil tells buildPromptData to not stamp
		// `directive` at all so the template's {{if .directive}}
		// guards skip cleanly.
		return nil
	}
	out := map[string]any{
		"mode": mode,
	}
	if v := strings.TrimSpace(hints[hintReason]); v != "" {
		out["reason"] = v
	}
	if v := strings.TrimSpace(hints[hintContentHint]); v != "" {
		out["contentHint"] = v
	}
	if v := strings.TrimSpace(hints[hintBrevity]); v != "" {
		out["brevity"] = v
	}
	if hints[hintSkipHandoffOpener] == "true" {
		out["skipHandoffOpener"] = true
	}
	if hints[hintSkipSelfIntro] == "true" {
		out["skipSelfIntro"] = true
	}
	if hints[hintSkipRoomAnnounce] == "true" {
		out["skipRoomAnnounce"] = true
	}
	return out
}

// formatPeerRoster renders a human-readable peer list for the prompt.
// Empty when there are no peers.
func formatPeerRoster(peers []*memqlv1.AgentTurnPeer) string {
	if len(peers) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range peers {
		if p == nil {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(p.Name))
		if p.Role != "" {
			b.WriteString(" (")
			b.WriteString(p.Role)
			b.WriteString(")")
		}
		if len(p.Domains) > 0 {
			b.WriteString(" -- ")
			b.WriteString(strings.Join(p.Domains, ", "))
		}
	}
	return b.String()
}
