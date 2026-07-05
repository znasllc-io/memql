package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/voice"
)

// humanUtteranceDebounce is how long the handler waits after receiving a
// human utterance before running the router + generating a response. If
// another human utterance arrives for the same space during the window,
// this handler exits and the newer one supersedes. Prevents AIs from
// replying to the first message of a multi-part human thought. Empirically
// 400ms is short enough to feel responsive but long enough to absorb
// rapid-fire follow-ups.
const humanUtteranceDebounce = 400 * time.Millisecond

// composeReplyId generates the canonical id for a new agent reply turn.
//
// memQL's node-id convention is "{concept}:{shortId}". Every stored
// utterance lives at that fully-qualified address. The chunks emitted
// while the reply streams carry the same id in their `replyId` field
// so consumers (the chat surface, transcription views) can key the
// in-flight bubble by it AND still match the same string when the
// commit lands. Streaming-to-commit becomes an in-place update on one
// React element -- no remount, no avatar flicker, no duplicate bubble.
//
// We compose the full form here at the dispatch site rather than
// passing a bare UUID because every consumer should see the same
// canonical string. Result is byte-identical to what Concept.Create
// would have produced if we had passed the bare UUID.
func composeReplyId(ctx context.Context) string {
	_ = ctx
	return id.BuildNodeId(memorynodes.ConceptCognitionUtterance, id.NewShortId())
}

// recordLatestHumanUtterance stores this utterance ID as the space's most
// recent. Later calls to isLatestHumanUtterance use it to detect whether a
// newer one has arrived during the debounce window.
func (c *CognitionIntegration) recordLatestHumanUtterance(partitionId, utteranceId string) {
	c.latestHumanUtteranceMu.Lock()
	defer c.latestHumanUtteranceMu.Unlock()
	c.latestHumanUtterance[partitionId] = utteranceId
}

// isLatestHumanUtterance reports whether the given utterance is still the
// most recent human utterance for the space. Returns false when a newer
// one has arrived in the meantime (the newer handler will carry the load).
func (c *CognitionIntegration) isLatestHumanUtterance(partitionId, utteranceId string) bool {
	c.latestHumanUtteranceMu.Lock()
	defer c.latestHumanUtteranceMu.Unlock()
	return c.latestHumanUtterance[partitionId] == utteranceId
}

// handleUtteranceForCognition is the unified handler for utterance events.
// It replaces both handleUtteranceCreatedForTurnState and handleTurnStateCreated
// by using the Polyphon Cognition's deterministic 5-factor scoring engine to decide
// which agent (if any) should respond, then generating and inserting the response
// in a single pipeline — no intermediate turn:state record.
func (c *CognitionIntegration) handleUtteranceForCognition(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	start := time.Now()

	utterance, err := extractUtteranceFromEvent(event)
	if err != nil {
		c.Logger.Warn("cognition: failed to extract utterance from event", "error", err)
		return
	}

	partitionId := strings.TrimSpace(utterance.PartitionId)
	if partitionId == "" {
		return
	}

	// Best-effort: record utterance for prompt context so we can avoid DB reads.
	c.recordRecentUtteranceForPrompt(partitionId, utterance.ID, utterance.ParticipantId, utterance.UtteranceType, utterance.Text)

	// Skip non-conversational utterance types -- these are system-generated
	// tags that should never trigger an AI response.
	//
	// 'system' utterances are pure tags (e.g. join/leave events).
	// 'action' utterances are reactive-agent activity surfaces ("Marketing is
	//   investigating cost savings...") -- they're rendered in the chat
	//   for the user to see but should never trigger another AI response
	//   loop. Letting them through here would cause the cognition pipeline
	//   to react to its own background work, which leads to feedback loops
	//   and pile-on. Phase 3 of the multi-agent rework will write into the
	//   action lane from heartbeat-driven topic listeners; the filter
	//   below is what makes that lane safe to write into.
	switch utterance.UtteranceType {
	case "system", "action":
		return
	}

	// Skip if AI utterance — prevents response loops.
	participantType, err := c.getParticipantType(ctx, utterance.ParticipantId)
	if err != nil {
		c.Logger.Warn("cognition: failed to resolve participant type", "error", err, "participantId", utterance.ParticipantId)
		participantType = ""
	}
	if participantType == "si" {
		// Update AI presence to idle after speaking (fixes stuck "Replying..." status).
		if aiParticipant, err := c.findAIParticipant(ctx, partitionId); err == nil && aiParticipant != nil && strings.TrimSpace(aiParticipant.ID) != "" {
			_ = c.upsertParticipantPresence(ctx, partitionId, aiParticipant.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
		}
		return
	}

	// Skip transcript-only realtime utterances — already handled via voice channel.
	if isTranscriptOnlyRealtimeUtterance(utterance.Source) {
		return
	}

	// Global AI kill switch.
	if !c.IsAIEnabled(ctx) {
		c.Logger.Debug("cognition: AI responses disabled, skipping")
		return
	}

	text := strings.TrimSpace(utterance.Text)
	if text == "" {
		return
	}

	// Debounce: record this utterance as the space's latest and sleep
	// humanUtteranceDebounce. If another human utterance arrives for this
	// space during the window, it overwrites our record and this handler
	// exits without responding -- the newer one will see the full
	// transcript (including this message in the session) and decide once
	// for the combined thought. Does not block ProcessUtterance below; the
	// score engine still records the transcript entry on every call.
	//
	// Voice utterances skip the debounce. The debounce exists to
	// absorb rapid-fire human typing ("hey... wait, what I meant
	// was..."); voice utterances are already silence-chunked by the
	// bridge, so two utterances close enough to interfere don't
	// happen in practice. Saves the full 400ms on every voice turn
	// per the voice-trace waterfall.
	c.recordLatestHumanUtterance(partitionId, utterance.ID)
	isVoiceEarly := isVoiceUtterance(utterance.Source)
	if !isVoiceEarly {
		select {
		case <-time.After(humanUtteranceDebounce):
		case <-ctx.Done():
			return
		}
		if !c.isLatestHumanUtterance(partitionId, utterance.ID) {
			c.Logger.Debug("cognition: utterance superseded during debounce, skipping",
				"partitionId", partitionId, "utteranceId", utterance.ID)
			return
		}
	}

	// Assistant-mediated plan feedback (epic memql#1404 / child #1406):
	// before the normal turn dispatch, check whether a Plan in this space is
	// parked in awaitingFeedback and whether this user message answers its
	// pending question. On a confident hit we route the answer into
	// attachPlanFeedback (which resumes the Plan cross-replica
	// exactly-once, #1405) and post a brief confirmation, then STOP -- the
	// message was the answer to a question, not a fresh turn for an agent to
	// respond to. On any miss / error this falls through to normal dispatch,
	// so the cost of a false negative is just "answer via the card instead."
	lastAgentText := mostRecentAgentText(c.scoreEngine, partitionId)
	if c.tryRouteUtteranceAsPlanFeedback(ctx, partitionId, text, lastAgentText) {
		c.Logger.Info("cognition: utterance routed as plan feedback; skipping normal dispatch",
			"partitionId", partitionId, "utteranceId", utterance.ID)
		return
	}

	// Find all AI participants in the space and build AgentCandidates.
	// Retry once on transient errors -- the cognition handler is the
	// only thing that runs per-utterance, so giving up on a single DB
	// blip means the user types and gets nothing back at all. One retry
	// with a small backoff covers most transient failures (connection
	// pool blip, brief network glitch) without delaying the happy path.
	var aiParticipants []*participantPayload
	for attempt := 0; attempt < 2; attempt++ {
		aiParticipants, err = c.findAllAIParticipants(ctx, partitionId)
		if err == nil {
			break
		}
		c.Logger.Warn("cognition: failed to load AI participants",
			"error", err, "partitionId", partitionId, "attempt", attempt+1)
		if attempt == 0 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}
	if err != nil {
		c.Logger.Error("cognition: giving up after retry; no response generated",
			"error", err, "partitionId", partitionId, "utteranceId", utterance.ID)
		return
	}
	if len(aiParticipants) == 0 {
		return
	}

	candidates := c.buildAgentCandidates(ctx, aiParticipants)
	if len(candidates) == 0 {
		return
	}

	// Start heartbeat for this space if not already running.
	// The heartbeat runs a background goroutine that ticks every 10 seconds,
	// checking for silence, pending events, and predictive state.
	if !c.scoreEngine.Sessions().HasHeartbeat(partitionId) && len(candidates) > 1 {
		evaluator := polyphon.NewDefaultHeartbeatEvaluator()
		c.scoreEngine.Sessions().StartHeartbeat(partitionId, 10*time.Second, evaluator, candidates,
			func(action polyphon.HeartbeatAction) {
				c.handleHeartbeatAction(ctx, partitionId, action, aiParticipants)
			},
		)
	}

	// Start prediction goroutine if not already running (every 30s, only when active).
	if !c.scoreEngine.Sessions().HasPrediction(partitionId) && len(candidates) > 1 {
		invokeAIFunc := c.engine.InvokeAI

		// Adapt embedFunc signature: cognition uses (ctx, text, provider) but
		// the predictive analyzer uses (ctx, text) with default provider.
		var embedFn func(ctx context.Context, text string) ([]float32, error)
		if c.embedFunc != nil {
			embedFn = func(ctx context.Context, text string) ([]float32, error) {
				return c.embedFunc(ctx, text, "")
			}
		}

		// Resolve prediction analyzer: external if configured, model-based otherwise.
		var analyzer polyphon.PredictiveAnalyzer
		modelAnalyzer := polyphon.NewModelPredictiveAnalyzer(invokeAIFunc, embedFn)
		if externalURL := os.Getenv("MEMQL_POLYPHON_PREDICTION_ENGINE_URL"); externalURL != "" {
			analyzer = polyphon.NewExternalPredictiveAnalyzer(externalURL, modelAnalyzer)
		} else {
			analyzer = modelAnalyzer
		}
		c.scoreEngine.Sessions().StartPrediction(partitionId, 30*time.Second, analyzer, candidates)
	}

	// Build the Polyphon utterance.
	speakerName := c.getParticipantDisplayName(ctx, utterance.ParticipantId)
	// Stamp the human speaker's display name into the context so the
	// agent forwarder picks it up and the agent prompt can address
	// the user by name. Empty/anonymous speakers fall through cleanly.
	ctx = contextWithCurrentUserDisplayName(ctx, speakerName)
	scoringUtterance := polyphon.Utterance{
		ID:            utterance.ID,
		ScopeId:       partitionId,
		ParticipantId: utterance.ParticipantId,
		SpeakerName:   speakerName,
		Text:          text,
		IsFinal:       true,
		Timestamp:     time.Now().UTC(),
		Source:        utterance.Source,
	}

	// Parse @ mentions (agents + humans) and attach to utterance. Humans are
	// loaded here so "@Alice" on a human resolves to a human-addressee
	// mention, which the router treats as "AI stays silent -- they're
	// calling out another person, not us".
	humanParticipants, _ := c.findActiveHumanParticipants(ctx, partitionId)
	mentionRefs := make([]polyphon.ParticipantRef, 0, len(candidates)+len(humanParticipants))
	for _, cand := range candidates {
		mentionRefs = append(mentionRefs, polyphon.ParticipantRef{
			ID:              cand.ID,
			Name:            cand.Name,
			ParticipantType: "agent",
		})
	}
	for _, hp := range humanParticipants {
		if hp == nil {
			continue
		}
		name := strings.TrimSpace(hp.DisplayName)
		if name == "" {
			continue
		}
		mentionRefs = append(mentionRefs, polyphon.ParticipantRef{
			ID:              hp.ID,
			Name:            name,
			ParticipantType: "human",
		})
	}
	mentions, _ := polyphon.ParseMentions(scoringUtterance.Text, mentionRefs)
	scoringUtterance.Mentions = mentions

	// Classify utterance intent.
	var prevEntry *polyphon.TranscriptEntry
	if session := c.scoreEngine.Sessions().Get(partitionId); session != nil {
		recent := session.RecentTranscript(1)
		if len(recent) > 0 {
			prevEntry = &recent[len(recent)-1]
		}
	}
	scoringUtterance.Intent = polyphon.ClassifyIntent(scoringUtterance.Text, scoringUtterance.Mentions, prevEntry)

	// Mark voice utterances from the Polyphon pipeline.
	scoringUtterance.IsVoice = isVoiceUtterance(scoringUtterance.Source)

	// Embed utterance for vector domain matching (polyphon multi-agent spaces only).
	// Best-effort: if embedding fails, keyword matching is the fallback.
	if len(candidates) > 1 && c.embedFunc != nil {
		if vec, err := c.embedFunc(ctx, text, ""); err == nil && len(vec) > 0 {
			scoringUtterance.Embedding = vec
		} else if err != nil {
			c.Logger.Debug("cognition: utterance embedding failed (keyword fallback)", "error", err)
		}
	}

	// --- Turn-taking decision ---
	//
	// The heuristic score engine runs every turn and produces signals
	// (per-agent domain match, intent, mentions, session state). Those
	// signals feed into the AI router, which makes the actual decision.
	// The router can choose "nobody responds" as a first-class outcome --
	// that's what lets the system stay silent on utterances that don't
	// need a reply (acknowledgments, reflective statements, humans mid-
	// thought), which is how real conversations work.
	decision := c.scoreEngine.ProcessUtterance(ctx, scoringUtterance, candidates)

	// Suppression decision: should this utterance dispatch at all,
	// or stay silent? Phase 2 of the llm-driven-decisions plan
	// (docs/internal/planning/llm-driven-decisions.md) replaces the old
	// hardcoded affirmation/follow-up/farewell guard with an
	// LLM-driven structured-output classification.
	//
	// The classification (cached per (userText, lastAgentText)
	// pair) reports semantic facts about the message:
	//   - intent kind (greeting / question / request_action /
	//     answer / affirmation / follow_up / correction / ...)
	//   - carriesAction (does the user expect an agent to DO
	//     something?)
	//   - answersPriorAgentPrompt (did the prior agent ask a
	//     question or pose an offer this message answers?)
	//   - addressedAgentName (in-text agent name, with or
	//     without @-mention)
	//   - confidence (how sure the classifier is)
	//
	// Suppress ONLY when:
	//   - no @-mentions in the structured mention list, AND
	//   - no in-text agent name detected by the classifier, AND
	//   - the message doesn't carry an action, AND
	//   - the message doesn't answer a prior agent prompt, AND
	//   - the classification confidence is high (>= 0.7), AND
	//   - the intent is one of the conversational-ack kinds
	//     (affirmation / follow_up / farewell / smalltalk).
	//
	// Otherwise let the conductor decide (which is the
	// LLM-driven decision-maker that already has full context).
	// "When in doubt, dispatch" -- the cost of a false silence is
	// much higher than the cost of a false dispatch.
	// Suppression-classifier decision is deferred to AFTER the
	// goroutine group below so the classifier's ~300-500ms LLM call
	// runs IN PARALLEL with context loading instead of adding to
	// the sequential path. Net latency cut on voice turns is ~500ms,
	// since context loading takes about as long as the classifier
	// and they were both blocking the agent LLM start before this
	// reordering.
	//
	// The trade-off: when the classifier suppresses (the ~minority
	// of turns that are pure ack / farewell / smalltalk), we've
	// already paid for context loading we won't use. That's fine --
	// context loading is cheap, the LLM was the bottleneck.

	var (
		routeOutcome        *routingOutcome
		routeErr            error
		routeMs             int64
		agentConfigs        = make(map[string]*agentPayload, len(candidates))
		agentConfigsMu      sync.Mutex
		participants        []map[string]any
		participantsErr     error
		recentUtterances    []map[string]any
		utterancesErr       error
		sInfo               spaceInfo
		attachmentSummaries []map[string]any
	)

	// UNIFIED-BRAIN DISPATCH (text path):
	//
	//   1. Try the deterministic fast-path (mention-based). If it
	//      hits, dispatch directly -- no LLM call needed.
	//   2. Voice utterances run the LLM router (latency-sensitive
	//      path; the conductor's ~2-3s overhead would stutter TTS).
	//   3. Text utterances run ONLY the conductor after this point.
	//      The conductor produces the routing decision (primary,
	//      fitScore, turnMode, handoff, severity) AND the per-agent
	//      plan in a single LLM call -- no router/conductor split.
	//
	// This unification eliminates the case where router and conductor
	// disagreed and the wrong brain won. The conductor sees the full
	// transcript window plus session memory, so its routing call
	// is more contextually grounded than the router's narrower view.
	isVoiceUtteranceEarly := isVoiceUtterance(scoringUtterance.Source)
	session := c.scoreEngine.Sessions().Get(partitionId)
	if fp := c.tryFastPathDispatch(scoringUtterance, candidates, session); fp != nil {
		routeOutcome = fp
	}

	// Single-agent fast path: if exactly one AI candidate is in the
	// room, the "winner" is obvious and the LLM router is pure
	// overhead. Daily spaces (one user + Sofia the GA) are the
	// canonical case -- saves a measured ~1000ms of routeMs per
	// voice turn before the agent LLM even starts. The mentions
	// fast path above (tryFastPathDispatch) already short-circuits
	// multi-agent rooms when the user @-names someone; this catches
	// the "no mention, only-one-agent-in-the-room" case.
	if routeOutcome == nil && len(candidates) == 1 {
		only := candidates[0]
		handoff, handoffFrom := handoffFromSession(session, only.ID)
		routeOutcome = buildFastPathOutcome(&only, handoff, handoffFrom,
			"Single AI agent in room (deterministic winner; router skipped)",
			"Deterministic routing: only one AI candidate available")
	}

	historyLimit := c.ConversationHistoryLimit(ctx)
	g, gCtx := errgroup.WithContext(ctx)

	// LLM-router goroutine fires ONLY for voice utterances without a
	// fast-path hit. Voice keeps the lightweight si_router path
	// because the conductor's ~1-1.5s sequential LLM call dominates
	// every voice turn end-to-end (was tried in f5e2af9, reverted
	// here after the user-visible latency tax outweighed the
	// correctness gain on the affirmation / silence-decision path).
	// The cheap suppression classifier (see `runClassifier` below)
	// runs on voice too -- without it every thinking-pause final
	// transcript triggers an agent reply, which sounds like the GA
	// is interrupting the user mid-thought.
	if isVoiceUtteranceEarly && routeOutcome == nil {
		g.Go(func() error {
			routeStart := time.Now()
			routeOutcome, routeErr = c.routeWithAI(gCtx, scoringUtterance, candidates, session)
			routeMs = time.Since(routeStart).Milliseconds()
			return nil // never fail the group; errors handled below
		})
	}

	// Suppression classifier in parallel with context loading. The
	// result is consumed AFTER g.Wait() so dispatch can be skipped
	// for affirmations / fragments / farewells without paying for
	// the agent LLM. runClassifier=false ONLY when @-mentioned --
	// mentions are never suppressed.
	//
	// Voice runs the classifier too. Without it, every interim
	// thinking pause the ASR commits as `is_final` triggers
	// an agent reply, including the "um, let me think..." cases
	// where the user is mid-thought. The classifier's
	// `intent=follow_up` + `carriesAction=false` signal is exactly
	// the "this isn't a complete thought yet, wait" check Voice
	// needs even more than text does (text users can rephrase by
	// editing; voice users get talked over).
	//
	// Deterministic short-circuit (#1329 candidate 3): in a
	// 1-human/1-agent TEXT space with no ambiguous @-addressing, the
	// classification is synthesized without an LLM call (see
	// classification_shortcircuit.go for the predicate + safety
	// argument). ClassifyWithShortCircuit returns instantly in that
	// case, so g.Wait() no longer blocks on the classifier for the
	// single-agent daily-space turn the #1329 trace measured.
	var classification MessageClassification
	var classificationOK bool
	runClassifier := len(scoringUtterance.Mentions) == 0
	if runClassifier {
		humanCount := 0
		for _, hp := range humanParticipants {
			if hp != nil {
				humanCount++
			}
		}
		g.Go(func() error {
			lastAgentText := mostRecentAgentText(c.scoreEngine, partitionId)
			roster := agentRosterFromCandidates(candidates)
			var shortCircuited bool
			classification, shortCircuited = c.classifier.ClassifyWithShortCircuit(
				gCtx, scoringUtterance.Text, lastAgentText, roster,
				humanCount, isVoiceUtteranceEarly)
			classificationOK = true
			if shortCircuited {
				c.Logger.Info("cognition: messageClassification short-circuit",
					"partitionId", partitionId,
					"utteranceId", utterance.ID,
					"addressedAgentName", classification.AddressedAgentName,
					"reason", classification.Reasoning,
					"text", scoringUtterance.Text)
			}
			return nil
		})
	}

	// Goroutines B-E: Context loading (parallel with router).
	g.Go(func() error {
		participants, participantsErr = c.getParticipantsForPromptCached(gCtx, partitionId)
		return nil
	})
	g.Go(func() error {
		recentUtterances, utterancesErr = c.getRecentUtterancesForPromptCached(gCtx, partitionId, clampInt(historyLimit, 10, 30))
		return nil
	})
	g.Go(func() error {
		sInfo = c.getSpaceInfoCached(gCtx, partitionId)
		return nil
	})
	g.Go(func() error {
		attachmentSummaries = c.getAttachmentsForPromptCached(gCtx, partitionId)
		return nil
	})

	// Goroutines F..H: Pre-load all candidate agent configs so we don't block on winner.
	for _, ap := range aiParticipants {
		pId := ap.ID
		aId := ap.AgentId
		g.Go(func() error {
			a, err := c.getAgentCached(gCtx, aId)
			if err == nil && a != nil {
				agentConfigsMu.Lock()
				agentConfigs[pId] = a
				agentConfigsMu.Unlock()
			}
			return nil
		})
	}

	_ = g.Wait()

	// Consume the classifier result now that the parallel goroutine
	// finished. Suppress dispatch when high-confidence ack-shaped
	// (affirmation / follow_up / farewell / smalltalk) AND the
	// utterance doesn't carry an action AND doesn't answer a prior
	// agent prompt AND no agent is named in-text. Voice + text both
	// land here; voice users say "OK cool" / "thanks that's it" all
	// the time and they shouldn't trip an agent reply.
	if runClassifier && classificationOK {
		c.Logger.Info("cognition: messageClassification",
			"partitionId", partitionId,
			"utteranceId", utterance.ID,
			"intent", classification.Intent,
			"carriesAction", classification.CarriesAction,
			"answersPriorAgentPrompt", classification.AnswersPriorAgentPrompt,
			"addressedAgentName", classification.AddressedAgentName,
			"confidence", classification.Confidence,
			"reasoning", classification.Reasoning,
			"text", scoringUtterance.Text,
		)

		// Text path: original suppression -- only fire on
		// high-confidence ack intent that doesn't answer a prior
		// prompt or carry an action.
		// Voice path: tighter rules because the cost of a false
		// negative (agent talks over the user mid-thought) is much
		// higher than the cost of a false positive (one missed
		// short reply). Three additions:
		//   (a) `answersPriorAgentPrompt` is ignored. A bare "Okay."
		//       after Sofia's offer is an acknowledgment of
		//       receipt, not an answer worth replying to; the
		//       classifier was correctly labelling these
		//       `affirmation` but the answer-to-prompt bypass let
		//       them through anyway.
		//   (b) The confidence floor drops to 0.5. Voice fragments
		//       are inherently ambiguous; the classifier hedging at
		//       0.5-0.7 is still much better signal than dispatching.
		//   (c) Grammar-shape heuristic: if the utterance text reads
		//       as mid-sentence (ends in a function word / no
		//       terminal punctuation / short), suppress regardless
		//       of intent. The classifier labels grammatical
		//       fragments as `question`/`answer`/`request` because
		//       it pattern-matches structure; the heuristic catches
		//       what the LLM rationalises away.
		var shouldSuppress bool
		var suppressReason string
		if isVoiceUtteranceEarly {
			ackHit := classification.Confidence >= 0.5 &&
				!classification.CarriesAction &&
				classification.AddressedAgentName == "" &&
				isConversationalAckIntent(classification.Intent)
			if ackHit {
				shouldSuppress = true
				suppressReason = "voice_ack"
			} else if looksIncompleteVoiceUtterance(scoringUtterance.Text) &&
				classification.AddressedAgentName == "" {
				shouldSuppress = true
				suppressReason = "voice_fragment"
			}
		} else {
			shouldSuppress = classification.Confidence >= 0.7 &&
				!classification.CarriesAction &&
				!classification.AnswersPriorAgentPrompt &&
				classification.AddressedAgentName == "" &&
				isConversationalAckIntent(classification.Intent)
			if shouldSuppress {
				suppressReason = "text_ack"
			}
		}

		if shouldSuppress {
			c.Logger.Info("cognition: suppressing dispatch (classifier ack)",
				"partitionId", partitionId,
				"utteranceId", utterance.ID,
				"intent", classification.Intent,
				"reason", suppressReason,
				"text", scoringUtterance.Text)
			for _, ap := range aiParticipants {
				_ = c.upsertParticipantPresence(ctx, partitionId, ap.ID,
					presenceStateIdle, "Idle", "", utterance.ID, "", nil)
			}
			return
		}
	}

	// Unified-brain conductor consult for text utterances without a
	// fast-path hit. The conductor produces BOTH the routing decision
	// (winner, fitScore, turnMode, handoff, severity) AND the per-agent
	// plan (sequence, chime-ins, instructions) in a single LLM call.
	// Voice utterances and fast-path mentions skip this -- routeOutcome
	// is already set for them. Voice was briefly conductor-driven in
	// f5e2af9 but the ~1-1.5s sequential LLM call dominated every turn
	// end-to-end; the cheap suppression classifier above still runs
	// on voice and catches the "ok thanks" silence cases at ~500ms
	// cost, which was the high-value subset of what the conductor
	// would have provided.
	var conductorPlan *ConductorPlan
	if !isVoiceUtteranceEarly && routeOutcome == nil {
		consultStart := time.Now()
		plan, planErr := c.consultConductor(ctx, scoringUtterance, candidates,
			agentConfigs, recentUtterances, sInfo, participants)
		routeMs = time.Since(consultStart).Milliseconds()
		if planErr == nil && plan != nil {
			conductorPlan = plan
			routeOutcome = routingOutcomeFromConductorPlan(plan, candidates)
			// Record handoff bookkeeping when the conductor declared one.
			if routeOutcome != nil && routeOutcome.Respond && routeOutcome.Handoff {
				c.recordHandoffOutcome(session, routeOutcome)
			}
		} else {
			// Conductor failed or produced an empty plan. Fall through
			// to the heuristic-decision fallback below by leaving
			// routeOutcome nil + setting routeErr.
			if planErr != nil {
				routeErr = fmt.Errorf("conductorTurn consult: %w", planErr)
			} else {
				routeErr = fmt.Errorf("conductorTurn returned empty plan")
			}
		}
	}

	// Router result is the source of truth. Heuristic decision is the
	// fallback only if the router failed outright (network, parse error).
	// If the router says "nobody should respond", we honor it.
	handoffFrom := ""
	if routeErr != nil {
		heuristicWinner := ""
		if decision.Winner != nil {
			heuristicWinner = decision.Winner.AgentName
		}
		c.Logger.Warn("cognition: AI router failed, falling back to heuristic",
			"error", routeErr, "heuristicWinner", heuristicWinner,
			"partitionId", partitionId, "routeMs", routeMs)
	} else if routeOutcome != nil {
		if !routeOutcome.Respond {
			// First-class "silence" outcome: presence returns to idle for
			// every AI agent in the space (no one should show 'thinking'
			// since no one is working), and we exit without inserting a
			// response utterance. Never emit error-state presence here --
			// silence is the correct answer, not a failure.
			for _, ap := range aiParticipants {
				_ = c.upsertParticipantPresence(ctx, partitionId, ap.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
			}
			reason := ""
			if routeOutcome.Winner != nil {
				reason = routeOutcome.Winner.Reason
			}
			c.Logger.Info("cognition: router chose silence",
				"partitionId", partitionId, "utteranceId", utterance.ID,
				"reason", reason, "routeMs", routeMs,
				"tookMs", time.Since(start).Milliseconds())
			return
		}

		aiWinner := routeOutcome.Winner

		// Deterministic override: when Polyphon's scorer detected a
		// primary direct-address (value==1.0 on the direct_address
		// factor) for an agent that's still a valid candidate, trust
		// that over the AI router's choice. The small chat model
		// occasionally returns an agentName that contradicts its own
		// reasoning text ("The user greeted Pearl directly, so Pearl
		// should respond" -> agentName:"Zara"), and every time that
		// happens the deterministic vocative detector has already
		// picked the right agent. No reason to second-guess it.
		if addressed := polyphonDirectAddressWinner(decision, candidates); addressed != nil &&
			aiWinner != nil && !strings.EqualFold(addressed.AgentId, aiWinner.AgentId) {
			c.Logger.Warn("cognition: AI router contradicted direct-address, overriding",
				"partitionId", partitionId,
				"aiRouterChose", aiWinner.AgentName,
				"aiRouterReason", aiWinner.Reason,
				"polyphonDirectAddress", addressed.AgentName)
			aiWinner = addressed
			routeOutcome.Handoff = false
			routeOutcome.HandoffFrom = ""
		}

		c.Logger.Info("cognition: AI router selected agent",
			"partitionId", partitionId, "aiWinner", aiWinner.AgentName,
			"aiReason", aiWinner.Reason, "toolsNeeded", aiWinner.ToolsNeeded,
			"handoff", routeOutcome.Handoff, "handoffFrom", routeOutcome.HandoffFrom,
			"routeMs", routeMs)
		decision.Winner = aiWinner
		decision.Action = "respond"
		decision.Confidence = "high"
		// Record the router's explicit decision (true OR false) so the
		// downstream forwarder can gate tool injection on it. Previously
		// we only set the context when ToolsNeeded was true, which made
		// "router said no tools needed" indistinguishable from "router
		// didn't have an opinion" -- and we leaked the full tool list
		// either way.
		ctx = contextWithToolsNeeded(ctx, aiWinner.ToolsNeeded)
		if session := c.scoreEngine.Sessions().Get(partitionId); session != nil {
			session.SetAddressedAgent(aiWinner.AgentId)
		}
		if routeOutcome.Handoff {
			handoffFrom = routeOutcome.HandoffFrom
		}
	}

	if !decision.HasWinner() {
		// Reset every agent's presence to Idle (NOT Waiting) so the UI
		// stops indicating "an agent is about to respond" when, in
		// fact, neither the heuristic nor the AI router produced a
		// winner. Previously this set every agent to "Waiting" which
		// reads to the user as "the system is still thinking" and
		// leaves them staring at a typing indicator forever. Idle is
		// honest -- nobody picked this up.
		for _, ap := range aiParticipants {
			_ = c.upsertParticipantPresence(ctx, partitionId, ap.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
		}
		// Promote the no-winner trace to INFO so it's visible without
		// flipping log level. Operators investigating "why didn't
		// anybody respond?" need this in default logs, not debug.
		c.Logger.Info("cognition: no winner produced by heuristic or AI router; staying silent",
			"partitionId", partitionId, "utteranceId", utterance.ID,
			"confidence", decision.Confidence, "tookMs", time.Since(start).Milliseconds(),
			"hint", "consider adding a general-assistant to the space, or lowering the fit threshold via MEMQL_COGNITION_FIT_THRESHOLD")
		return
	}

	// We have a winner.
	winner := decision.Winner
	winnerParticipantId := winner.AgentId

	// Find the participant record for the winning agent.
	var winnerParticipant *participantPayload
	for _, ap := range aiParticipants {
		if ap.ID == winnerParticipantId {
			winnerParticipant = ap
			break
		}
	}
	if winnerParticipant == nil {
		c.Logger.Warn("cognition: winner participant not found", "winnerId", winnerParticipantId, "partitionId", partitionId)
		return
	}

	// Cross-replica dispatch gate (znasllc-io/memql#1217, re-enabled on the
	// reliable substrate per #1272). The utterance-created event is broadcast
	// to BOTH cognition replicas, so both reach this point. Acquire a Postgres
	// advisory lock keyed by the utterance id: exactly one replica wins and
	// proceeds, the loser bails immediately without dispatching (no second LLM
	// turn, no duplicate row). The winner holds the lock until the handler
	// returns (release deferred below), which spans the multi-second turn
	// through insertAIResponse.
	//
	// This is now the GENUINE exactly-once boundary: the single reply it admits
	// is delivered reliably because #1264 routes the chat-reply path through the
	// durable DeliverySubstrate (logical addressing + dedup + replay) -- so the
	// gate gives no dup AND no drop. The loser's bail is enforced for real.
	// Still fails SAFE on any DB/lock error (proceeds, falling through to the
	// read-before-write check just below) so a gate-infra failure can never DROP
	// a reply, and is a no-op on single-replica. See dispatch_gate.go.
	proceed, releaseGate, gateErr := c.dispatchGate.tryDispatch(ctx, utterance.ID)
	if gateErr != nil {
		c.Logger.Warn("cognition: dispatch gate errored; failing safe and proceeding",
			"error", gateErr, "partitionId", partitionId, "utteranceId", utterance.ID)
	}
	if !proceed {
		// Another cognition replica owns this utterance's turn. Bail without
		// dispatching. Do NOT touch presence here -- the winning replica drives
		// the winner's thinking/idle transitions; the loser staying silent
		// avoids presence flicker from two writers.
		c.Logger.Debug("cognition: another replica owns this utterance's dispatch, skipping",
			"partitionId", partitionId, "utteranceId", utterance.ID)
		return
	}
	defer releaseGate()

	// Idempotency: skip if we've already responded to this utterance.
	if c.hasAIResponseForReply(ctx, partitionId, winnerParticipant.ID, utterance.ID) {
		c.Logger.Debug("cognition: AI response already exists, skipping",
			"partitionId", partitionId, "replyToId", utterance.ID)
		_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
		return
	}

	// Set non-winning agents to waiting, winner to thinking.
	for _, ap := range aiParticipants {
		if ap.ID != winnerParticipantId {
			_ = c.upsertParticipantPresence(ctx, partitionId, ap.ID, presenceStateWaiting, "Waiting", "Another agent is responding", utterance.ID, "", nil)
		}
	}
	_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateThinking, "Thinking…", "", utterance.ID, "", nil)

	// Resolve agent config from pre-loaded map.
	agent := agentConfigs[winnerParticipant.ID]
	if agent == nil {
		c.Logger.Error("cognition: failed to get agent config", "agentId", winnerParticipant.AgentId, "partitionId", partitionId)
		_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateError, "Error", "Failed to load agent configuration.", utterance.ID, "", nil)
		return
	}
	if participantsErr != nil {
		c.Logger.Warn("cognition: failed to load participants for prompt", "error", participantsErr)
		participants = nil
	}
	if utterancesErr != nil {
		c.Logger.Warn("cognition: failed to load recent utterances for prompt", "error", utterancesErr)
		recentUtterances = nil
	}

	history := buildHistoryFromRecentUtterances(recentUtterances, winnerParticipant.ID, participants)

	// Build peer agent roster for multi-agent awareness in the prompt.
	// Load all agent payloads for agents in this space (best-effort).
	var allAgentPayloads []*agentPayload
	for _, ap := range aiParticipants {
		if peerAgent, err := c.getAgentCached(ctx, ap.AgentId); err == nil && peerAgent != nil {
			allAgentPayloads = append(allAgentPayloads, peerAgent)
		}
	}

	// Inject tool defaults so that tool calls receive partitionId and
	// participantId even if the AI model omits them.
	defaults := map[string]any{
		"partitionId":   partitionId,
		"participantId": winnerParticipant.ID,
	}
	// Inject workspace for claw-capable agents.
	if agent != nil && agent.ClawCapable() {
		workspace := "/workspaces/" + winnerParticipant.AgentId
		if cap, ok := agent.Capabilities["clawWorkspace"].(string); ok && cap != "" {
			workspace = cap
		}
		defaults["workspace"] = workspace
	}
	ctx = common.ContextWithToolDefaults(ctx, defaults)

	// Task tracker for concurrent tool execution visibility.
	tracker := newAgentTaskTracker()

	// Wire tool activity to presence updates so the frontend shows precise status.
	ctx = common.ContextWithToolActivityCallback(ctx, func(event common.ToolActivityEvent) {
		label := humanizeToolCall(event.ToolName, event.Label)
		switch event.Phase {
		case "start":
			taskId := tracker.Start(event.ToolName, label)
			// Propagate taskId back via the Label field for the "end" phase.
			event.TaskId = taskId
			count := tracker.Count()
			meta := map[string]any{
				"activeTaskCount": count,
				"activeTaskLabel": label,
			}

			if clawToolNames[event.ToolName] {
				displayLabel := label
				if count > 1 {
					displayLabel = fmt.Sprintf("Working on %d tasks", count)
				}
				_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID,
					presenceStateWorking, displayLabel, event.ToolName, utterance.ID, "", meta)

				// Start heartbeat goroutine for long-running claw tools (Step 4).
				heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
				tracker.SetCancel(taskId, cancelHeartbeat)
				taskStartTime := time.Now()

				go func(hCtx context.Context, tId string, tLabel string) {
					// First heartbeat at 5 seconds.
					timer := time.NewTimer(5 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						elapsed := time.Since(taskStartTime)
						cnt := tracker.Count()
						heartbeatLabel := fmt.Sprintf("Still working... (%ds)", int(elapsed.Seconds()))
						if cnt > 1 {
							heartbeatLabel = fmt.Sprintf("Working on %d tasks (%ds)", cnt, int(elapsed.Seconds()))
						}
						hMeta := map[string]any{
							"activeTaskCount": cnt,
							"activeTaskLabel": tLabel,
						}
						_ = c.upsertParticipantPresence(hCtx, partitionId, winnerParticipant.ID,
							presenceStateWorking, heartbeatLabel, "", utterance.ID, "", hMeta)
					case <-hCtx.Done():
						return
					}

					// Subsequent heartbeats every 10 seconds.
					ticker := time.NewTicker(10 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-ticker.C:
							elapsed := time.Since(taskStartTime)
							cnt := tracker.Count()
							heartbeatLabel := fmt.Sprintf("Still working... (%ds)", int(elapsed.Seconds()))
							if cnt > 1 {
								heartbeatLabel = fmt.Sprintf("Working on %d tasks (%ds)", cnt, int(elapsed.Seconds()))
							}
							hMeta := map[string]any{
								"activeTaskCount": cnt,
								"activeTaskLabel": tLabel,
							}
							_ = c.upsertParticipantPresence(hCtx, partitionId, winnerParticipant.ID,
								presenceStateWorking, heartbeatLabel, "", utterance.ID, "", hMeta)
						case <-hCtx.Done():
							return
						}
					}
				}(heartbeatCtx, taskId, label)
			} else {
				displayLabel := fmt.Sprintf("Using %s...", event.ToolName)
				if count > 1 {
					displayLabel = fmt.Sprintf("Working on %d tasks", count)
				}
				_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID,
					presenceStateUsingTool, displayLabel, "", utterance.ID, "", meta)
			}

		case "end":
			// Find task ID: try event.TaskId first, fall back to matching.
			if event.TaskId != "" {
				tracker.End(event.TaskId)
			}
			count := tracker.Count()
			if count > 0 {
				activeLabel := tracker.ActiveLabel()
				meta := map[string]any{
					"activeTaskCount": count,
					"activeTaskLabel": activeLabel,
				}
				displayLabel := activeLabel
				if count > 1 {
					displayLabel = fmt.Sprintf("Working on %d tasks", count)
				}
				_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID,
					presenceStateWorking, displayLabel, "", utterance.ID, "", meta)
			} else {
				meta := map[string]any{
					"activeTaskCount": 0,
					"activeTaskLabel": "",
				}
				_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID,
					presenceStateTyping, "Typing...", "", utterance.ID, "", meta)
			}
		}
	})

	// Wire early text callback: emit an acknowledgment utterance before tools execute.
	earlyTextInserted := false
	ctx = common.ContextWithEarlyTextCallback(ctx, func(text string) {
		if earlyTextInserted {
			return
		}
		earlyTextInserted = true
		// Early ack is its own (short, non-streaming) utterance, so it
		// gets a freshly-minted id rather than sharing replyId.
		if insertErr := c.insertAIResponse(ctx, partitionId, winnerParticipant, "", utterance.ID, text, nil, nil, nil); insertErr != nil {
			c.Logger.Warn("cognition: failed to insert early acknowledgment", "error", insertErr)
		} else {
			c.Logger.Info("cognition: early acknowledgment emitted",
				"partitionId", partitionId,
				"textLen", len(text),
			)
		}
		_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID,
			presenceStateWorking, "Working...", "", utterance.ID, "", nil)
	})

	t0 := time.Now()

	// Derive a human-readable selection reason from the winner's highest-scoring factor.
	selectionReason := deriveSelectionReason(winner)
	ctx = contextWithSelectionReason(ctx, selectionReason)

	// Pass utterance intent to AI responder for dynamic tool gating.
	ctx = contextWithIntent(ctx, scoringUtterance.Intent)

	// When the router signaled a handoff (winner differs from previous
	// responder), attach the previous responder's name so the reply
	// prompt can open with a brief acknowledgment instead of a cold start.
	if handoffFrom != "" {
		ctx = contextWithHandoffFrom(ctx, handoffFrom)
	}

	// Guardrail context: propagate the router's turn mode, fit score, and
	// current handoff-chain depth so the agent template renders the right
	// branch (answer / fallback_attempt / escalation_notice) and the
	// routing context carries telemetry for the agent-side logs.
	if routeOutcome != nil {
		ctx = contextWithTurnMode(ctx, routeOutcome.TurnMode)
		ctx = contextWithFitScore(ctx, routeOutcome.FitScore)
	}
	if session := c.scoreEngine.Sessions().Get(partitionId); session != nil {
		ctx = contextWithHandoffChainDepth(ctx, session.GetHandoffChainDepth())
	}

	// Generate AI reply.
	//
	// The voice path (utterance originated in the Polyphon voice
	// pipeline) runs the non-streaming one-shot generator so we avoid
	// tool-calling overhead on the TTS hot path. Every other
	// utterance runs the streaming tool-calling loop.
	//
	// With the group-only space refactor, there's no longer a
	// per-space architecture flag -- the voice-pipeline distinction
	// lives entirely on the utterance source metadata.
	//
	// Single-binary (no cluster) builds have no forwarder -- they
	// fall back to local generation via generateAIResponse /
	// generateStreaming.
	var response string
	// Citations from the respondToUser envelope (cluster forwarder
	// path only -- single-binary streaming/sync paths don't carry
	// them yet). Persisted on the committed utterance so the
	// frontend can render chips alongside the reply text.
	var responseCitations []*memqlv1.AgentTurnCitation
	// Retrieval audit -- the full RAG chunk pool the agent's replier
	// surfaced to the LLM. Same wire path as citations; persisted on
	// the same utterance row so the frontend "Show details" expander
	// can show what was searched (including chunks the model didn't
	// cite). Empty for non-forwarder paths.
	var responseRetrieved []*memqlv1.AgentRetrievedChunk
	isVoice := isVoiceUtterance(scoringUtterance.Source)
	// Decide whether to disable tools for this forwarded turn. Voice
	// utterances always run the non-streaming generator (tool latency
	// would break the TTS hot path). Beyond that, only the router's
	// `turnMode` is a strong-enough signal to short-circuit the
	// streaming tool loop:
	//
	//   - escalation_notice  (router wants a refusal, not action)
	//   - fallback_attempt   (assistant stretching;
	//                         answer from knowledge, don't tool-call)
	//
	// Earlier this also gated on `toolsNeeded=false`, but the router
	// prompt defines toolsNeeded narrowly ("data lookup, file
	// operation, or code execution") -- UI-driving for Operator
	// agents doesn't fit that definition, so a Sofia asked to
	// "create an agent for ops" gets toolsNeeded=false (correctly,
	// by the prompt's lights) and stripping her tools breaks the
	// whole UI-control (Operator) flow. The right place to keep agents
	// from tool-spamming greetings is the agent-side prompt's
	// tool-use decision tree (the operatorEnabled-gated branch
	// near the top of agentReply.tmpl), not a hard Go-side gate.
	// We keep the toolsNeeded plumbing in the context for telemetry
	// + future use; we just don't disable the loop on it.
	// Tools stay ENABLED for voice utterances. Disabling them would
	// strip the sentinel respondToUser tool too, and the prompt
	// instructs the model to terminate every turn with that call --
	// without it registered, the model emits the literal text
	// "respondToUser({...args})" as plain content, which then leaks
	// into chat as the agent's response. The streaming layer needs
	// the structured tool call to parse the Envelope and extract
	// the user-facing text. Side-effect tools (uiClick, claw, etc)
	// are gated by the prompt itself, not a Go-side flag.
	disableTools := false
	switch turnModeFromContext(ctx) {
	case "escalation_notice", "fallback_attempt":
		disableTools = true
	}
	// Build the conductor directive for the primary winner. The
	// directive captures decisions cognition has already made (does
	// the user already know this agent? did they address the room?
	// is this a real takeover or a parallel response?) and routes
	// them through to the prompt as structured flags so the agent
	// doesn't have to re-derive them from prompt context.
	conductorState := c.conductors.GetOrCreate(partitionId)
	conductorState.RecordHumanSpoke()
	primaryDirective := BuildDirective(
		conductorState,
		winner.AgentId,
		true,  // isPrimaryWinner
		false, // isChimeIn
		handoffFrom,
		userAddressedRoom(scoringUtterance.Text),
		c.queryAgentIsKnownToUser(ctx, agent.ID),
	)

	// Layer the conductor's per-turn intelligence onto the primary
	// directive. The conductor consult already ran sequentially after
	// g.Wait (above) and produced `conductorPlan` in the outer scope;
	// we just stamp its outputs into the directive here.
	//
	// In the unified-brain text path, conductorPlan's primary IS the
	// dispatched winner (routingOutcomeFromConductorPlan built the
	// routeOutcome from it), so the primary's instruction maps
	// directly. The "primary mismatch" case from the old router-vs-
	// conductor split can no longer occur for text turns.
	//
	// Voice utterances skip the conductor (latency-sensitive); they
	// fall through to the legacy BuildDirective shape and conductorPlan
	// stays nil here.
	if conductorPlan != nil {
		plan := conductorPlan
		var primaryInstruction string
		if plan.PrimaryAgentId() == winnerParticipant.ID {
			primaryInstruction = strings.TrimSpace(plan.Primary.Instruction)
		} else if winnerPlan := plan.ChimeInForAgent(winnerParticipant.ID); winnerPlan != nil {
			// Defensive fallback: if some path mutated the dispatch
			// winner away from the conductor's primary, look up the
			// winner in the chime-in list. Should be rare in the
			// unified flow.
			primaryInstruction = strings.TrimSpace(winnerPlan.Instruction)
			c.Logger.Info("conductor: primary mismatch (post-unification fallback)",
				"partitionId", partitionId,
				"dispatchedWinner", winnerParticipant.ID,
				"conductorPrimary", plan.PrimaryAgentId())
		}
		if primaryInstruction != "" {
			primaryDirective.Instruction = primaryInstruction
		}
		primaryDirective.GlobalGuidance = strings.TrimSpace(plan.GlobalGuidance)
		primaryDirective.Phase = strings.TrimSpace(plan.Phase)
		primaryDirective.Temperature = strings.TrimSpace(plan.Temperature)
		primaryDirective.UserIntent = strings.TrimSpace(plan.UserIntent)
		primaryDirective.AcknowledgePrior = strings.TrimSpace(plan.Primary.AcknowledgePrior)
		primaryDirective.ExpectedOutput = strings.TrimSpace(plan.Primary.ExpectedOutput)
		switch strings.TrimSpace(plan.Primary.Brevity) {
		case "short":
			primaryDirective.Brevity = BrevityShort
		case "detailed":
			primaryDirective.Brevity = BrevityDetailed
		case "normal":
			primaryDirective.Brevity = BrevityNormal
		}
	}

	// Voice-mode brevity override -- voice replies that aren't capped
	// produce 800+ char monologues like Sofia's full capabilities tour
	// from a casual "what can you do" turn, which then plays back as
	// 49 seconds of TTS audio. The conductor doesn't run on voice
	// turns (latency-sensitive), so its `brevity=short` knob never
	// fires there. Force it here, after BuildDirective + the
	// conditional conductor overlay, so voice always lands at one or
	// two sentences regardless of what the model would have produced
	// for the same utterance in chat. The Instruction string surfaces
	// the rule explicitly so the prompt doesn't have to infer it from
	// brevity alone.
	if isVoice {
		primaryDirective.Brevity = BrevityShort
		// Hard cap voice replies. The previous wording ("under ~40
		// words / 1-2 sentences") was being interpreted loosely --
		// turn 3 of the latest trace produced 351 chars (~60 words)
		// for "what can you do?". Tighten to specific numeric caps
		// the model can self-check against, and call out the
		// "what can you do?" failure mode by name.
		voiceInstruction := "VOICE MODE -- HARD LENGTH CAP. Reply MUST be under 30 words AND under 200 characters. Aim for one sentence; two short sentences max. Absolutely no bullet lists, no enumerations, no capability tours. For 'what can you do?' / 'how can you help?' / 'what are you able to do?' style questions: name 1-2 categories MAX in one sentence (e.g. 'I can answer questions or drive the app for you -- which sounds useful?') and STOP. The user will ask follow-ups; long-form belongs in chat."
		if primaryDirective.Instruction == "" {
			primaryDirective.Instruction = voiceInstruction
		} else {
			// Conductor never runs on voice today, so this branch is
			// defensive -- if a future voice path layers a conductor
			// instruction in, we append rather than clobber it so the
			// brevity rule always fires.
			primaryDirective.Instruction = primaryDirective.Instruction + "\n\n" + voiceInstruction
		}

		// A2 (#1198, epic #1197): when the agent-tool-loop flag is on, cognition
		// does NOT publish the gate directive + return. Instead it falls through to
		// the normal agent-loop path below so a voice turn runs the SAME full tool
		// loop text chat runs (produceArtifact etc.) and authors a brief reply; the
		// realtime model re-voices the authored FinalText (returned via the relay's
		// reply-utterance path, directive_mode empty). The brevity instruction
		// above still caps the spoken length. Flag off keeps the proven #479 gate
		// path unchanged (cognition gates WHEN/brevity, the model authors WHAT).
		if !voiceAgentToolLoopEnabled() {
			// #479: hand the turn to the realtime model. Cognition has decided WHO
			// (the winner) and WHEN/how-briefly (the directive) -- it stays the
			// director + scribe. It no longer AUTHORS the voice reply: publish the
			// gate directive (mode + brevity) and stop here. The relay forwards it on
			// VoiceAgentTurnComplete, the realtime model generates the words natively,
			// and its spoken output is captured as the AI utterance
			// (handleVoiceAgentRealtimeOutput). This removes the ~1-1.5s authoring
			// LLM call from the voice critical path (#475/#477). Voice is GA-only, so
			// there is exactly one agent to gate here.
			gateMode := string(primaryDirective.Mode)
			if strings.TrimSpace(gateMode) == "" || strings.EqualFold(gateMode, string(DirectiveDefer)) {
				gateMode = string(DirectivePrimary)
			}
			gateBrevity := string(primaryDirective.Brevity)
			if strings.TrimSpace(gateBrevity) == "" {
				gateBrevity = string(BrevityShort)
			}
			// Grounding (#490, opt-in via MEMQL_VOICE_GROUNDING): retrieve the top
			// knowledge chunks for this turn over the agent's domains and ride the
			// rendered block to the executor on the directive, so the model-authored
			// voice reply is grounded. Off by default; fail-safe (empty -> no
			// grounding, no behaviour change).
			var grounding string
			if voiceGroundingEnabled() {
				grounding = c.retrieveVoiceGroundingBlock(ctx, scoringUtterance.Text, agent.domains())
			}
			c.publishVoiceGateDirective(ctx, partitionId, utterance.ID, VoiceGateDecision{
				Engage: true, Mode: gateMode, Brevity: gateBrevity, Reason: "voice_engage",
			}, grounding)
			if c.Logger != nil {
				c.Logger.Info("voice trace: gate directive published",
					"voiceTrace", utterance.ID,
					"stage", "cognition.gate.engage",
					"partitionId", partitionId,
					"agentName", winner.AgentName,
					"mode", gateMode,
					"brevity", gateBrevity)
			}
			// Reset the winner's presence so it does not stick in a thinking state
			// now that cognition no longer authors the voice reply (the model speaks,
			// and the realtime output capture lands the utterance row).
			_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
			return
		}
		if c.Logger != nil {
			c.Logger.Info("voice trace: agent tool loop engaged (A2)",
				"voiceTrace", utterance.ID,
				"stage", "cognition.toolloop.engage",
				"partitionId", partitionId,
				"agentName", winner.AgentName)
		}
	}

	// replyId: canonical id for this whole agent reply. Fully qualified at
	// the source (partition:concept:uuid) so it's byte-identical to the
	// eventual utterance.id once committed. Stamped on every streamed
	// text-chunk's `replyId` field; reused as the utterance id at insert
	// time. One canonical string -> one bubble in the UI -> in-place
	// streaming-to-committed transition with no remount, no avatar
	// flicker, no duplicate. See composeReplyId for the contract.
	replyId := composeReplyId(ctx)

	llmStart := time.Now()
	// voiceStreamed = true once generateVoiceStreaming has dispatched
	// sentences to the bridge. The legacy single-shot TTS block below
	// is then skipped (otherwise the same response would TTS again).
	voiceStreamed := false
	if isVoice {
		c.Logger.Info("voice trace: agent llm start",
			"voiceTrace", utterance.ID,
			"stage", "cognition.agent.start",
			"partitionId", partitionId,
			"agentName", winner.AgentName,
			"routeMs", routeMs,
			"handlerOffsetMs", time.Since(start).Milliseconds(),
		)
	}
	if c.agentForwarder != nil {
		var fwd agentReplyResult
		fwd, err = c.forwardTurnToAgent(ctx, agent, "utterance",
			partitionId, winnerParticipant.ID, replyId, history, sInfo, attachmentSummaries,
			disableTools, allAgentPayloads, humanParticipants, utterance.ParticipantId,
			primaryDirective, isVoice)
		response = fwd.text
		responseCitations = fwd.citations
		responseRetrieved = fwd.retrieved
	} else if isVoice {
		// Voice path: resolve audio mode FIRST. If suppressed
		// (always_off / muted mirror_user), don't stream sentences
		// to the bridge -- just batch-generate so the chat reply
		// commits. Otherwise stream the agent LLM and dispatch each
		// sentence to the bridge as it completes; the bridge's
		// per-room queue serializes TTS + publish so audio flows
		// in sentence order while later tokens are still being
		// generated. Time-to-first-audio drops from "wait for full
		// reply + one TTS round-trip" (~3-4s) to "first sentence
		// boundary + streaming-TTS first chunk" (~1-1.5s).
		voiceMode := resolveAgentAudioMode(ctx, c, partitionId, winnerParticipant.AgentId, agent)
		if voiceMode == "mirror_user" {
			if c.allHumansMuted(ctx, partitionId) {
				voiceMode = "always_off"
			} else {
				voiceMode = "always_on"
			}
		}
		// A2 (#1198): when voice runs the agent tool loop, the realtime model
		// re-voices the authored reply -- cognition must NOT also dispatch cascade
		// TTS, or the turn would play twice. Author chat-only (the reply utterance
		// still lands in chat, which the relay returns as FinalText for the model
		// to voice). Flag off keeps today's cascade streaming behaviour.
		voiceStreamed = voiceMode != "always_off" && !voiceAgentToolLoopEnabled()
		if voiceStreamed {
			voiceModelResolved := resolveAgentVoice(agent)
			response, err = c.generateVoiceStreaming(ctx, agent, "utterance", partitionId,
				winner.AgentName, voiceModelResolved, participants, history, sInfo, attachmentSummaries)
			if err != nil {
				c.Logger.Warn("cognition: voice streaming failed; falling back to batch",
					"error", err, "partitionId", partitionId)
				voiceStreamed = false
				response, err = c.generateAIResponse(ctx, agent, "utterance", partitionId, participants, recentUtterances, history, sInfo, attachmentSummaries, allAgentPayloads...)
			}
		} else {
			// always_off -- chat-only, skip bridge dispatch.
			response, err = c.generateAIResponse(ctx, agent, "utterance", partitionId, participants, recentUtterances, history, sInfo, attachmentSummaries, allAgentPayloads...)
		}
	} else {
		result, streamErr := c.generateStreaming(ctx, agent, "utterance", partitionId, winnerParticipant.ID, replyId, participants, history, sInfo, attachmentSummaries)
		if streamErr != nil {
			c.Logger.Warn("cognition: streaming failed, falling back to sync", "error", streamErr)
			response, err = c.generateAIResponse(ctx, agent, "utterance", partitionId, participants, recentUtterances, history, sInfo, attachmentSummaries, allAgentPayloads...)
		} else {
			response = result.Text
		}
	}

	if err != nil {
		// If we already emitted an early acknowledgment, send an error follow-up.
		// Pass "" for replyId -- this is its own (non-streaming) utterance.
		if earlyTextInserted {
			_ = c.insertAIResponse(ctx, partitionId, winnerParticipant, "", utterance.ID,
				"Sorry, I ran into an issue while working on that. Let me know if you'd like me to try again.", nil, nil, nil)
		}
		_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateError, "Error", "Having trouble generating a reply.", utterance.ID, err.Error(), nil)
		c.Logger.Error("cognition: failed to generate AI response", "error", err)
		return
	}
	aiMs := time.Since(t0).Milliseconds()

	if isVoice {
		c.Logger.Info("voice trace: agent llm complete",
			"voiceTrace", utterance.ID,
			"stage", "cognition.agent.complete",
			"partitionId", partitionId,
			"agentName", winner.AgentName,
			"agentLlmMs", time.Since(llmStart).Milliseconds(),
			"responseChars", len(response),
		)
	}

	// Presence: responding. Voice turns get "Speaking…" so the
	// presence pill matches what the user is hearing (TTS audio
	// playing); text turns keep "Replying…" since chat readers see
	// text streaming in. Same `responding` state machine; just a
	// surfaced label difference.
	respondingLabel := "Replying…"
	if isVoice {
		respondingLabel = "Speaking…"
	}
	_ = c.upsertParticipantPresence(ctx, partitionId, winnerParticipant.ID, presenceStateResponding, respondingLabel, "", utterance.ID, "", nil)

	// Build response source metadata.
	var responseSource map[string]string
	if isVoiceUtterance(scoringUtterance.Source) {
		responseSource = map[string]string{
			"outputMethod": "tts",
			"pipeline":     "polyphon",
		}

		// If streaming already dispatched sentences to the bridge,
		// the audio is in flight. Don't fire the legacy single-shot
		// TTS path -- that would re-synthesize the entire response
		// and double-publish. The chat insertAIResponse below still
		// runs so the canonical text reply commits to the chat
		// transcript.
		if voiceStreamed {
			c.Logger.Info("voice trace: streaming dispatched -- skipping single-shot TTS",
				"voiceTrace", utterance.ID,
				"partitionId", partitionId,
				"agentName", winner.AgentName,
			)
		} else {
			// VOICE PATH: single TTS call for the full response.
			//
			// We previously split the response into sentences and POSTed
			// each one to /agent-response-sentence in a sequential loop.
			// Each POST blocked on the bridge's HandleAgentSentence (TTS
			// call + LiveKit publish), so the browser heard silence
			// between sentences while the bridge was mid-API-call on the
			// next one -- the "robotic / choppy" delivery the user
			// flagged. The intent was pipelining ("audio from sentence 1
			// plays while sentence 2 is being synthesized"), but because
			// cognition only splits AFTER the full LLM response is in
			// hand and then dispatches sequentially, we got N round-trip
			// costs and N-1 audible gaps with zero TTFT win.
			//
			// One TTS call per response -> one OGG-Opus stream -> smooth
			// playback. When real token-level streaming gets wired
			// through cognition (so the first sentence can synthesize
			// while later tokens are still being generated), per-sentence
			// dispatch comes back -- properly pipelined.

			// Resolve the winning agent's canonical voice to a provider
			// voice id before forwarding to the bridge. The agent record
			// stores a canonical name (alto, tenor, ...); the bridge
			// expects the provider-specific id (alloy, English-US-Male-1,
			// ...). resolvedVoiceModel is "" for legacy agents missing a
			// canonical voice -- voice.ResolveVoice falls back to the
			// provider default in that case so audio still plays.
			resolvedVoiceModel := resolveAgentVoice(agent)

			// Initiative C, Phase 11: Bridge Agent's TTS notify path was
			// deleted here. The Go voice-agent owns voice synthesis
			// now via the VoiceAgent* gRPC contract; cognition's job on
			// the voice path stops at landing the GA's reply utterance
			// in chat (which insertAIResponse does below). The voice-agent
			// subscribes to those rows + streams the prose to Aura-2.
			_ = resolvedVoiceModel
			_ = winnerParticipant
			_ = agent
		} // end !voiceStreamed
	}

	c.Logger.Info("cognition: AI response generated",
		"partitionId", partitionId,
		"replyToId", utterance.ID,
		"winner", winner.AgentName,
		"score", winner.TotalScore,
		"confidence", decision.Confidence,
		"aiMs", aiMs,
		"tookMs", time.Since(start).Milliseconds(),
	)

	// Post-response: fire-and-forget for independent operations.
	// The response is already delivered to the user. These run in background
	// goroutines so the handler returns immediately.
	bgCtx := context.WithoutCancel(ctx)

	go func() {
		// Use the replyId minted upstream so the committed utterance.id
		// matches the chunks' replyId -- the frontend keys one bubble
		// across the streaming/commit transition.
		if err := c.insertAIResponse(bgCtx, partitionId, winnerParticipant, replyId, utterance.ID, response, responseSource, responseCitations, responseRetrieved); err != nil {
			c.Logger.Error("cognition: failed to insert AI response (async)", "error", err, "partitionId", partitionId)
		}
	}()

	// Record that this primary agent contributed to the cycle. Without
	// this, the continuation re-invoke filter (filterAlreadySpoken-
	// FromContinuation) doesn't see the primary as already-spoken and
	// can re-dispatch them on the next iteration -- exactly the
	// "Nova told a joke twice" failure mode. The chime-in + sequence
	// chains already call RecordAgentSpoke at their end-of-iteration;
	// the primary path was the only gap.
	if conductorState := c.conductors.Get(partitionId); conductorState != nil && agent != nil {
		conductorState.RecordAgentSpoke(agent.ID)
	}

	// On VOICE turns the response text just landed in the chat but
	// the bridge is going to spend the next ~3-10s synthesizing TTS
	// and publishing audio. The presence-pill / speaking-highlight
	// on the frontend reads `responding`; flipping straight to
	// `idle` here would clear the pill the moment Sofia STARTS
	// talking, so her tile never shows the speaking highlight
	// during voice playback. Hold `responding` for a heuristic
	// playback window (~60ms/char ≈ 150 wpm) before idling. Text
	// turns idle immediately as before.
	go func() {
		if isVoice && len(response) > 0 {
			holdMs := len(response) * 60
			if holdMs < 1500 {
				holdMs = 1500
			}
			if holdMs > 30000 {
				holdMs = 30000
			}
			select {
			case <-bgCtx.Done():
				return
			case <-time.After(time.Duration(holdMs) * time.Millisecond):
			}
		}
		_ = c.upsertParticipantPresence(bgCtx, partitionId, winnerParticipant.ID, presenceStateIdle, "Idle", "", utterance.ID, "", nil)
	}()

	// Broadcast chime-in chain: when the user opens with a greeting
	// directed at the room (no @-mention, intent=greeting, 2+ other
	// agents in the space), fire additional agents to say hi too --
	// staggered by random delays so the chat feels like real
	// colleagues acknowledging in turn rather than a chorus.
	//
	// Distinct from the standard continuation goroutine below: that
	// path handles "next-best agent has score >= ContinuationThreshold
	// and adds value to the previous turn" cases (clarification
	// chains, follow-on contributions). This one handles the
	// "everyone in the room should chime in to a greeting" case,
	// which the standard continuation refuses because it doesn't
	// fit the score-threshold model. Broadcast chime-ins also skip
	// the MaxConsecutiveAgentTurns cap -- the user explicitly
	// invited everyone, so the cap (which exists to prevent
	// agent-only feedback loops) doesn't apply.
	// Decide post-primary dispatch. Three paths in priority order:
	//
	//   - Conductor plan with non-empty Sequence: ordered solo turns
	//     ("each of you / start with X / let's go around"). Each
	//     entry answers the user directly, NOT the primary. Run via
	//     runConductorSequence in array order.
	//   - Conductor plan with non-empty ChimeIns: parallel-style
	//     additions to the primary's response. Run via
	//     runConductorChimeInChain.
	//   - Conductor absent or empty: fall back to legacy score-gated
	//     chime-in chain (shouldFireChimeIns + runChimeInChain).
	//
	// Sequence and ChimeIns can both fire if the conductor declared
	// both (rare but valid: ordered solo turns followed by chime-ins
	// on the last one). The branch-point re-invoke (step 7) handles
	// continuation after the chain finishes.
	switch {
	case conductorPlan != nil && conductorPlan.HasSequence():
		go c.runConductorSequence(bgCtx, partitionId, utterance.ID,
			winnerParticipant.ID, conductorPlan, aiParticipants, agentConfigs, sInfo)
	case conductorPlan != nil && len(conductorPlan.ChimeIns) > 0:
		go c.runConductorChimeInChain(bgCtx, partitionId, utterance.ID,
			winnerParticipant.ID, conductorPlan, aiParticipants, agentConfigs, sInfo)
	case conductorPlan == nil:
		if fire, maxN, minScore := shouldFireChimeIns(scoringUtterance, candidates); fire {
			// Pass decision.Scores so the chain can apply the
			// per-agent score gate. candidates carry agent metadata
			// (name, role, tools, etc.); scores carry the polyphon
			// scorer's verdict on each one.
			go c.runChimeInChain(bgCtx, partitionId, utterance.ID,
				winnerParticipant.ID, decision.Scores, aiParticipants,
				agentConfigs, sInfo, maxN, minScore)
		} else {
			c.resetWaitingPresence(bgCtx, partitionId, utterance.ID, winnerParticipant.ID, aiParticipants)
		}
	default:
		// No post-primary dispatch (conductor present but produced no
		// sequence and no chime-ins). Reset Waiting peers to Idle so
		// the UI doesn't stick.
		c.resetWaitingPresence(bgCtx, partitionId, utterance.ID, winnerParticipant.ID, aiParticipants)
	}

	go func() {
		shouldContinue, nextAgent := c.scoreEngine.RecordAgentResponse(partitionId, winnerParticipant.ID, agent.Name, response, candidates)
		if !shouldContinue || nextAgent == nil {
			return
		}

		// Guard: only trigger if continuation agent is different from winner.
		if nextAgent.AgentId == winnerParticipant.ID {
			return
		}

		// Guard: respect MaxConsecutiveAgentTurns.
		session := c.scoreEngine.Sessions().Get(partitionId)
		if session != nil && session.AgentTurnsSinceHuman() >= 2 {
			c.Logger.Debug("cognition: continuation suppressed (max agent turns)",
				"partitionId", partitionId, "turns", session.AgentTurnsSinceHuman())
			return
		}

		// Delay for natural conversation flow. Cancel mid-delay if a
		// new human utterance arrives in this space -- continuing on
		// top of a fresh user message is the worst failure mode here
		// (the continuation agent is reacting to stale context the
		// new utterance has already moved past).
		select {
		case <-time.After(750 * time.Millisecond):
		case <-bgCtx.Done():
			return
		}
		// One more check before firing: did a newer human utterance
		// supersede us during the delay? recordLatestHumanUtterance
		// updates per-space; if we're no longer the latest, the new
		// handler will route the continuation if appropriate.
		if !c.isLatestHumanUtterance(partitionId, utterance.ID) {
			c.Logger.Debug("cognition: continuation cancelled (new utterance arrived)",
				"partitionId", partitionId, "utteranceId", utterance.ID,
				"continuationAgent", nextAgent.AgentName)
			return
		}

		c.Logger.Info("cognition: triggering continuation",
			"partitionId", partitionId,
			"continuationAgent", nextAgent.AgentName,
			"primaryAgent", winner.AgentName)

		// Find continuation agent's participant record.
		var contParticipant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == nextAgent.AgentId {
				contParticipant = ap
				break
			}
		}
		if contParticipant == nil {
			return
		}

		// Load agent config (from pre-loaded map or cache).
		contAgent := agentConfigs[contParticipant.ID]
		if contAgent == nil {
			contAgent, _ = c.getAgentCached(bgCtx, contParticipant.AgentId)
		}
		if contAgent == nil {
			return
		}

		// Set presence: continuation agent is thinking.
		_ = c.upsertParticipantPresence(bgCtx, partitionId, contParticipant.ID,
			presenceStateThinking, "Thinking...", "", utterance.ID, "", nil)

		// Generate continuation response.
		contResponse, contErr := c.generateAIResponse(bgCtx, contAgent, "continuation",
			partitionId, participants, recentUtterances, history, sInfo, attachmentSummaries, allAgentPayloads...)
		if contErr != nil {
			c.Logger.Warn("cognition: continuation response failed", "error", contErr, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, contParticipant.ID,
				presenceStateIdle, "Idle", "", utterance.ID, "", nil)
			return
		}

		// Insert continuation response. Non-streaming path -> pass "" so
		// insertAIResponse mints a fresh utterance id.
		if err := c.insertAIResponse(bgCtx, partitionId, contParticipant, "", utterance.ID, contResponse, nil, nil, nil); err != nil {
			c.Logger.Error("cognition: failed to insert continuation response", "error", err, "partitionId", partitionId)
		}

		_ = c.upsertParticipantPresence(bgCtx, partitionId, contParticipant.ID,
			presenceStateIdle, "Idle", "", utterance.ID, "", nil)

		c.Logger.Info("cognition: continuation response generated",
			"partitionId", partitionId,
			"continuationAgent", nextAgent.AgentName,
			"responseLen", len(contResponse))
	}()

	// Auto-embed AI response for future semantic context retrieval.
	if c.embedFunc != nil {
		go func() {
			embedCtx, embedCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer embedCancel()
			embedding, err := c.embedFunc(embedCtx, response, "embedding3Small")
			if err != nil {
				c.Logger.Debug("cognition: failed to embed response for context", "error", err)
				return
			}
			// Store the embedding for this response utterance.
			// The utterance ID for the response will be generated by insertAIResponse.
			// For now, just log success -- full storage requires the response utterance ID.
			_ = embedding
			c.Logger.Debug("cognition: response embedded for semantic context", "partitionId", partitionId, "embeddingDims", len(embedding))
		}()
	}

	// Auto-embed human utterance for semantic context retrieval.
	if c.embedFunc != nil {
		go func() {
			embedCtx, embedCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer embedCancel()
			_, err := c.embedFunc(embedCtx, text, "embedding3Small")
			if err != nil {
				c.Logger.Debug("cognition: failed to embed utterance for context", "error", err)
			}
		}()
	}

	// Trigger context compaction in background (debounced, max every 30s).
	go func() {
		session := c.scoreEngine.Sessions().Get(partitionId)
		if session == nil {
			return
		}
		cb := polyphon.NewContextBuilder()
		cb.TriggerCompaction(bgCtx, session, c.engine.InvokeAI)
	}()
}

// buildAgentCandidates converts cognition AI participants to Polyphon AgentCandidates.
func (c *CognitionIntegration) buildAgentCandidates(ctx context.Context, aiParticipants []*participantPayload) []polyphon.AgentCandidate {
	candidates := make([]polyphon.AgentCandidate, 0, len(aiParticipants))
	for _, ap := range aiParticipants {
		agent, err := c.getAgentCached(ctx, ap.AgentId)
		if err != nil || agent == nil {
			c.Logger.Warn("cognition: agent lookup failed",
				"participantId", ap.ID,
				"agentId", ap.AgentId,
				"error", err,
			)
			continue
		}

		// Platform-infrastructure agents (MemQL Planner, MemQL Trainer
		// and any other kind=="system" entries) must never be candidates
		// for utterance routing. They're invoked directly by the
		// planner service, not by the conductor / router. Defensive --
		// the seeds set autoJoin=false so they shouldn't reach this
		// pool anyway, but a stray join (manual add, future bug)
		// must not turn them into routing targets.
		if agent.Kind == "system" {
			continue
		}

		speakWhen := ""
		if agent.TriggerBehavior != nil {
			if sw, ok := agent.TriggerBehavior["speakWhen"].(string); ok {
				speakWhen = strings.TrimSpace(sw)
			}
		}
		if speakWhen == "" {
			speakWhen = "relevant"
		}

		var domains []string
		var keywords []string
		if agent.Capabilities != nil {
			if d, ok := agent.Capabilities["domains"].([]any); ok {
				for _, v := range d {
					if s, ok := v.(string); ok {
						domains = append(domains, s)
					}
				}
			}
			if k, ok := agent.Capabilities["keywords"].([]any); ok {
				for _, v := range k {
					if s, ok := v.(string); ok {
						keywords = append(keywords, s)
					}
				}
			}
		}

		// Expand domains with common synonyms for better keyword matching.
		expandedDomains := polyphon.ExpandDomains(domains)

		// Role: prefer the concept's declared role field (specialist vs
		// assistant) over the legacy description-parsed role. The
		// guardrail router keys off the enum value directly, so empty role
		// falls back to a description-derived label for the prompt only.
		role := strings.TrimSpace(agent.Role)
		if role == "" {
			role = parseAgentRole(agent.Description)
		}

		candidate := polyphon.AgentCandidate{
			ID:             ap.ID,
			ParticipantId:  ap.ID,
			Name:           agent.Name,
			Description:    agent.Description,
			Personality:    agent.Personality,
			Role:           role,
			Domains:        expandedDomains,
			Keywords:       keywords,
			SpeakWhen:      speakWhen,
			ProviderConfig: agent.ProviderConfig,
			Capabilities:   agent.Capabilities,
		}

		// Load or compute agent profile embedding for vector domain scoring.
		if c.embedFunc != nil {
			profileText := buildAgentProfileText(agent)
			if profileText != "" {
				candidate.ProfileEmbedding = c.getOrComputeProfileEmbedding(ctx, ap.ID, profileText)
			}
		}

		candidates = append(candidates, candidate)
	}
	return candidates
}

// deriveSelectionReason produces a short, human-readable explanation of why
// the cognition integration selected this agent. Used in the system prompt so the agent
// understands its role in the current turn.
func deriveSelectionReason(winner *polyphon.AgentScore) string {
	if winner == nil || len(winner.Factors) == 0 {
		return ""
	}
	// Find the highest-scoring factor.
	best := winner.Factors[0]
	for _, f := range winner.Factors[1:] {
		if f.Score > best.Score {
			best = f
		}
	}
	switch best.Name {
	case "direct_address":
		if best.Value >= 1.0 {
			return "You were directly addressed by name."
		}
		return "Your name was mentioned in the conversation."
	case "domain_relevance":
		return "You are the domain expert most relevant to this topic."
	case "conversational_thread":
		return "You are continuing an ongoing conversation with this participant."
	case "question_detection":
		return "A question or task was asked and you are the best fit to answer."
	case "continuation_relevance":
		return "You are following up on what another agent said."
	case "solo_agent_boost":
		return "You are the only agent in this space."
	case "si_router":
		return best.Detail // Already contains the AI's reasoning.
	default:
		return ""
	}
}

// handleHeartbeatAction dispatches proactive actions from the heartbeat evaluator.
// Currently handles re-engagement after extended silence. Other action types
// (notify, proactive) are logged as foundations for follow-up implementation.
func (c *CognitionIntegration) handleHeartbeatAction(ctx context.Context, partitionId string, action polyphon.HeartbeatAction, aiParticipants []*participantPayload) {
	switch action.Type {
	case "re-engage":
		// Find the agent participant record.
		var agentParticipant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == action.AgentId {
				agentParticipant = ap
				break
			}
		}
		if agentParticipant == nil {
			return
		}

		// Generate a gentle re-engagement message.
		c.Logger.Info("cognition: heartbeat re-engagement",
			"partitionId", partitionId,
			"agent", action.AgentName,
			"reason", action.Reason,
		)

		// FUTURE: Call generateAIResponse with trigger="silence_followup" to
		// produce a contextual re-engagement message. For now, log the intent.
		// The actual generation will be wired when the re-engagement prompt
		// template is created.

	case "notify":
		// FUTURE: Surface pending notifications via the appropriate agent.
		c.Logger.Debug("cognition: heartbeat notification (not yet implemented)",
			"partitionId", partitionId,
			"agent", action.AgentName,
			"reason", action.Reason,
		)

	case "proactive":
		// Reactive dispatch (phase 3): the heartbeat evaluator saw a
		// non-empty Prediction.SuggestedAction on the session. The
		// predictive analyzer populates that field when the
		// conversation embeds near a topic that one of the agents in
		// the room is configured to act on. We surface the activity
		// as an action utterance, set the agent's presence to
		// `researching`, and run a non-blocking topic-investigation
		// turn whose result posts back as a follow-up action
		// utterance. The chat thread continues unaffected -- this is
		// the "agents do background work while chatting" model.
		c.dispatchReactiveTopic(ctx, partitionId, action, aiParticipants)
	}
}

// dispatchReactiveTopic runs a background investigation for a single
// agent in response to a heartbeat-driven topic match. Posts an
// action utterance for the in-flight state, generates a result, and
// posts a second action utterance with the finding. All
// best-effort: failures log but do not affect the primary chat flow.
//
// The dispatch is gated by an in-memory de-dup map keyed on
// (partitionId, agentId, topic) so the same heartbeat tick doesn't fire
// the same investigation twice in a row, and so two consecutive
// ticks with the same prediction don't pile on.
func (c *CognitionIntegration) dispatchReactiveTopic(ctx context.Context, partitionId string, action polyphon.HeartbeatAction, aiParticipants []*participantPayload) {
	if c == nil || strings.TrimSpace(action.AgentId) == "" {
		return
	}
	topic := strings.TrimSpace(action.Reason)
	if topic == "" {
		topic = "the recent conversation thread"
	}

	// Locate the matched agent's participant record.
	var agentParticipant *participantPayload
	for _, ap := range aiParticipants {
		if ap.ID == action.AgentId {
			agentParticipant = ap
			break
		}
	}
	if agentParticipant == nil {
		c.Logger.Debug("cognition: reactive dispatch skipped (agent not in space)",
			"partitionId", partitionId, "agentId", action.AgentId)
		return
	}

	// De-dup: avoid stacking the same investigation if the heartbeat
	// fires the same prediction multiple ticks in a row.
	dedupKey := fmt.Sprintf("%s|%s|%s", partitionId, action.AgentId, topic)
	c.reactiveDispatchMu.Lock()
	if last, ok := c.reactiveDispatchAt[dedupKey]; ok && time.Since(last) < reactiveDispatchCooldown {
		c.reactiveDispatchMu.Unlock()
		return
	}
	c.reactiveDispatchAt[dedupKey] = time.Now()
	c.reactiveDispatchMu.Unlock()

	c.Logger.Info("cognition: reactive dispatch firing",
		"partitionId", partitionId,
		"agent", action.AgentName,
		"topic", topic,
	)

	go func() {
		bgCtx := contextWithSystemActor(context.Background())

		// Set presence to `researching` so the UI can show a quiet
		// background-activity indicator distinct from the typing
		// indicator. The chat lane stays available.
		_ = c.upsertParticipantPresence(bgCtx, partitionId, agentParticipant.ID,
			presenceStateResearching, "Researching…",
			fmt.Sprintf("Looking into %s", topic), "", "", nil)

		// Action utterance #1: in-flight ("X is investigating Y").
		notice := fmt.Sprintf("%s is looking into %s in the background.", action.AgentName, topic)
		if err := c.insertSystemActionUtterance(bgCtx, partitionId, agentParticipant.ID,
			"reactive_dispatch_start", notice, map[string]string{
				"agentName": action.AgentName,
				"topic":     topic,
			}); err != nil {
			c.Logger.Debug("cognition: reactive in-flight notice failed",
				"error", err, "partitionId", partitionId)
		}

		// Run a topic-investigation generation. We don't load the
		// full participants/utterance context here -- the topic +
		// agent identity are enough for the predictive prompt to
		// produce a focused finding. Tools are intentionally NOT
		// passed (this is a research/summary task, not an action
		// task); if the agent needs to act on the finding it'll do
		// so on its next conversational turn.
		agent, err := c.getAgentCached(bgCtx, agentParticipant.AgentId)
		if err != nil || agent == nil {
			c.Logger.Warn("cognition: reactive dispatch agent load failed",
				"error", err, "partitionId", partitionId, "agentId", agentParticipant.AgentId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, agentParticipant.ID,
				presenceStateIdle, "Idle", "", "", "", nil)
			return
		}

		// Build a minimal prompt for the investigation. Uses the
		// agentReply prompt with a synthetic trigger so the agent
		// answers in-character, not as a generic system message.
		// History is empty: the action utterance lane is for
		// background work, not a continuation of the chat thread.
		data := map[string]any{
			"trigger": "reactive_dispatch",
			"assistant": map[string]any{
				"name":         agent.Name,
				"id":           agent.ID,
				"description":  agent.Description,
				"personality":  agent.Personality,
				"role":         "specialist",
				"systemPrompt": "",
			},
			"space": map[string]any{
				"id": partitionId,
			},
			"history":         []map[string]any{},
			"selectionReason": fmt.Sprintf("background investigation of: %s", topic),
			"turnMode":        "answer",
		}

		result, invokeErr := c.engine.InvokeAI(bgCtx, "agentReply", data)
		_ = c.upsertParticipantPresence(bgCtx, partitionId, agentParticipant.ID,
			presenceStateIdle, "Idle", "", "", "", nil)

		if invokeErr != nil {
			c.Logger.Warn("cognition: reactive investigation failed",
				"error", invokeErr, "partitionId", partitionId, "agent", action.AgentName)
			return
		}

		// Extract reply text from the structured result.
		var reply string
		if m, ok := result.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				reply = t
			} else if t, ok := m["reply"].(string); ok {
				reply = t
			}
		}
		if s, ok := result.(string); ok {
			reply = s
		}
		reply = strings.TrimSpace(reply)
		if reply == "" {
			c.Logger.Debug("cognition: reactive investigation produced no text",
				"partitionId", partitionId, "agent", action.AgentName)
			return
		}

		// Truncate to a single short paragraph -- the action lane is
		// for headlines, not full essays. Long findings should
		// resurface as a normal chat reply if/when the user circles
		// back to the topic.
		if len(reply) > 600 {
			reply = reply[:580] + "…"
		}

		finding := fmt.Sprintf("%s — %s", action.AgentName, reply)
		if err := c.insertSystemActionUtterance(bgCtx, partitionId, agentParticipant.ID,
			"reactive_dispatch_result", finding, map[string]string{
				"agentName": action.AgentName,
				"topic":     topic,
			}); err != nil {
			c.Logger.Warn("cognition: reactive result notice failed",
				"error", err, "partitionId", partitionId)
			return
		}

		c.Logger.Info("cognition: reactive dispatch complete",
			"partitionId", partitionId, "agent", action.AgentName,
			"topic", topic, "replyLen", len(reply))
	}()
}

// reactiveDispatchCooldown is the minimum time between firings of the
// same (space, agent, topic) reactive dispatch. Prevents two
// consecutive heartbeat ticks with the same prediction from
// stacking duplicate investigations.
const reactiveDispatchCooldown = 90 * time.Second

// parseAgentRole extracts the agent role from the structured description.
// Description format: "{Gender} {RoleLabel} specialist -- {Styles}"
// Returns the role slug (e.g., "assistant") or empty string if not parsable.
func parseAgentRole(description string) string {
	d := strings.TrimSpace(description)
	if d == "" {
		return ""
	}
	// Map of role labels (as they appear in descriptions) to role slugs.
	roleMap := map[string]string{
		"assistant":                "assistant",
		"accounting & finance":     "accounting-finance",
		"data & analytics":         "data-analytics",
		"engineering & technology": "engineering-technology",
		"human resources":          "human-resources",
		"legal & compliance":       "legal-compliance",
		"marketing & branding":     "marketing-branding",
		"operations & logistics":   "operations-logistics",
		"product management":       "product-management",
		"sales & business dev":     "sales-business-dev",
		"customer success":         "customer-success",
		"creative & design":        "creative-design",
	}
	dl := strings.ToLower(d)
	for label, slug := range roleMap {
		if strings.Contains(dl, strings.ToLower(label)+" specialist") {
			return slug
		}
	}
	return ""
}

// getParticipantDisplayName returns the display name for a participant.
func (c *CognitionIntegration) getParticipantDisplayName(ctx context.Context, participantId string) string {
	if c == nil || c.engine == nil || strings.TrimSpace(participantId) == "" {
		return ""
	}
	query := "concept==v1:cognition:participant;id==\"" + participantId + "\""
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return ""
	}
	payload, err := extractPayloadFromResult(result, "participant")
	if err != nil {
		return ""
	}
	name, _ := payload["displayName"].(string)
	return strings.TrimSpace(name)
}

// getParticipantType resolves the participantType field for a given participant ID.
func (c *CognitionIntegration) getParticipantType(ctx context.Context, participantId string) (string, error) {
	if strings.TrimSpace(participantId) == "" {
		return "", fmt.Errorf("participantId is empty")
	}
	query := fmt.Sprintf(`concept==v1:cognition:participant;id=="%s"`, participantId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return "", err
	}
	payload, err := extractPayloadFromResult(result, "participant")
	if err != nil {
		return "", err
	}
	if pt, ok := payload["participantType"].(string); ok {
		return strings.TrimSpace(pt), nil
	}
	if p2, ok := payload["payload"].(map[string]any); ok {
		if pt, ok := p2["participantType"].(string); ok {
			return strings.TrimSpace(pt), nil
		}
	}
	return "", nil
}

// isTranscriptOnlyRealtimeUtterance returns true when the utterance source indicates
// it was transcribed from a realtime voice channel and should NOT trigger a text-based AI response.
func isTranscriptOnlyRealtimeUtterance(source map[string]string) bool {
	if len(source) == 0 {
		return false
	}
	if strings.TrimSpace(source["inputMethod"]) == "realtimeVoice" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(source["transcriptOnly"]), "true")
}

// findAllAIParticipants returns all active AI participants in a space.
// Delegates to the spaceParticipants MemQL query function.
func (c *CognitionIntegration) findAllAIParticipants(ctx context.Context, partitionId string) ([]*participantPayload, error) {
	return c.findParticipantsByType(ctx, partitionId, "si")
}

// findActiveHumanParticipants returns all active human participants in a space.
// Used for @mention resolution so the router can distinguish "@Stella" (agent
// addressee -> agent responds) from "@Alice" (human addressee -> AI silent).
func (c *CognitionIntegration) findActiveHumanParticipants(ctx context.Context, partitionId string) ([]*participantPayload, error) {
	return c.findParticipantsByType(ctx, partitionId, "human")
}

// findParticipantsByType is the shared implementation behind the AI / human
// participant lookups. participantType should be "si" or "human".
func (c *CognitionIntegration) findParticipantsByType(ctx context.Context, partitionId, participantType string) ([]*participantPayload, error) {
	query := fmt.Sprintf(`query spaceParticipants(partitionId: "%s", participantType: "%s", status: "active")`, partitionId, participantType)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, nil
	}
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return nil, nil
		}
	}
	var nodes []any
	if n, ok := bundle["nodes"].([]any); ok {
		nodes = n
	} else if n, ok := bundle["Nodes"].([]any); ok {
		nodes = n
	}
	out := make([]*participantPayload, 0, len(nodes))
	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		// Defensive concept filter. The spaceParticipants shape is
		// declared @concepts("v1:cognition:participant") so in principle
		// this is redundant, but in practice the shape runtime has been
		// observed returning agent and human-participant nodes from a
		// filtered AI query -- stripping them here prevents phantom
		// "agentId is empty" lookup warnings and wrong router decisions.
		if concept, _ := node["concept"].(string); concept != "" && concept != "v1:cognition:participant" {
			continue
		}
		nodeId, _ := node["id"].(string)
		if nodeId == "" {
			nodeId, _ = node["Id"].(string)
		}
		payload, ok := node["payload"].(map[string]any)
		if !ok {
			payload, ok = node["Payload"].(map[string]any)
			if !ok {
				continue
			}
		}
		var part participantPayload
		if err := mapToStruct(payload, &part); err != nil {
			continue
		}
		// Require the payload to actually match the requested type. Same
		// defensive reasoning as the concept filter above.
		if part.ParticipantType != participantType {
			continue
		}
		part.ID = nodeId
		out = append(out, &part)
	}
	return out, nil
}

// isVoiceUtterance returns true when the utterance source indicates
// it was produced by a realtime voice channel. Two pipelines tag
// here today: the legacy /memql/audio WebSocket path (still backing
// voice-first creation modals) stamps `pipeline=polyphon`, and the
// Go voice-agent path (the in-session voice channel)
// stamps `pipeline=voice-agent`. inputMethod is also checked as a
// belt-and-suspenders catch for STT producers that omit pipeline.
func isVoiceUtterance(source map[string]string) bool {
	if len(source) == 0 {
		return false
	}
	pipeline := strings.TrimSpace(source["pipeline"])
	if pipeline == "polyphon" || pipeline == "voice-agent" {
		return true
	}
	switch strings.TrimSpace(source["inputMethod"]) {
	case "stt", "realtimeVoice":
		return true
	}
	return false
}

// resolveAgentAudioMode resolves the effective TTS publication mode
// for the winning agent in the current space. Order:
//
//  1. Active per-(space, agent) override on
//     v1:cognition:audioOverride (active=true wins).
//  2. Agent default on payload.audioControl.
//  3. "always_off" fallback for legacy rows that predate the field.
//
// Returns one of "always_on" | "always_off" | "mirror_user". The
// caller treats "always_off" as the suppression signal; the other
// two fall through to publication today (mirror_user becomes
// mic-state-aware in a follow-up).
func resolveAgentAudioMode(ctx context.Context, c *CognitionIntegration, partitionId, agentId string, agent *agentPayload) string {
	override := c.lookupAudioOverride(ctx, partitionId, agentId)
	if override != "" {
		return override
	}
	if agent != nil {
		mode := strings.TrimSpace(agent.AudioControl)
		if mode == "always_on" || mode == "always_off" || mode == "mirror_user" {
			return mode
		}
	}
	return "always_off"
}

// allHumansMuted returns true when every human in the space has a
// recent v1:cognition:micState row marking them muted (or no row at
// all). Drives the mirror_user audio mode: when nobody is talking
// out loud, mirror_user agents respond text-only; when at least one
// human has their LiveKit mic open, the agent speaks aloud.
//
// Stale rows older than micStateStaleAfter are ignored -- a user
// who closed the tab without untoggling shouldn't pin every agent
// silent forever. The next user mic event will write a fresh row.
func (c *CognitionIntegration) allHumansMuted(ctx context.Context, partitionId string) bool {
	if c == nil || c.engine == nil || strings.TrimSpace(partitionId) == "" {
		return false
	}
	q := fmt.Sprintf(`query queryUserMicStatesForSpace(partitionId: %q)`, partitionId)
	res, err := c.engine.Execute(ctx, q)
	if err != nil {
		c.Logger.Debug("cognition: mic state lookup failed (non-fatal)", "error", err, "partitionId", partitionId)
		// On query failure default to NOT-all-muted so we don't
		// accidentally silence the agent. The user reading the
		// response in chat is the safety net either way.
		return false
	}
	rows, ok := res.([]any)
	if !ok || len(rows) == 0 {
		// No micState rows yet means nobody has toggled in this
		// space. Default to "not all muted" so the very first
		// agent turn after a fresh space create still publishes
		// audio when audio is the user's intended modality.
		return false
	}
	cutoff := time.Now().Add(-micStateStaleAfter)
	anyFresh := false
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		payload, _ := m["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		muted, _ := payload["muted"].(bool)
		updated := parseMicStateUpdatedAt(payload["updatedAt"])
		if !updated.IsZero() && updated.Before(cutoff) {
			continue
		}
		anyFresh = true
		if !muted {
			return false
		}
	}
	// No fresh rows at all -> treat as "no signal", not-all-muted.
	if !anyFresh {
		return false
	}
	return true
}

// micStateStaleAfter is the window past which a mic state row is
// ignored. 60s is long enough to absorb a tab refresh / brief
// disconnect and short enough that an abandoned session doesn't
// silence agents indefinitely.
const micStateStaleAfter = 60 * time.Second

// parseMicStateUpdatedAt parses the payload.updatedAt field from a
// queryUserMicStatesForSpace row. Tolerates both time.Time and
// RFC3339 string shapes.
func parseMicStateUpdatedAt(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return time.Time{}
		}
		return parsed
	}
	return time.Time{}
}

// lookupAudioOverride returns the active mode for the given (space,
// agent) tuple, or "" if no row exists. Falls back to "" on query
// errors so the agent default still applies -- the gating layer
// must never silently default to a strict-suppress on transient
// query failures.
func (c *CognitionIntegration) lookupAudioOverride(ctx context.Context, partitionId, agentId string) string {
	if c == nil || c.engine == nil || strings.TrimSpace(partitionId) == "" || strings.TrimSpace(agentId) == "" {
		return ""
	}
	q := fmt.Sprintf(`query audioOverridesForSpace(partitionId: %q)`, partitionId)
	res, err := c.engine.Execute(ctx, q)
	if err != nil {
		c.Logger.Debug("cognition: audio override lookup failed (non-fatal)", "error", err, "partitionId", partitionId)
		return ""
	}
	rows, ok := res.([]any)
	if !ok || len(rows) == 0 {
		return ""
	}
	target := strings.ToLower(strings.TrimSpace(agentId))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		payload, _ := m["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		rowAgent, _ := payload["agentId"].(string)
		if !sameAgent(rowAgent, target) {
			continue
		}
		mode, _ := payload["mode"].(string)
		mode = strings.TrimSpace(mode)
		if mode == "always_on" || mode == "always_off" || mode == "mirror_user" {
			return mode
		}
	}
	return ""
}

// sameAgent compares an agent id from a query result against a
// target id, tolerating bare slug vs canonical full-id differences.
func sameAgent(rowAgentId, targetAgentId string) bool {
	row := strings.ToLower(strings.TrimSpace(rowAgentId))
	target := strings.ToLower(strings.TrimSpace(targetAgentId))
	if row == "" || target == "" {
		return false
	}
	if row == target {
		return true
	}
	if strings.HasSuffix(row, ":"+target) || strings.HasSuffix(target, ":"+row) {
		return true
	}
	return false
}

// resolveAgentVoice extracts the canonical voice name from an agent's
// providerConfig.voice.voiceId field, normalises the active TTS
// provider name, and resolves both into the provider-specific voice
// id the Bridge Agent expects. Agents without a canonical voice
// (legacy rows or migrating agents) return "" -- the bridge then
// falls back to its hardcoded default voice for the active provider.
func resolveAgentVoice(agent *agentPayload) string {
	if agent == nil {
		return ""
	}
	canonical := canonicalVoiceFromAgent(agent)
	if canonical == "" {
		return ""
	}
	// voice.ActiveProvider applies the same MEMQL_POLYPHON_VOICE_PROVIDER
	// default rule the bridge-agent uses (see cmd/bridge-agent/main.go's
	// initVoiceProviders). Hardcoding "openai" here is a bug -- a bridge
	// on a different provider gets sent voice ids it can't synthesize
	// and every TTS call 400s.
	return voice.ResolveVoice(canonical, voice.ActiveProvider())
}

// canonicalVoiceFromAgent reads providerConfig.voice.voiceId from the
// agent payload, tolerating both the engine's nested-map JSON
// round-trip shape and the partially-typed map-of-any shape.
func canonicalVoiceFromAgent(agent *agentPayload) string {
	if agent == nil || agent.ProviderConfig == nil {
		return ""
	}
	rawVoice, ok := agent.ProviderConfig["voice"]
	if !ok {
		return ""
	}
	voiceMap, ok := rawVoice.(map[string]any)
	if !ok {
		return ""
	}
	voiceId, _ := voiceMap["voiceId"].(string)
	return strings.TrimSpace(voiceId)
}

// mustJSON marshals v to JSON. Panics on error (should only be used for known-good values).
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// polyphonDirectAddressWinner returns the Polyphon winner when the
// scoring decision contains a `direct_address` factor with value==1.0
// (primary address: name at the start of the utterance, or greeting +
// name like "hi pearl"). Returns nil otherwise.
//
// The returned *polyphon.AgentScore is guaranteed to correspond to an
// entry in `candidates` -- we re-validate the ID here because the
// winner's Factors sometimes outlive the candidate set (transient
// cache staleness at session start).
func polyphonDirectAddressWinner(decision *polyphon.ScoreDecision, candidates []polyphon.AgentCandidate) *polyphon.AgentScore {
	if decision == nil || decision.Winner == nil {
		return nil
	}
	w := decision.Winner
	var hasPrimaryAddress bool
	for _, f := range w.Factors {
		if f.Name == "direct_address" && f.Value >= 1.0 {
			hasPrimaryAddress = true
			break
		}
	}
	if !hasPrimaryAddress {
		return nil
	}
	for _, cand := range candidates {
		if cand.ID == w.AgentId {
			return w
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Multi-agent chime-in chain
// ---------------------------------------------------------------------
//
// The single-winner conductor model picks one agent per human turn.
// That's the right shape for most utterances -- a specialist
// question, a clarification answer -- but it's wrong for any
// utterance where multiple agents in the room have something
// genuine to add. The chime-in chain runs AFTER the primary
// winner's response and fires up to N additional peers with
// staggered delays (3-7s between) so the chat reads like
// colleagues participating in turn, not a single representative
// speaking for the team.
//
// Two-mode policy lives in shouldFireChimeIns:
//
//   - Greetings to the room (no @-mention, 4+ agents OR an
//     explicit collective-addressing word): broad participation.
//     Up to `maxGreetingChimeIns` peers, no per-agent score gate
//     -- anyone in the room can return a "hi".
//
//   - Any other intent (DomainQuestion, TaskRequest, etc.) on an
//     un-addressed utterance with 3+ agents present: narrow
//     participation. Up to `maxConversationalChimeIns` peers, but
//     each peer must clear `minConversationalChimeInScore` (matches
//     Polyphon's `ResponseThreshold`) so only agents that ALSO had
//     a real fit chime in. The polyphon scorer's existing 6-factor
//     evaluation -- DomainRelevance, ContinuationRelevance, etc. --
//     is what decides "does this agent have something to add?"
//     We don't pattern-match on the utterance text; the scorer is
//     the source of truth.
//
//   - Affirmations + Follow-ups ("ok", "thanks", "yes") + @-addressed
//     turns: skip the chain. Piling on those reads as noise; the
//     user already chose the audience.
//
// Each chime-in:
//   - waits a random delay between minDelay and maxDelay (the
//     spacing varies per peer so it feels organic)
//   - aborts mid-delay if a new human utterance arrives in the
//     same space
//   - generates with trigger="chimein" so the prompt knows it's a
//     follow-on contribution (brief, additive, don't repeat the
//     primary response)
//   - posts as utteranceType="text" (a normal chat message,
//     visible inline -- chime-ins ARE the conversation, distinct
//     from the action-utterance lane used for background work)

const (
	// Greetings to the room: broad participation, no score gate.
	maxGreetingChimeIns     = 3
	minGreetingChimeInScore = 0.0

	// Other turns: narrow participation, must clear ResponseThreshold.
	maxConversationalChimeIns     = 2
	minConversationalChimeInScore = 30.0 // matches polyphon TurnPolicyConfig.ResponseThreshold

	chimeInMinDelay = 3 * time.Second
	chimeInMaxDelay = 7 * time.Second
)

// userAddressedRoom returns true if the utterance text contains a
// collective-addressing phrase ("everyone", "all of you", "you guys",
// etc.). Shared between shouldFireChimeIns (decides whether to fan
// out) and BuildDirective (sets SkipHandoffOpener so the chosen agent
// doesn't open with "Thanks Sofia, I'll take this one" when the user
// invited the room).
func userAddressedRoom(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, w := range collectiveAddressingWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// collectiveAddressingWords are phrases that signal "audience is the
// room" rather than a specific agent. Single source of truth used by
// userAddressedRoom + shouldFireChimeIns.
var collectiveAddressingWords = []string{
	"everyone", "everybody", "y'all", "yall", "you all", "all of you",
	"all of us", "team", "to all", "from all",
	"you guys", "you folks", "you all think", "you guys think",
	"what do you all", "what does the room", "input from you", "input from all",
	// Greeting + collective forms. Plain "all" alone is too noisy
	// (matches "call", "small", "all the time", etc.); these are the
	// common multi-agent greeting patterns.
	"hello all", "hi all", "hey all", "hello team", "hi team", "hey team",
	"good morning all", "good morning everyone", "good morning team",
	"good afternoon all", "good evening all",
	"hi folks", "hello folks", "hey folks",
}

// shouldFireChimeIns returns whether to attempt the chime-in chain
// for this utterance and the parameters that govern it. Policy is
// intent-FIRST, addressing-second:
//
//  1. Skip intents (Affirmation, FollowUp, Farewell) and @-mentions
//     short-circuit out -- piling on those reads as noise.
//  2. Greeting intent + (collective addressing OR 4+ agent room)
//     -- the user is opening the floor. Broad participation, no
//     score gate.
//  3. Anything else (DomainQuestion, TaskRequest,
//     CapabilityQuestion, etc.) -- narrow participation, score-
//     gated. Even when the user phrases the question collectively
//     ("can you guys see X"), only peers with real fit chime in;
//     a chorus of "we can't" or three identical takes is exactly
//     the failure mode this gate prevents.
//
// The earlier version short-circuited to broad on collective
// addressing alone, which made every "you guys"-shaped question
// pile on three near-identical replies. Collective addressing now
// nudges (path 2) but does NOT override the score gate (path 3).
func shouldFireChimeIns(utterance polyphon.Utterance, candidates []polyphon.AgentCandidate) (fire bool, maxN int, minScore float64) {
	// Need at least 2 candidates beyond the primary winner.
	if len(candidates) < 3 {
		return false, 0, 0
	}
	// @-mentions narrow the audience -- the user picked a specific
	// recipient. Chiming in over their choice would feel like
	// piling on.
	if len(utterance.Mentions) > 0 {
		return false, 0, 0
	}

	// (1) Skip intents where additional voices feel like piling on.
	// Affirmations ("ok", "thanks"), follow-ups (short replies
	// after an agent turn), and farewells are micro-turns; chime-
	// ins make them awkward. The user can still get peer input
	// by asking a real question.
	if utterance.Intent != nil {
		switch utterance.Intent.Primary {
		case polyphon.IntentAffirmation, polyphon.IntentFollowUp, polyphon.IntentFarewell:
			return false, 0, 0
		}
	}

	collective := userAddressedRoom(utterance.Text)
	intent := polyphon.IntentType("")
	if utterance.Intent != nil {
		intent = utterance.Intent.Primary
	}

	// (2) Greeting broadcast: collective greeting ("hello all", "hi
	// everyone") OR plain greeting in a 4+ agent room. Broad
	// participation -- the user is inviting the room.
	if intent == polyphon.IntentGreeting {
		if collective || len(candidates) >= 4 {
			return true, maxGreetingChimeIns, minGreetingChimeInScore
		}
		// Greeting in a small room without collective phrasing:
		// primary only.
		return false, 0, 0
	}

	// (3) Conversational chime-in: any other intent (including
	// collectively-phrased questions like "can you guys see X").
	// Per-agent score gate inside runChimeInChain limits this to
	// turns where a peer has real fit (matches Polyphon's
	// ResponseThreshold). If nobody else scores high enough the
	// chain fires zero chime-ins -- which is correct: a chorus on
	// a specific question is exactly what we don't want.
	return true, maxConversationalChimeIns, minConversationalChimeInScore
}

// runChimeInChain fires a sequence of additional agents to chime in
// after the primary winner's response. Runs in its own goroutine;
// best-effort throughout (failures log but don't escalate).
//
// maxN caps the number of chime-ins; minScore is the per-agent
// TotalScore threshold below which a peer is skipped (used for the
// non-greeting branch's "must have real fit" gate). Both come from
// shouldFireChimeIns.
//
// The function takes []AgentScore (the scorer's verdict for each
// agent on this utterance) rather than []AgentCandidate so the
// per-agent score gate has the right input. Scores are pre-sorted
// by TotalScore descending by the polyphon engine, so the iteration
// order is "next-best fit first."
func (c *CognitionIntegration) runChimeInChain(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	primaryWinnerParticipantId string,
	scores []polyphon.AgentScore,
	aiParticipants []*participantPayload,
	agentConfigs map[string]*agentPayload,
	sInfo spaceInfo,
	maxN int,
	minScore float64,
) {
	if c == nil || maxN <= 0 {
		return
	}

	// Build the chime-in roster: agents other than the primary
	// winner, ordered by score descending. Skip any agent whose
	// TotalScore is below minScore -- in greeting mode minScore is
	// 0 so this is a no-op; in conversational mode it filters down
	// to peers with genuine fit (matching Polyphon's ResponseThreshold).
	type chimeIn struct {
		participantId string
		agent         *agentPayload
		name          string
		score         float64
		quiet         bool // true if agent hasn't spoken this cycle
	}

	// Pull conductor state once for the quiet-agent bias check below.
	conductorState := c.conductors.Get(partitionId)

	// First pass: collect every eligible candidate (excluding the
	// primary winner + agents below the score gate). Tag each with
	// whether they've already spoken this cycle.
	all := make([]chimeIn, 0, len(scores))
	for _, sc := range scores {
		if sc.AgentId == primaryWinnerParticipantId {
			continue
		}
		if sc.TotalScore < minScore {
			continue
		}
		var participant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == sc.AgentId {
				participant = ap
				break
			}
		}
		if participant == nil {
			continue
		}
		ag := agentConfigs[participant.ID]
		if ag == nil {
			ag, _ = c.getAgentCached(bgCtx, participant.AgentId)
		}
		if ag == nil {
			continue
		}
		quiet := true
		if conductorState != nil {
			quiet = !conductorState.AgentSpokeThisCycle(ag.ID)
		}
		all = append(all, chimeIn{
			participantId: participant.ID,
			agent:         ag,
			name:          ag.Name,
			score:         sc.TotalScore,
			quiet:         quiet,
		})
	}

	// Quiet-agent bias: sort so agents who haven't spoken this cycle
	// come first, then by score descending. Stable sort preserves
	// the score ordering within each tier. Result: a peer who's been
	// silent through three turns gets dispatch priority over one
	// who chimed in last turn, even if the latter scored slightly
	// higher this turn. Real rooms work this way too -- if Marketing
	// has been quiet for 5 minutes, give them the floor.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].quiet != all[j].quiet {
			return all[i].quiet // quiet=true sorts first
		}
		return all[i].score > all[j].score
	})

	// Second pass: cap at maxN.
	chimeIns := make([]chimeIn, 0, maxN)
	for _, item := range all {
		if len(chimeIns) >= maxN {
			break
		}
		chimeIns = append(chimeIns, item)
	}
	if len(chimeIns) == 0 {
		return
	}

	c.Logger.Info("cognition: chime-in chain starting",
		"partitionId", partitionId,
		"chimeInCount", len(chimeIns),
		"maxN", maxN,
		"minScore", minScore,
		"primaryWinner", primaryWinnerParticipantId)

	// Track how many chime-ins actually persisted (vs. just attempted).
	// The chain-complete log uses this rather than len(chimeIns) so
	// "fired=N" reports successful inserts, not the roster size.
	successfulFires := 0

	// Random source for delay variation. Each chime-in's delay is
	// drawn separately so the spacing varies (some agents are
	// faster, some slower -- feels organic).
	for i, ci := range chimeIns {
		// Stagger delay: a randomised value between the min and
		// max bounds. The first chime-in waits the standard amount
		// after the primary; subsequent chime-ins each wait their
		// own delay, so total elapsed time grows roughly linearly.
		delay := chimeInMinDelay +
			time.Duration(rand.Int63n(int64(chimeInMaxDelay-chimeInMinDelay)))
		select {
		case <-time.After(delay):
		case <-bgCtx.Done():
			return
		}

		// Abort the chain if a new human utterance arrived during
		// the delay. Continuing on top of fresh user input is the
		// worst failure mode here -- the chime-ins would be
		// reacting to stale context the user has already moved
		// past.
		if !c.isLatestHumanUtterance(partitionId, originatingUtteranceId) {
			c.Logger.Debug("cognition: chime-in chain aborted (new utterance)",
				"partitionId", partitionId, "remainingChimeIns", len(chimeIns)-i)
			return
		}

		c.Logger.Info("cognition: chime-in firing",
			"partitionId", partitionId,
			"chimeInIdx", i,
			"agent", ci.name,
			"score", ci.score,
			"delay", delay.String())

		// Set presence: the chime-in agent is thinking briefly.
		_ = c.upsertParticipantPresence(bgCtx, partitionId, ci.participantId,
			presenceStateThinking, "Thinking…", "", originatingUtteranceId, "", nil)

		// Generate the chime-in. Trigger="chimein" tells the prompt
		// this is a follow-on contribution, not a primary response;
		// the agentReply template can branch on it (or fall through
		// to default behavior, which produces a reasonable result
		// thanks to the conversation history already including the
		// primary response and the user's utterance).
		//
		// Reuse the standard prompt context build via the same
		// caches the primary turn used. Latency is hidden by the
		// staggered delay anyway.
		participants, _ := c.getParticipantsForPromptCached(bgCtx, partitionId)
		recentUtterances, _ := c.getRecentUtterancesForPromptCached(bgCtx, partitionId, 20)
		history := buildHistoryFromRecentUtterances(recentUtterances, ci.participantId, participants)
		var allAgentPayloads []*agentPayload
		for _, ap := range aiParticipants {
			if ag, ok := agentConfigs[ap.ID]; ok && ag != nil {
				allAgentPayloads = append(allAgentPayloads, ag)
			}
		}

		// Build a chime-in directive so the agent's prompt knows
		// (a) it's a chime-in, not a primary turn,
		// (b) brevity is short,
		// (c) skip the takeover opener (the user invited the room),
		// (d) skip self-intro (the room already heard from peers
		//     this turn -- chiming in with another self-intro is
		//     noise),
		// (e) skip room-announce (always; user has the panel).
		// generateAIResponse uses context to receive it.
		chimeInDirective := BuildDirective(
			conductorStateOrNil(c, partitionId),
			ci.agent.ID,
			false, // isPrimaryWinner
			true,  // isChimeIn
			"",    // no specific handoff for chime-ins
			true,  // userAddressedRoom -- chime-ins fire when the audience is the room
			c.queryAgentIsKnownToUser(bgCtx, ci.agent.ID),
		)
		// Differentiated content hint per chime-in: derive the
		// agent's distinctive angle from their role + domains so
		// each peer's contribution covers a different facet. Without
		// this, three chime-ins on a generic ask all produce
		// near-identical "Happy to help" filler. The conductor
		// pre-decides each peer's slot here so the prompt doesn't
		// have to figure it out from history alone.
		chimeInDirective.ContentHint = chimeInContentAngle(ci.agent)
		chimeInCtx := contextWithDirective(bgCtx, chimeInDirective)

		response, err := c.generateAIResponse(chimeInCtx, ci.agent, "chimein",
			partitionId, participants, recentUtterances, history, sInfo, nil, allAgentPayloads...)
		if err != nil {
			c.Logger.Warn("cognition: chime-in generation FAILED -- this is why peers don't appear in chat despite firing",
				"error", err, "agent", ci.name, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, ci.participantId,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}
		if strings.TrimSpace(response) == "" {
			c.Logger.Warn("cognition: chime-in generation returned EMPTY -- prompt rendered but produced no text",
				"agent", ci.name, "partitionId", partitionId,
				"reason", "model returned empty completion or prompt was empty")
			_ = c.upsertParticipantPresence(bgCtx, partitionId, ci.participantId,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}

		// Post the chime-in.
		participantPayloadForCi := &participantPayload{
			ID:      ci.participantId,
			AgentId: ci.agent.ID,
		}
		if insertErr := c.insertAIResponse(bgCtx, partitionId, participantPayloadForCi, "", originatingUtteranceId, response, nil, nil, nil); insertErr != nil {
			c.Logger.Warn("cognition: chime-in insert failed",
				"error", insertErr, "agent", ci.name, "partitionId", partitionId)
		} else {
			successfulFires++
		}
		_ = c.upsertParticipantPresence(bgCtx, partitionId, ci.participantId,
			presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)

		// Record this agent's contribution in the conductor state so
		// the next dispatch (this turn or next cycle) can see who's
		// already participated. Phase 2 uses this to skip agents
		// who've already chimed in.
		if conductorState := c.conductors.Get(partitionId); conductorState != nil {
			conductorState.RecordAgentSpoke(ci.agent.ID)
		}
	}

	c.Logger.Info("cognition: chime-in chain complete",
		"partitionId", partitionId,
		"rosterSize", len(chimeIns),
		"successfulFires", successfulFires)
}

// chimeInContentAngle produces a content hint that nudges a chime-in
// agent toward their distinctive angle on the conversation. Without
// this, multiple chime-ins on a generic ask tend to produce near-
// identical filler ("Happy to help, what do you need?"). The hint
// is short and prompt-shaped: "give the X angle / perspective / take",
// derived from the agent's role + first-listed domain.
//
// Heuristic, not LLM-driven: the role slug + domain string are
// already curated when the agent is created, so a one-liner template
// gives the model a focused instruction without an extra LLM call
// per chime-in. Phase 4+ can swap this for an LLM-generated angle
// if the heuristic feels stale.
func chimeInContentAngle(agent *agentPayload) string {
	if agent == nil {
		return ""
	}
	// Prefer role-slug-based phrasing (specialist roles map cleanly
	// to perspective phrasing). Fall back to first-listed domain.
	role := strings.TrimSpace(agent.Role)
	switch role {
	case "assistant":
		return "give the general / orienting angle -- what's the high-level take, not the specialist deep-dive"
	case "it-support", "engineering-technology":
		return "give the IT / technical angle"
	case "human-resources":
		return "give the HR / people-side angle"
	case "marketing-branding":
		return "give the marketing / brand-side angle"
	case "sales-business-dev":
		return "give the sales / business-development angle"
	case "operations-logistics":
		return "give the operations / process-side angle"
	case "accounting-finance":
		return "give the finance / numbers-side angle"
	case "legal-compliance":
		return "give the legal / compliance-side angle"
	case "product-management":
		return "give the product-side angle"
	case "data-analytics":
		return "give the data / analytics angle"
	case "customer-success":
		return "give the customer / retention-side angle"
	case "creative-design":
		return "give the design / creative-side angle"
	}
	// No role match -- use the first declared domain if any.
	domains := agent.domains()
	if len(domains) > 0 {
		return fmt.Sprintf("give your %s perspective -- specifically, what's distinct about how someone in %s would see this", strings.TrimSpace(domains[0]), strings.TrimSpace(domains[0]))
	}
	// Last resort: a generic differentiation nudge.
	return "give your distinct take -- something the primary speaker didn't already cover"
}

// runConductorChimeInChain dispatches chime-ins selected by the LLM
// conductor. Differs from runChimeInChain:
//
//   - Uses the conductor's per-agent list (and per-agent instructions)
//     instead of a score-gated polyphon ranking.
//   - Each agent receives the conductor's specific Instruction in its
//     directive instead of a role-derived ContentHint.
//   - No score gate -- the conductor already filtered for relevance.
//
// Same staggered delays + abort-on-new-utterance behavior as the legacy
// chain so the user experience is consistent.
func (c *CognitionIntegration) runConductorChimeInChain(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	primaryWinnerParticipantId string,
	plan *ConductorPlan,
	aiParticipants []*participantPayload,
	agentConfigs map[string]*agentPayload,
	sInfo spaceInfo,
) {
	if c == nil || plan == nil || len(plan.ChimeIns) == 0 {
		return
	}

	// Reset non-chime-in agents to Idle. Earlier in this turn we set
	// every non-winner to Waiting; agents the conductor didn't pick
	// for chime-in need their state cleared so the UI doesn't stick.
	chimeInIds := make(map[string]bool, len(plan.ChimeIns))
	for _, ci := range plan.ChimeIns {
		chimeInIds[strings.TrimSpace(ci.AgentId)] = true
	}
	go func() {
		// Tiny delay so the primary's response renders first.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-bgCtx.Done():
			return
		}
		for _, ap := range aiParticipants {
			if ap.ID == primaryWinnerParticipantId {
				continue
			}
			if chimeInIds[ap.ID] {
				continue
			}
			_ = c.upsertParticipantPresence(bgCtx, partitionId, ap.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
		}
	}()

	// Final sweep: at function exit, force every chime-in agent's
	// presence to Idle if it's still Thinking / Waiting / etc. This is
	// the safety net for orphan-state bugs where a chime-in iteration
	// hits a continue path before reaching the end-of-iteration Idle
	// reset (id mismatch, agent payload missing, abort mid-chain). The
	// per-iteration resets are still preferred because they keep
	// presence accurate during the chain; this defer just guarantees no
	// agent stays stuck on Waiting forever.
	defer func() {
		for participantId := range chimeInIds {
			if participantId == "" || participantId == primaryWinnerParticipantId {
				continue
			}
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participantId,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
		}
	}()

	c.Logger.Info("conductor: chime-in chain starting",
		"partitionId", partitionId,
		"chimeInCount", len(plan.ChimeIns),
		"phase", plan.Phase,
		"temperature", plan.Temperature)

	successfulFires := 0
	for i, ci := range plan.ChimeIns {
		participantId := strings.TrimSpace(ci.AgentId)
		if participantId == "" || participantId == primaryWinnerParticipantId {
			continue
		}

		// Stagger delay between min and max bounds.
		delay := chimeInMinDelay +
			time.Duration(rand.Int63n(int64(chimeInMaxDelay-chimeInMinDelay)))
		select {
		case <-time.After(delay):
		case <-bgCtx.Done():
			return
		}

		// Abort the chain if a new human utterance arrived during the
		// delay -- continuing on stale context would be the worst
		// failure mode.
		if !c.isLatestHumanUtterance(partitionId, originatingUtteranceId) {
			c.Logger.Debug("conductor: chime-in chain aborted (new utterance)",
				"partitionId", partitionId, "remaining", len(plan.ChimeIns)-i)
			return
		}

		// Find the participant + agent config.
		var participant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == participantId {
				participant = ap
				break
			}
		}
		if participant == nil {
			c.Logger.Warn("conductor: chime-in agent not found in space (orphan presence reset)",
				"partitionId", partitionId, "agentId", participantId)
			// participantId didn't match any aiParticipant -- can't
			// meaningfully reset its presence (we don't know which
			// participant it refers to). The defer-style sweep at the
			// end handles this case; nothing to do here.
			continue
		}
		agent := agentConfigs[participant.ID]
		if agent == nil {
			agent, _ = c.getAgentCached(bgCtx, participant.AgentId)
		}
		if agent == nil {
			c.Logger.Warn("conductor: chime-in agent payload not loaded",
				"partitionId", partitionId, "agentId", participantId)
			// Reset Waiting -> Idle so the UI doesn't stick. The defer
			// sweep would also catch this, but resetting eagerly keeps
			// the UI accurate during the rest of the chain.
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}

		c.Logger.Info("conductor: chime-in firing",
			"partitionId", partitionId, "idx", i, "agent", agent.Name,
			"instruction", conductorTruncate(ci.Instruction, 80),
			"delay", delay.String())

		_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
			presenceStateThinking, "Thinking…", "", originatingUtteranceId, "", nil)

		// Build a chime-in directive carrying the conductor's
		// per-agent instruction. The prompt template surfaces the
		// instruction as the load-bearing behavioral guide for this
		// turn -- no need for chimeInContentAngle's role-based
		// fallback string when we have a real conductor instruction.
		chimeInDirective := directiveFromConductorAgentPlan(plan, &ci, "chimein")
		chimeInCtx := contextWithDirective(bgCtx, chimeInDirective)

		// Build context (participants/recent utterances/history) for
		// the chime-in's prompt the same way the primary path does.
		participants, _ := c.getParticipantsForPromptCached(bgCtx, partitionId)
		recentUtterances, _ := c.getRecentUtterancesForPromptCached(bgCtx, partitionId, 20)
		history := buildHistoryFromRecentUtterances(recentUtterances, participant.ID, participants)
		var allAgentPayloads []*agentPayload
		for _, ap := range aiParticipants {
			if ag, ok := agentConfigs[ap.ID]; ok && ag != nil {
				allAgentPayloads = append(allAgentPayloads, ag)
			}
		}

		response, err := c.generateAIResponse(chimeInCtx, agent, "chimein",
			partitionId, participants, recentUtterances, history, sInfo, nil, allAgentPayloads...)
		if err != nil {
			c.Logger.Warn("conductor: chime-in generation failed",
				"error", err, "agent", agent.Name, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}
		if strings.TrimSpace(response) == "" {
			c.Logger.Warn("conductor: chime-in generation returned empty",
				"agent", agent.Name, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}

		participantPayloadForCi := &participantPayload{
			ID:      participant.ID,
			AgentId: agent.ID,
		}
		if insertErr := c.insertAIResponse(bgCtx, partitionId, participantPayloadForCi,
			"", originatingUtteranceId, response, nil, nil, nil); insertErr != nil {
			c.Logger.Warn("conductor: chime-in insert failed",
				"error", insertErr, "agent", agent.Name, "partitionId", partitionId)
		} else {
			successfulFires++
		}
		_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
			presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)

		if conductorState := c.conductors.Get(partitionId); conductorState != nil {
			conductorState.RecordAgentSpoke(agent.ID)
		}
	}

	c.Logger.Info("conductor: chime-in chain complete",
		"partitionId", partitionId,
		"plannedCount", len(plan.ChimeIns),
		"successfulFires", successfulFires)

	c.continueIfBranchPointsDeclared(bgCtx, partitionId, originatingUtteranceId,
		primaryWinnerParticipantId, plan, aiParticipants, agentConfigs, sInfo)
}

// mostRecentAgentText returns the text of the most recent agent
// turn in the score engine's session transcript, or "" if no agent
// has spoken yet (or the session doesn't exist). Used by the
// message classifier so it can detect when a user message is
// answering a question or offer the prior agent posed.
//
// Replaces previousAgentAskedQuestion -- the classifier no longer
// needs a yes/no signal about whether the prior agent ended with
// "?", it just gets the prior text and reasons about it.
func mostRecentAgentText(scoreEngine *polyphon.ScoreEngine, partitionId string) string {
	if scoreEngine == nil {
		return ""
	}
	session := scoreEngine.Sessions().Get(partitionId)
	if session == nil {
		return ""
	}
	recent := session.RecentTranscript(10)
	for i := len(recent) - 1; i >= 0; i-- {
		entry := recent[i]
		if entry.SpeakerType != "agent" {
			continue
		}
		return entry.Text
	}
	return ""
}

// agentRosterFromCandidates extracts the display names of every
// agent currently in the room. Passed to the message classifier
// so it can resolve in-text mentions like "is Nova there" without
// requiring an @-prefix.
func agentRosterFromCandidates(candidates []polyphon.AgentCandidate) []string {
	if len(candidates) == 0 {
		return nil
	}
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if name := strings.TrimSpace(c.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// isConversationalAckIntent reports whether the classifier-returned
// intent string is one of the kinds that a confident classifier
// would expect to silence the room for. Restrictive -- we only
// suppress on labels we explicitly trust as "no agent action
// needed". Any unknown label falls through to dispatch.
func isConversationalAckIntent(intent string) bool {
	switch strings.ToLower(strings.TrimSpace(intent)) {
	case "affirmation", "follow_up", "farewell", "smalltalk":
		return true
	default:
		return false
	}
}

// looksIncompleteVoiceUtterance returns true when the text reads as
// a mid-sentence fragment rather than a complete thought worth
// responding to. Heuristic-only, intentionally conservative: a true
// hit suppresses an agent dispatch, so the cost of a false positive
// is "Sofia doesn't reply when she should" -- the same shape as the
// classifier suppression itself. Tuned for the final-on-
// pause failure mode: user pauses mid-sentence, the ASR commits
// the in-flight chunk as a "final" transcript, the LLM classifier
// labels it as the closest complete intent (question / answer /
// request) because grammar pattern-matches.
//
// Three signals, any of which trips the fragment label:
//
//  1. Text ends in a function word -- pronoun, article, conjunction,
//     preposition, auxiliary verb -- which English speakers almost
//     never use as the final word of a complete sentence. "I would",
//     "Maybe you can help me with", "is there any" all hit this.
//
//  2. No terminal punctuation (`. ? !`) AND the utterance is short
//     (<= 6 words). ASR providers tend not to punctuate the trailing
//     pre-pause chunk it commits as final; a short un-punctuated
//     burst is almost always a fragment.
//
//  3. Ends in an obvious filler-trail token ("um", "uh", "like",
//     "so"). The ASR occasionally commits these as the final
//     word of an interim phrase when the user is gathering the
//     next clause.
func looksIncompleteVoiceUtterance(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	// Strip trailing terminal punctuation if any.
	last := trimmed[len(trimmed)-1]
	hasTerminalPunct := last == '.' || last == '?' || last == '!'
	cleaned := strings.TrimRight(trimmed, ".!?,;:")

	words := strings.Fields(cleaned)
	if len(words) == 0 {
		return false
	}
	lastWord := strings.ToLower(words[len(words)-1])

	// Terminal punctuation is the strongest "this clause is complete"
	// signal the ASR gives us. When it's there, trust it and don't
	// second-guess based on the last word. "What are you able to help
	// me with?" ends on the preposition `with` but is unambiguously a
	// complete question because the ASR terminated it with `?`. The
	// previous heuristic flipped this around -- it checked the last
	// word FIRST and suppressed complete sentences whose final token
	// happened to be a function word.
	if hasTerminalPunct {
		return false
	}
	// No terminal punctuation. Now the trailing word matters: function
	// words and fillers in this position almost always mean the
	// utterance trailed off mid-thought.
	if voiceTrailingFunctionWords[lastWord] {
		return true
	}
	if voiceFillerWords[lastWord] {
		return true
	}
	// Without terminal punctuation AND with a content word at the end,
	// the signal is ambiguous: it could be a complete declarative
	// ("I want pizza") that the ASR didn't punctuate, or a fragment
	// ("Maybe we should think about the"). Falling through to NOT
	// suppress here -- the user-perceived cost of suppressing a real
	// question is much higher than the cost of one extra reply on a
	// trailing fragment, and the function-word + filler rules above
	// already catch the high-confidence cases. The earlier rule (no
	// terminal punct + >2 words = fragment) was too aggressive
	// against the ASR's known-unreliable punctuation on flat-
	// intonation utterances.
	return false
}

// voiceTrailingFunctionWords are tokens that a complete English
// sentence almost never ends on. If the user's final word is one of
// these, the utterance is almost certainly a fragment.
var voiceTrailingFunctionWords = map[string]bool{
	// Pronouns (subject + object)
	"i": true, "you": true, "we": true, "they": true,
	"me": true, "him": true, "her": true, "us": true, "them": true,
	// Articles + determiners
	"a": true, "an": true, "the": true, "this": true, "that": true,
	"these": true, "those": true, "my": true, "your": true, "our": true,
	"their": true, "his": true, "any": true, "some": true, "every": true,
	"each": true, "no": true, "all": true,
	// Conjunctions
	"and": true, "or": true, "but": true, "so": true, "because": true,
	"if": true, "when": true, "while": true, "as": true, "though": true,
	// Prepositions
	"to": true, "for": true, "with": true, "on": true, "in": true,
	"at": true, "of": true, "by": true, "from": true, "about": true,
	"into": true, "onto": true, "upon": true, "over": true, "under": true,
	"through": true, "between": true, "among": true, "without": true,
	"within": true, "across": true,
	// Auxiliary verbs (modal + helper)
	"is": true, "are": true, "was": true, "were": true, "am": true,
	"be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "having": true,
	"do": true, "does": true, "did": true, "doing": true,
	"will": true, "would": true, "could": true, "should": true,
	"can": true, "may": true, "might": true, "must": true, "shall": true,
	"want": true, "need": true, "going": true,
	// Wh-words ending an utterance almost always means trailing.
	// "when" already appears above under conjunctions; keep these
	// distinct entries elided to avoid map-literal duplicates.
	"what": true, "where": true, "why": true, "how": true,
	"which": true, "whose": true, "whom": true,
}

// voiceFillerWords are conversational filler tokens that English
// speakers use to gather the next clause. Sentences rarely end on
// these; if the ASR commits with one as the final token, the
// utterance is mid-thought.
var voiceFillerWords = map[string]bool{
	"um": true, "uh": true, "uhh": true, "uhm": true, "umm": true,
	"like": true, "well": true, "yeah": true, "okay": true, "ok": true,
	"hmm": true, "huh": true, "right": true,
}

// resetWaitingPresence clears the "Waiting" state from every non-winner
// agent in the space. Used by dispatch paths that don't fire any
// post-primary chain (and thus don't have per-agent reset built into
// their normal flow). Runs in a goroutine with a small delay so the
// primary's response renders before the UI transitions back.
func (c *CognitionIntegration) resetWaitingPresence(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	excludeParticipantId string,
	aiParticipants []*participantPayload,
) {
	go func() {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-bgCtx.Done():
			return
		}
		for _, ap := range aiParticipants {
			if ap == nil || ap.ID == excludeParticipantId {
				continue
			}
			_ = c.upsertParticipantPresence(bgCtx, partitionId, ap.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
		}
	}()
}

// runConductorSequence dispatches an ORDERED list of independent solo
// turns selected by the conductor. Distinct from runConductorChimeInChain:
//
//   - Sequence agents each answer the user directly, NOT the primary.
//   - Order is preserved (array order); the conductor's order is the
//     conversational order (e.g. "let's start with X then Y then Z").
//   - Each agent's directive uses Mode=DirectivePrimary (independent
//     solo turn), not DirectiveChimeIn -- they're not adding to the
//     primary, they're each answering separately.
//
// Same staggered delays + abort-on-new-utterance + defer-style
// presence sweep as runConductorChimeInChain.
func (c *CognitionIntegration) runConductorSequence(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	primaryWinnerParticipantId string,
	plan *ConductorPlan,
	aiParticipants []*participantPayload,
	agentConfigs map[string]*agentPayload,
	sInfo spaceInfo,
) {
	if c == nil || plan == nil || len(plan.Sequence) == 0 {
		return
	}

	sequenceIds := make(map[string]bool, len(plan.Sequence))
	for _, sp := range plan.Sequence {
		sequenceIds[strings.TrimSpace(sp.AgentId)] = true
	}

	// Reset agents who are NOT primary AND NOT in the sequence.
	go func() {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-bgCtx.Done():
			return
		}
		for _, ap := range aiParticipants {
			if ap == nil {
				continue
			}
			if ap.ID == primaryWinnerParticipantId || sequenceIds[ap.ID] {
				continue
			}
			_ = c.upsertParticipantPresence(bgCtx, partitionId, ap.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
		}
	}()

	// Defer-style presence sweep: at exit, force every sequence agent
	// to Idle. Belt-and-suspenders for orphan paths.
	defer func() {
		for participantId := range sequenceIds {
			if participantId == "" || participantId == primaryWinnerParticipantId {
				continue
			}
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participantId,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
		}
	}()

	c.Logger.Info("conductor: sequence chain starting",
		"partitionId", partitionId,
		"sequenceCount", len(plan.Sequence),
		"phase", plan.Phase,
		"temperature", plan.Temperature)

	successfulFires := 0
	for i, sp := range plan.Sequence {
		participantId := strings.TrimSpace(sp.AgentId)
		if participantId == "" || participantId == primaryWinnerParticipantId {
			continue
		}

		delay := chimeInMinDelay +
			time.Duration(rand.Int63n(int64(chimeInMaxDelay-chimeInMinDelay)))
		select {
		case <-time.After(delay):
		case <-bgCtx.Done():
			return
		}

		if !c.isLatestHumanUtterance(partitionId, originatingUtteranceId) {
			c.Logger.Debug("conductor: sequence aborted (new utterance)",
				"partitionId", partitionId, "remaining", len(plan.Sequence)-i)
			return
		}

		var participant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == participantId {
				participant = ap
				break
			}
		}
		if participant == nil {
			c.Logger.Warn("conductor: sequence agent not found in space",
				"partitionId", partitionId, "agentId", participantId)
			continue
		}
		agent := agentConfigs[participant.ID]
		if agent == nil {
			agent, _ = c.getAgentCached(bgCtx, participant.AgentId)
		}
		if agent == nil {
			c.Logger.Warn("conductor: sequence agent payload not loaded",
				"partitionId", partitionId, "agentId", participantId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}

		c.Logger.Info("conductor: sequence firing",
			"partitionId", partitionId, "idx", i, "agent", agent.Name,
			"instruction", conductorTruncate(sp.Instruction, 80),
			"acknowledgePrior", sp.AcknowledgePrior,
			"delay", delay.String())

		_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
			presenceStateThinking, "Thinking…", "", originatingUtteranceId, "", nil)

		// Sequence agents get Mode=DirectivePrimary -- they're each
		// answering the user, not adding to a primary's response.
		seqDirective := directiveFromConductorAgentPlan(plan, &sp, "sequence")
		seqCtx := contextWithDirective(bgCtx, seqDirective)

		participants, _ := c.getParticipantsForPromptCached(bgCtx, partitionId)
		recentUtterances, _ := c.getRecentUtterancesForPromptCached(bgCtx, partitionId, 20)
		history := buildHistoryFromRecentUtterances(recentUtterances, participant.ID, participants)
		var allAgentPayloads []*agentPayload
		for _, ap := range aiParticipants {
			if ag, ok := agentConfigs[ap.ID]; ok && ag != nil {
				allAgentPayloads = append(allAgentPayloads, ag)
			}
		}

		response, err := c.generateAIResponse(seqCtx, agent, "utterance",
			partitionId, participants, recentUtterances, history, sInfo, nil, allAgentPayloads...)
		if err != nil {
			c.Logger.Warn("conductor: sequence generation failed",
				"error", err, "agent", agent.Name, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}
		if strings.TrimSpace(response) == "" {
			c.Logger.Warn("conductor: sequence generation returned empty",
				"agent", agent.Name, "partitionId", partitionId)
			_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
				presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)
			continue
		}

		participantPayloadForSp := &participantPayload{
			ID:      participant.ID,
			AgentId: agent.ID,
		}
		if insertErr := c.insertAIResponse(bgCtx, partitionId, participantPayloadForSp,
			"", originatingUtteranceId, response, nil, nil, nil); insertErr != nil {
			c.Logger.Warn("conductor: sequence insert failed",
				"error", insertErr, "agent", agent.Name, "partitionId", partitionId)
		} else {
			successfulFires++
		}
		_ = c.upsertParticipantPresence(bgCtx, partitionId, participant.ID,
			presenceStateIdle, "Idle", "", originatingUtteranceId, "", nil)

		if conductorState := c.conductors.Get(partitionId); conductorState != nil {
			conductorState.RecordAgentSpoke(agent.ID)
		}
	}

	c.Logger.Info("conductor: sequence chain complete",
		"partitionId", partitionId,
		"plannedCount", len(plan.Sequence),
		"successfulFires", successfulFires)

	c.continueIfBranchPointsDeclared(bgCtx, partitionId, originatingUtteranceId,
		primaryWinnerParticipantId, plan, aiParticipants, agentConfigs, sInfo)
}

// continueIfBranchPointsDeclared optionally re-invokes the conductor
// after the primary + sequence + chime-in chain has finished, when
// the original plan declared completion criteria OR branch points
// AND the per-turn iteration cap hasn't been hit.
//
// Two triggers:
//
//   - completionCriteria non-empty: the conductor said "this plan
//     counts as done when X happens." Re-consult so the conductor
//     can see the chain's outputs in the transcript and verify X
//     was met. If not, the next plan dispatches the missing
//     agents (e.g., "Sofia commented instead of joking -- dispatch
//     her with a stronger 'tell your own joke' instruction").
//   - branchPoints non-empty: explicit re-evaluation triggers.
//     Same mechanism, narrower contract.
//
// Hard guards against runaway:
//   - maxConductorIterations cap on ConductorState
//   - aborts if a new human utterance arrives mid-evaluation
//   - aborts on context cancellation
//
// On re-consult, the new plan dispatches normally (primary +
// sequence + chime-ins). Recursion is bounded by the iteration cap.
func (c *CognitionIntegration) continueIfBranchPointsDeclared(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	primaryWinnerParticipantId string,
	plan *ConductorPlan,
	aiParticipants []*participantPayload,
	agentConfigs map[string]*agentPayload,
	sInfo spaceInfo,
) {
	if c == nil || plan == nil {
		return
	}
	hasCompletionCheck := strings.TrimSpace(plan.CompletionCriteria) != "" || len(plan.BranchPoints) > 0
	if !hasCompletionCheck {
		return
	}
	state := conductorStateOrNil(c, partitionId)
	if state == nil {
		return
	}
	if state.CurrentIteration() >= maxConductorIterations {
		c.Logger.Info("conductor: completion-check re-invoke skipped (iteration cap)",
			"partitionId", partitionId, "iterations", state.CurrentIteration(),
			"cap", maxConductorIterations)
		return
	}
	// Don't re-invoke if a new human utterance arrived during the
	// chain. The fresh utterance kicks its own conductor consult on
	// the normal path, so re-invoking here would race with that.
	if !c.isLatestHumanUtterance(partitionId, originatingUtteranceId) {
		return
	}

	c.Logger.Info("conductor: completion-check re-invoke",
		"partitionId", partitionId,
		"branchCount", len(plan.BranchPoints),
		"completionCriteria", conductorTruncate(plan.CompletionCriteria, 80),
		"iteration", state.CurrentIteration())

	// Re-invoke runs in a goroutine so the calling chain can return.
	// The re-invoke is independent of the original chain's lifetime.
	go func() {
		// Tiny settle delay so the chain's last insert has landed in
		// the transcript before we re-consult.
		select {
		case <-time.After(750 * time.Millisecond):
		case <-bgCtx.Done():
			return
		}

		// One more abort check: the user might have spoken during the
		// settle delay. Bail if so.
		if !c.isLatestHumanUtterance(partitionId, originatingUtteranceId) {
			return
		}

		// Reload context. Transcript now includes the agents' chain
		// responses, so the conductor sees what was just said.
		participants, _ := c.getParticipantsForPromptCached(bgCtx, partitionId)
		recentUtterances, _ := c.getRecentUtterancesForPromptCached(bgCtx, partitionId, 20)

		// Build a synthetic Utterance for the consult. Reuse the
		// originating utterance's metadata; the transcript carries
		// the new context.
		var synthetic polyphon.Utterance
		for _, u := range recentUtterances {
			if id, _ := u["id"].(string); id == originatingUtteranceId {
				synthetic.ID = id
				synthetic.ScopeId = partitionId
				if t, _ := u["text"].(string); t != "" {
					synthetic.Text = t
				}
				if pid, _ := u["participantId"].(string); pid != "" {
					synthetic.ParticipantId = pid
				}
				break
			}
		}
		if synthetic.ID == "" {
			// Originating utterance scrolled out of the cache; nothing
			// safe to re-consult against.
			return
		}

		// Build candidates from the participants list (subset that's
		// AI participants).
		candidates := make([]polyphon.AgentCandidate, 0, len(aiParticipants))
		for _, ap := range aiParticipants {
			if ap == nil {
				continue
			}
			ag := agentConfigs[ap.ID]
			cand := polyphon.AgentCandidate{
				ID:            ap.AgentId,
				ParticipantId: ap.ID,
			}
			if ag != nil {
				cand.Name = ag.Name
				cand.Description = ag.Description
				cand.Role = ag.Role
				if doms := ag.domains(); len(doms) > 0 {
					cand.Domains = doms
				}
			}
			candidates = append(candidates, cand)
		}

		nextPlan, err := c.consultConductor(bgCtx, synthetic, candidates,
			agentConfigs, recentUtterances, sInfo, participants)
		if err != nil || nextPlan == nil {
			c.Logger.Info("conductor: branch-point re-invoke produced no plan",
				"error", err, "partitionId", partitionId)
			return
		}

		// Empty plan -> conductor decided we're done. No dispatch.
		if nextPlan.PrimaryAgentId() == "" && !nextPlan.HasSequence() && len(nextPlan.ChimeIns) == 0 {
			c.Logger.Info("conductor: branch-point re-invoke -> plan complete",
				"partitionId", partitionId)
			return
		}

		c.dispatchContinuationPlan(bgCtx, partitionId, originatingUtteranceId,
			nextPlan, aiParticipants, agentConfigs, sInfo)
	}()
}

// dispatchContinuationPlan dispatches a re-invoke plan returned by the
// branch-point loop. It's a narrower dispatch path than the original
// turn handler: the user has already received the original primary's
// response, so the "continuation" is just additional voices on the
// same turn. Order: primary -> sequence -> chime-ins, same as the
// original turn but skipping the routing/score-gate logic (the
// conductor's plan IS the routing).
func (c *CognitionIntegration) dispatchContinuationPlan(
	bgCtx context.Context,
	partitionId string,
	originatingUtteranceId string,
	plan *ConductorPlan,
	aiParticipants []*participantPayload,
	agentConfigs map[string]*agentPayload,
	sInfo spaceInfo,
) {
	// Filter out agents who already spoke on this user-turn cycle.
	// Continuation re-invokes are explicitly the case where the
	// conductor is re-evaluating after a chain settled. Re-dispatching
	// agents who already produced their expectedOutput causes the
	// duplicate-joke bug ("Briar told joke #1 in Plan #2, then Plan #3
	// dispatched Briar AGAIN to tell joke #2"). The conductor SHOULD
	// produce plans that only target missing agents, but the LLM
	// doesn't always honor that -- so we enforce it Go-side.
	plan = c.filterAlreadySpokenFromContinuation(partitionId, plan, agentConfigs)
	if plan == nil {
		return
	}

	primaryParticipantId := plan.PrimaryAgentId()
	if primaryParticipantId == "" {
		// No primary in the continuation -- skip directly to sequence /
		// chime-ins. Use a sentinel id so the dispatch helpers don't
		// accidentally skip the first agent.
		primaryParticipantId = "__no_primary__"
	} else {
		// Find + dispatch the primary. We use generateAIResponse +
		// insertAIResponse directly (no streaming) because this is a
		// continuation, not a fresh user turn -- shorter, contextual.
		var participant *participantPayload
		for _, ap := range aiParticipants {
			if ap.ID == primaryParticipantId {
				participant = ap
				break
			}
		}
		if participant != nil {
			agent := agentConfigs[participant.ID]
			if agent == nil {
				agent, _ = c.getAgentCached(bgCtx, participant.AgentId)
			}
			if agent != nil {
				directive := directiveFromConductorAgentPlan(plan, &plan.Primary, "primary")
				ctxWithDirective := contextWithDirective(bgCtx, directive)
				participants, _ := c.getParticipantsForPromptCached(bgCtx, partitionId)
				recentUtterances, _ := c.getRecentUtterancesForPromptCached(bgCtx, partitionId, 20)
				history := buildHistoryFromRecentUtterances(recentUtterances, participant.ID, participants)
				response, err := c.generateAIResponse(ctxWithDirective, agent, "utterance",
					partitionId, participants, recentUtterances, history, sInfo, nil)
				if err == nil && strings.TrimSpace(response) != "" {
					_ = c.insertAIResponse(bgCtx, partitionId, &participantPayload{
						ID:      participant.ID,
						AgentId: agent.ID,
					}, "", originatingUtteranceId, response, nil, nil, nil)
					if state := conductorStateOrNil(c, partitionId); state != nil {
						state.RecordAgentSpoke(agent.ID)
					}
				}
			}
		}
	}

	// Run sequence + chime-ins through the existing chain helpers.
	if plan.HasSequence() {
		c.runConductorSequence(bgCtx, partitionId, originatingUtteranceId,
			primaryParticipantId, plan, aiParticipants, agentConfigs, sInfo)
	}
	if len(plan.ChimeIns) > 0 {
		c.runConductorChimeInChain(bgCtx, partitionId, originatingUtteranceId,
			primaryParticipantId, plan, aiParticipants, agentConfigs, sInfo)
	}
}

// filterAlreadySpokenFromContinuation removes agents from a continuation
// plan whose ConductorState already shows them as having spoken on the
// current user-turn cycle. This is the Go-side enforcement of "don't
// re-dispatch agents who already produced their expectedOutput on this
// turn." Returns the filtered plan, or nil when nothing remains to
// dispatch.
//
// The cycle resets on RecordHumanSpoke (i.e. each new human utterance),
// so this filter applies ONLY within the lifetime of a single user
// turn -- exactly the scope where re-dispatch produces duplicates.
func (c *CognitionIntegration) filterAlreadySpokenFromContinuation(
	partitionId string,
	plan *ConductorPlan,
	agentConfigs map[string]*agentPayload,
) *ConductorPlan {
	if plan == nil {
		return nil
	}
	state := conductorStateOrNil(c, partitionId)
	if state == nil {
		return plan
	}
	logger := c.safeLogger()

	// State tracks AgentsSpokenThisCycle keyed by agent template id
	// (RecordAgentSpoke is called with agent.ID), but plans carry
	// participant ids. Resolve participant id -> agent template id
	// via agentConfigs so the comparison hits.
	already := func(participantId string) bool {
		participantId = strings.TrimSpace(participantId)
		if participantId == "" {
			return false
		}
		agent := agentConfigs[participantId]
		if agent == nil {
			return false
		}
		return state.AgentSpokeThisCycle(agent.ID)
	}

	// Filter primary
	if plan.Primary.AgentId != "" && already(plan.Primary.AgentId) {
		if logger != nil {
			logger.Info("conductor: continuation -- dropping primary already spoken this cycle",
				"agentId", plan.Primary.AgentId)
		}
		plan.Primary = ConductorAgentPlan{}
	}
	// Filter sequence
	cleanedSeq := plan.Sequence[:0]
	for _, sp := range plan.Sequence {
		if already(sp.AgentId) {
			if logger != nil {
				logger.Info("conductor: continuation -- dropping sequence agent already spoken",
					"agentId", sp.AgentId)
			}
			continue
		}
		cleanedSeq = append(cleanedSeq, sp)
	}
	plan.Sequence = cleanedSeq
	// Filter chime-ins
	cleanedChime := plan.ChimeIns[:0]
	for _, ci := range plan.ChimeIns {
		if already(ci.AgentId) {
			if logger != nil {
				logger.Info("conductor: continuation -- dropping chime-in agent already spoken",
					"agentId", ci.AgentId)
			}
			continue
		}
		cleanedChime = append(cleanedChime, ci)
	}
	plan.ChimeIns = cleanedChime

	// If the entire plan was filtered out, signal "no continuation"
	if plan.PrimaryAgentId() == "" && !plan.HasSequence() && len(plan.ChimeIns) == 0 {
		if logger != nil {
			logger.Info("conductor: continuation plan empty after filtering already-spoken agents",
				"partitionId", partitionId)
		}
		return nil
	}
	return plan
}
