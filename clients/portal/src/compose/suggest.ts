import { useCallback, useState } from "react";
import { ai, type Dispatcher } from "@znasllc-io/memql-sdk-core";
import {
  arrangementRequest,
  readArrangement,
  type Arrangement,
  type ArrangementProblem,
  type ConceptProfile,
} from "@znasllc-io/memql-view-kit";

// The optional half of the composer: asking a model to arrange a view.
//
// ===========================================================================
// THE RULE THIS MODULE EXISTS TO ENFORCE
// ===========================================================================
// A suggestion is an OFFER. Every path out of this module -- success, refusal,
// transport failure, a reply that parses to nothing -- leaves the composer
// holding a working arrangement, because the composer never gave up the one it
// already had. Nothing here can produce a blank screen, and nothing downstream
// branches on whether a model was involved: `applied` takes the same
// Arrangement value `seeded` produces.
//
// That is why the suggester is a FUNCTION PASSED IN rather than a call made
// inline. The composer takes `(request) => Promise<unknown>`; the cluster-
// backed one is below, a test supplies one that throws, and the code path
// under test is then the real one rather than a mock of it. "Works with the
// provider unavailable" is a unit test, not a manual check.
//
// ===========================================================================
// THE WIRE PATH, AND THE ONE PIECE THAT IS NOT WIRED YET
// ===========================================================================
// The call rides `AiSuggestMsg` on MemqlService.Stream -- the engine's
// structured-output surface. Its handler (component/grpc/ai_handlers.go) looks
// the wire `domain` up in the suggest-domain registry
// (component/memql/suggest_registry.go), obtains a rendered prompt plus a JSON
// schema, and runs the call through ChatStructuredProvider.CallChatStructured,
// so the reply is a validated object rather than prose to parse. That is
// exactly the path memql#3320 asks for, and it is domain-agnostic on this
// side: the client names a domain and sends a payload.
//
// The `viewArrangement` domain's handler is a Go registration --
// RegisterSuggestDomain("viewArrangement", ...) rendering the
// `composeViewArrangement` prompt (dsl/portalviews/prompts.memql) against
// ARRANGEMENT_PROPOSAL_SCHEMA -- and it is NOT part of this change. Until it
// lands the engine answers with the registry's typed "unknown domain" error,
// which arrives here as a refusal and is reported to the person as
// "suggestions are not available on this cluster". The composer is fully
// usable in that state; that is the state every test below runs in, and it is
// the state a cluster with no AI provider configured is in permanently.

export type ArrangementSuggester = (
  request: ReturnType<typeof arrangementRequest>,
  signal?: AbortSignal,
) => Promise<unknown>;

// The wire domain. One constant, because the DSL prompt's comment names it and
// a second spelling would be a silent no-op.
export const ARRANGEMENT_SUGGEST_DOMAIN = "viewArrangement";

// clusterSuggester is the real one: the structured-output call, over the
// connection the portal already has.
export function clusterSuggester(dispatcher: Dispatcher): ArrangementSuggester {
  return async (request, signal) => {
    const reply = await ai.aiSuggest(
      dispatcher,
      ARRANGEMENT_SUGGEST_DOMAIN,
      // Cast because the wire payload is a plain record; the request is a
      // structurally-compatible object of scalars, arrays and records, which
      // is what the Struct encoding accepts.
      request as unknown as Record<string, unknown>,
      signal ? { signal } : {},
    );
    return reply.result;
  };
}

export type SuggestionStatus = "idle" | "asking" | "ready" | "unavailable";

export interface SuggestionState {
  readonly status: SuggestionStatus;
  // The proposed arrangement, once one has arrived and been repaired. The
  // composer applies this; it never applies a raw reply.
  readonly arrangement: Arrangement | undefined;
  // The model's own words. Empty when it said nothing.
  readonly reasoning: string;
  // What had to be corrected on the way in. Shown rather than swallowed: a
  // model that keeps proposing an element that does not fit is a fact about
  // the prompt, and hiding it hides the bug.
  readonly problems: readonly ArrangementProblem[];
  // Why there is no suggestion, in words a person can act on. Set only when
  // status is "unavailable".
  readonly unavailable: string;
  readonly ask: () => void;
  readonly dismiss: () => void;
}

// useArrangementSuggestion drives one section's suggestion.
//
// It holds NO part of the composed view. The arrangement it produces is handed
// back to the caller to apply or ignore, so declining a suggestion is not an
// undo -- it is simply not dispatching anything.
export function useArrangementSuggestion(
  profile: ConceptProfile | undefined,
  suggester: ArrangementSuggester | undefined,
): SuggestionState {
  const [status, setStatus] = useState<SuggestionStatus>("idle");
  const [arrangement, setArrangement] = useState<Arrangement | undefined>(undefined);
  const [reasoning, setReasoning] = useState("");
  const [problems, setProblems] = useState<readonly ArrangementProblem[]>([]);
  const [unavailable, setUnavailable] = useState("");

  const dismiss = useCallback(() => {
    setStatus("idle");
    setArrangement(undefined);
    setReasoning("");
    setProblems([]);
    setUnavailable("");
  }, []);

  const ask = useCallback(() => {
    if (profile === undefined) return;
    if (suggester === undefined) {
      setStatus("unavailable");
      setUnavailable(
        "Not connected to a cluster, so there is nothing to ask. The arrangement " +
          "below was matched from the rows and works without one.",
      );
      return;
    }
    setStatus("asking");
    setUnavailable("");
    const request = arrangementRequest(profile);

    void suggester(request)
      .then((raw) => {
        // Parsed and repaired before it is offered. readArrangement drops any
        // element that does not exist or does not fit and falls back to the
        // deterministic baseline, so what lands in state is always renderable.
        const proposal = readArrangement(raw, profile);
        setArrangement(proposal.arrangement);
        setReasoning(proposal.reasoning);
        setProblems(proposal.problems);
        setStatus("ready");
      })
      .catch((err: unknown) => {
        // EVERY failure lands here and all of them mean the same thing to the
        // person: there is no second opinion today. The message says which
        // one it was, because "no provider configured" and "the call timed
        // out" want different responses from an operator -- but neither
        // changes what the composer can do.
        setStatus("unavailable");
        setArrangement(undefined);
        setReasoning("");
        setProblems([]);
        setUnavailable(err instanceof Error ? err.message : String(err));
      });
  }, [profile, suggester]);

  return { status, arrangement, reasoning, problems, unavailable, ask, dismiss };
}
