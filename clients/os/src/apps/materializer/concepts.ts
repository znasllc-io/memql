import { Concepts } from "@znasllc-io/memql-sdk-core/client";

// The concepts the Materializer is about.
//
// GENERATED CONSTANTS, NEVER COMPOSED IDS (the Logs epic's rule,
// memql#4895): the app's Logs section asks the engine for lines tagged
// `materializer` OR about one of these, and a hand-written
// "v1:compose:composition" here would silently stop matching the day the
// namespace moves -- which is exactly the move this epic just made.
//
// ALL THREE BROADCAST. `component/node/routing.go` carries created /
// updated / deleted rules for every compose concept, so every surface in
// this app is live with no further engine work. That is checked rather
// than assumed: reasoning from the absence of a rule with a concept's
// NAME in it is the mistake the Fleet app made once and printed on the
// page as operator-facing copy.

/** The app id. It stays `materializer` while the namespace is `compose`. */
export const MATERIALIZER_APP_ID = "materializer";

export const COMPOSITION_CONCEPT: string = Concepts.COMPOSE_COMPOSITION;
export const TEMPLATE_CONCEPT: string = Concepts.COMPOSE_TEMPLATE;
export const RECIPE_CONCEPT: string = Concepts.COMPOSE_RECIPE;

/** Everything this app owns, for its Logs section's subject scope. */
export const MATERIALIZER_LOG_CONCEPTS = [
  COMPOSITION_CONCEPT,
  TEMPLATE_CONCEPT,
  RECIPE_CONCEPT,
] as const;
