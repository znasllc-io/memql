import { useCallback, useState } from "react";
import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import {
  BAND_QUESTIONS,
  BAND_ROLES,
  LAYOUT_DESCRIPTIONS,
  SECTION_LAYOUTS,
  VIEW_KIT_ELEMENTS,
  elementBand,
  type Arrangement,
} from "@znasllc-io/memql-view-kit";

import type { ClusterSuggest } from "./suggest";
import { parseArrangements } from "./savedViews";

// "Describe it": a whole view from a sentence (epic memql#4661, task
// memql#4670).
//
// ===========================================================================
// A DIFFERENT DECISION FROM "SUGGEST AN ARRANGEMENT"
// ===========================================================================
// The per-section suggester improves the arrangement of a concept the person
// ALREADY PICKED. This one picks the concept. That is the harder half and the
// one worth having a model for: somebody who wants "the agents that failed
// something this week" does not necessarily know what a concept is, and
// choosing between four hundred of them is exactly the task a registry digest
// plus a sentence is good at.
//
// ===========================================================================
// THE REPLY IS A DRAFT, NOT A VIEW
// ===========================================================================
// It arrives as `origin: "suggested"`, is repaired like a stored row before it
// is shown, and lands in the composer for editing. A draft the person then
// changes by hand is the expected outcome rather than a failure of the draft
// -- which is why getting the CONCEPT right matters more than getting the
// arrangement right, and why the prompt says so.
//
// ===========================================================================
// THE DIGEST IS COMPACT BY CONSTRUCTION
// ===========================================================================
// Ids, prose, field names with kinds, relationship labels. Not full schemas: a
// cluster publishes hundreds of concepts and their JSON Schema would be most
// of a context window spent on constraint keywords no layout decision reads.
// It is built HERE rather than server-side because the portal already holds
// the registry -- it reads it on connect -- and shipping it up is one round
// trip fewer than asking the engine to re-derive what the client has.

export const VIEW_COMPOSE_SUGGEST_DOMAIN = "viewCompose";

// How many concepts the digest carries.
//
// A CAP, because a cluster with four hundred concepts would spend the whole
// budget on the digest and leave none for the answer. Ordered by the registry's
// own order, which puts core domains first.
export const DIGEST_CAP = 120;

export interface ViewDraft {
  readonly name: string;
  readonly reasoning: string;
  readonly conceptIds: readonly string[];
  readonly arrangements: readonly Arrangement[];
}

export interface DescribeState {
  readonly status: "idle" | "asking" | "ready" | "unavailable";
  readonly draft: ViewDraft | undefined;
  readonly unavailable: string;
  readonly ask: (description: string) => void;
  readonly dismiss: () => void;
}

export function useDescribeView(
  concepts: readonly Concept[],
  suggest: ClusterSuggest | undefined,
): DescribeState {
  const [status, setStatus] = useState<DescribeState["status"]>("idle");
  const [draft, setDraft] = useState<ViewDraft | undefined>(undefined);
  const [unavailable, setUnavailable] = useState("");

  const dismiss = useCallback(() => {
    setStatus("idle");
    setDraft(undefined);
    setUnavailable("");
  }, []);

  const ask = useCallback(
    (description: string) => {
      if (description.trim() === "") return;
      if (suggest === undefined) {
        setStatus("unavailable");
        setUnavailable(
          "Not connected to a cluster, so there is nothing to ask. Pick the concepts " +
            "yourself and the composer will match elements to them.",
        );
        return;
      }
      setStatus("asking");
      setUnavailable("");

      void suggest(VIEW_COMPOSE_SUGGEST_DOMAIN, {
        description: description.trim(),
        registry: registryDigest(concepts),
        elements: elementDigest(),
        layouts: layoutDigest(),
        bands: bandDigest(),
      })
        .then((reply) => {
          const parsed = readDraft(reply.result, concepts);
          if (parsed === undefined) {
            // A reply naming no concept this cluster publishes is not a draft
            // -- it is a reply about a different cluster. Reporting it as
            // unavailable is honest; opening an empty composer would look like
            // the feature worked.
            setStatus("unavailable");
            setUnavailable(
              "The reply did not name a concept this cluster publishes, so there is " +
                "nothing to open. Try naming the population you mean.",
            );
            return;
          }
          setDraft(parsed);
          setStatus("ready");
        })
        .catch((err: unknown) => {
          // EVERY failure lands here and all of them mean the same thing to the
          // person: pick the concepts yourself. The message says which one it
          // was, because "no provider configured" and "the call timed out"
          // want different responses from an operator.
          setStatus("unavailable");
          setDraft(undefined);
          setUnavailable(err instanceof Error ? err.message : String(err));
        });
    },
    [concepts, suggest],
  );

  return { status, draft, unavailable, ask, dismiss };
}

// readDraft parses an untrusted reply.
//
// UNTRUSTED IS THE OPERATIVE WORD, exactly as it is for an arrangement
// proposal: the structured-output path constrains a reply to a JSON schema, it
// does not constrain it to concepts that exist. A section naming a concept
// this cluster does not publish is DROPPED here, and the arrangements that
// survive are repaired against live rows by the composer's own seed path --
// which is the same validation a hand-built draft goes through.
function readDraft(raw: unknown, concepts: readonly Concept[]): ViewDraft | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const object = raw as Record<string, unknown>;
  const published = new Set(concepts.map((c) => c.id));

  const sections = Array.isArray(object["sections"]) ? object["sections"] : [];
  const arrangements = parseArrangements(sections).filter((a) => published.has(a.conceptId));
  if (arrangements.length === 0) return undefined;

  const name = typeof object["name"] === "string" ? object["name"].trim() : "";
  const reasoning = typeof object["reasoning"] === "string" ? object["reasoning"] : "";
  return {
    name: name === "" ? "Suggested view" : name,
    reasoning,
    conceptIds: arrangements.map((a) => a.conceptId),
    arrangements,
  };
}

function registryDigest(concepts: readonly Concept[]): unknown[] {
  return concepts.slice(0, DIGEST_CAP).map((concept) => ({
    id: concept.id,
    entity: concept.entity,
    description: concept.description,
    // The DECLARED shape where the cluster publishes one (epic memql#4661).
    // Absent leaves the model with the id and the prose, which is what it had
    // before and is still enough to pick a population.
    fields: (concept.fields ?? []).map((field) => ({
      name: field.name,
      kind: field.kind,
      required: field.required,
    })),
    relationships: (concept.relationships ?? []).map((rel) => ({
      as: rel.as === "" ? rel.field : rel.as,
      target: rel.target,
    })),
  }));
}

function elementDigest(): unknown[] {
  return VIEW_KIT_ELEMENTS
    // The hosted kinds are PLACED, never discovered -- a scene or a widget is
    // meaningless without a module id, and offering them here would invite a
    // draft naming one that does not exist.
    .filter((element) => element.placedOnly !== true)
    .map((element) => ({
      element: element.id,
      title: element.title,
      summary: element.summary,
      band: elementBand(element),
    }));
}

function layoutDigest(): unknown[] {
  return SECTION_LAYOUTS.map((layout) => ({
    layout,
    description: LAYOUT_DESCRIPTIONS[layout],
  }));
}

function bandDigest(): unknown[] {
  return BAND_ROLES.map((band) => ({ band, question: BAND_QUESTIONS[band] }));
}
