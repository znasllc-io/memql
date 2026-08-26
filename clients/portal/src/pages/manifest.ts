import type { ArrangedElement, Arrangement } from "@znasllc-io/memql-view-kit";

// A PAGE MANIFEST: what a page IS, as data (epic memql#4661).
//
// ===========================================================================
// THE CLAIM THIS TYPE MAKES
// ===========================================================================
// The arrangement system is the PAGE system (spec D6). Every portal page that
// shows data is a layout plus elements plus registered widgets, and is
// therefore -- for free -- regenerable, versioned, and consistent with every
// other page. A manifest is how a page says that about itself.
//
// Before this, "a predefined view" was a React module and "a composed view"
// was a graph row, and the two rendered through different code. That is why
// the five designed pages could not be regenerated, could not carry a version
// strip, and improved only when somebody edited their module. The manifest is
// the seed half of making them one thing; the override row (task memql#4668)
// is the other half.
//
// ===========================================================================
// A MANIFEST IS A SEED, NOT A SETTING
// ===========================================================================
// It is the ORIGINAL version of a page -- what the page looks like before
// anybody regenerated it. It needs no graph row precisely because it is the
// default: a person who has never regenerated a page has no row, and the
// absence of a row is not a missing setting, it is the answer.

export interface PageSection {
  // The concept this section's rows come from. Every section is exactly one
  // authorized concept walk -- no joins (spec D2) -- so a page reading two
  // populations declares two sections.
  readonly conceptId: string;
  // The section heading, when a page wants one. A single-section page usually
  // does not: its own page header already names the population.
  readonly title?: string;
  // A line under the heading. Interface copy, not a description of the schema.
  readonly meta?: string;
  // The seed arrangement: layout, elements, roles, bindings.
  readonly arrangement: Arrangement;
  // LINK this section to an earlier one (epic memql#4661, task memql#4671).
  //
  // "Selecting a row in section A filters section B to the rows related to
  // it." Declared as the relationship LABEL (`as`) plus the concept it points
  // back at, because that is the shape the schema already publishes -- and
  // because naming the relationship rather than a field means the link
  // survives a field rename.
  //
  // V1 FILTERS THE LOADED WALK, CLIENT-SIDE, AND SAYS SO. A predicate-capable
  // read path is the filed follow-up, not this epic (spec F). The honest
  // label is part of the contract rather than decoration: a section that
  // showed three related rows when the cluster holds forty, without saying it
  // was filtering only what it had loaded, would be wrong in a way nobody
  // could see.
  readonly linkedTo?: {
    // The section whose selection drives this one.
    readonly conceptId: string;
    // The field on THIS section's rows holding the pointer back.
    readonly field: string;
  };
  // Entries this page CANNOT BE WITHOUT.
  //
  // The guardrail behind regeneration: a model rearranging the fleet page may
  // move the machines list and re-caption it, and may not remove it, because a
  // fleet page with no machines on it is not a rearrangement of the fleet page
  // -- it is a different page that happens to load the same rows.
  // sanitizeArrangement re-inserts a missing one, matched on element id plus
  // module option, so moving and re-captioning still work.
  readonly required?: readonly ArrangedElement[];
}

export interface PageManifest {
  // The page id: `views.users`, `fleet.machines`, `me.settings`.
  //
  // DOTTED, SURFACE FIRST, and it is a durable identifier rather than a route:
  // an override row is keyed on it, so renaming a page id orphans every
  // regeneration anybody made of that page. A route may change freely.
  readonly pageId: string;
  readonly title: string;
  // One line under the heading saying what this page is and why somebody is
  // looking at it. Interface copy, shown verbatim.
  readonly blurb: string;
  readonly sections: readonly PageSection[];
}

// requiredEntriesFor is what a page hands sanitizeArrangement.
//
// Concatenated across sections rather than looked up per section, because a
// caller repairing ONE section still needs that section's own required list
// and nothing else -- see requiredForSection.
export function requiredForSection(
  manifest: PageManifest | undefined,
  conceptId: string,
): readonly ArrangedElement[] {
  if (manifest === undefined) return [];
  const section = manifest.sections.find((s) => s.conceptId === conceptId);
  return section?.required ?? [];
}

export function sectionFor(
  manifest: PageManifest | undefined,
  conceptId: string,
): PageSection | undefined {
  return manifest?.sections.find((s) => s.conceptId === conceptId);
}
