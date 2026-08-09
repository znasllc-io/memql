import type { Concept } from "@znasllc-io/memql-sdk-core/client";
import type { ConceptLike, RowLike } from "@znasllc-io/memql-view-kit";

import { useConcepts } from "../cluster/useConcepts";

// The campaign concepts, by id.
//
// Naming concept ids in a FEATURE module is the same decision src/views/
// registry.ts makes: a designed surface is about a specific population and has
// to say which one. The concept-agnostic rule that forbids an id literal
// applies to the generic BROWSE path (src/concepts, src/components,
// src/viewkit) -- the machinery that must work for a concept nobody wrote code
// for. This is not that machinery.
//
// The DESCRIPTORS come from the cluster's own registry rather than from
// literals here, and that matters: the @displayCard slots are declared in
// dsl/campaigns/concepts.memql, and re-stating them in TypeScript would be a
// second source of truth that drifts the first time a slot changes.

export const CAMPAIGN_CONCEPT_ID = "v1:campaigns:campaign";
export const AUDIENCE_CONCEPT_ID = "v1:campaigns:audience";
export const TEMPLATE_CONCEPT_ID = "v1:campaigns:template";
export const DELIVERY_CONCEPT_ID = "v1:campaigns:delivery";
export const RECIPIENT_CONCEPT_ID = "v1:campaigns:recipient";

export interface CampaignConceptsState {
  // Undefined for an id this cluster does not declare -- a node whose DSL
  // bundle omits the campaigns domain. The surface says so rather than
  // rendering an empty population that reads as "you have no campaigns".
  descriptor: (conceptId: string) => Concept | undefined;
  loading: boolean;
  error: string;
}

export function useCampaignConcepts(): CampaignConceptsState {
  const { concepts, loading, error } = useConcepts();
  return {
    descriptor: (conceptId: string) => concepts.find((c) => c.id === conceptId),
    loading,
    error,
  };
}

// fallbackConcept keeps a table renderable when the registry read has not
// landed yet. It carries NO display card on purpose: view-kit's documented
// inference then picks the slots from the rows themselves, which is a better
// answer than a card this file invented and which would silently outrank the
// declared one if the registry arrived later.
export function fallbackConcept(conceptId: string, entity: string): ConceptLike {
  return { id: conceptId, entity };
}

export interface SelectOption {
  value: string;
  label: string;
}

// selectOptions turns a row set into picker options. Lives here rather than in
// a component so the layout modules stay free of row iteration, matching the
// discipline the predefined views are held to.
export function selectOptions(rows: readonly RowLike[], labelField: string): SelectOption[] {
  const out: SelectOption[] = [];
  for (const row of rows) {
    const id = row["id"];
    if (typeof id !== "string" || id === "") continue;
    const label = row[labelField];
    out.push({ value: id, label: typeof label === "string" && label !== "" ? label : id });
  }
  return out;
}

// labelFor resolves one row id to its human label, for the places a form has
// to name a selection outside the picker itself.
export function labelFor(options: readonly SelectOption[], value: string): string {
  return options.find((option) => option.value === value)?.label ?? value;
}
