// An ARRANGEMENT: which elements make up a view, in which band, bound to
// which fields.
//
// ===========================================================================
// WHAT THIS IS FOR
// ===========================================================================
// fitness.ts answers "can this element render this concept, and with which
// fields". That is the hard half and it is already deterministic. What is
// left is the composition question -- "so what does the SCREEN look like" --
// and a user-composed view (memql#3320) has three different things that
// answer it:
//
//   * the system, from the concept's shape alone,
//   * a model, asked to propose something better,
//   * the person, editing either of the above.
//
// The load-bearing decision in this module is that all three produce THE SAME
// KIND OF VALUE: an `Arrangement`, a plain JSON-safe data structure with no
// behaviour. That is what makes the AI optional rather than structural. A
// proposal is not a different mode the composer runs in; it is one more way
// to obtain the value the composer was already holding, and it is validated
// by the same function that validates a hand-built one. Delete the model, the
// network, the whole provider system, and the composer still produces, saves
// and renders views -- it just stops offering a second opinion.
//
// It is also why a saved view is storable: an Arrangement is exactly what
// goes in a `v1:portalviews:view` row's payload, and reading one back is a
// parse, not a reconstruction.
//
// ===========================================================================
// THE ARRANGEMENT IS ORDERED AND BANDED, NOT POSITIONED
// ===========================================================================
// An entry says WHICH BAND it belongs to, never where on a page it sits. A
// band is a question (fitness.ts: BAND_QUESTIONS), a host decides what a band
// looks like, and a saved arrangement therefore survives a redesign of the
// surface that renders it. Storing pixel positions, column spans or a grid
// would pin every saved row to one release's layout.
//
// Entries are a FLAT ORDERED LIST rather than a map of band -> element,
// because a person may legitimately want two readings or two rolls, and
// because order within a band is a real choice. The deterministic proposal
// emits at most one entry per band; nothing else is limited to that.
//
// See docs/public/concepts/composed-views.md.

import { SCENE_ELEMENT_ID, VIEW_KIT_ELEMENTS, WIDGET_ELEMENT_ID } from "./elements.js";
import {
  BAND_ROLES,
  boundFields,
  elementBand,
  explainFit,
  fitElement,
  type BandRole,
  type ConceptProfile,
  type ElementFit,
  type ElementOptions,
  type ElementSpec,
} from "./fitness.js";

// ---------------------------------------------------------------------------
// The value
// ---------------------------------------------------------------------------

// One element in a view.
//
// `bindings` is the same shape ElementOptions.bindings takes, and it means the
// same thing -- including the part that matters most here: naming a slot with
// an EMPTY LIST is how a composed view declines a slot the automatic
// resolution would otherwise fill. A person who removed a chart's measure
// meant it, and the arrangement has to be able to say so.
// ---------------------------------------------------------------------------
// Layout and role -- the second and third dimensions (epic memql#4661)
// ---------------------------------------------------------------------------

// SectionLayout is HOW a section's bands are placed, as distinct from WHICH
// elements are in them. Five values, chosen against live mockups with the
// owner (spec D1).
//
// It is a section-level property, not a per-entry one, because it is a
// statement about the whole page and there is exactly one page. Two entries
// cannot disagree about whether the section is a dashboard.
//
// ABSENT MEANS STACK, and that is what makes this additive: every arrangement
// stored before this existed renders byte-identically to how it always did.
// The default is not "the best layout we can infer" -- inferring one would
// silently re-lay-out views people already have.
export type SectionLayout = "stack" | "dashboard" | "split" | "focus" | "gallery";

export const SECTION_LAYOUTS: readonly SectionLayout[] = [
  "stack",
  "dashboard",
  "split",
  "focus",
  "gallery",
];

// One line per layout, for a picker. Prose rather than labels, for the same
// reason BAND_QUESTIONS is prose: a person choosing a layout is choosing what
// the page should EMPHASISE, not naming a CSS grid.
export const LAYOUT_DESCRIPTIONS: Readonly<Record<SectionLayout, string>> = {
  stack: "Everything in a column, in the order it reads.",
  dashboard: "Numbers across the top, shapes side by side, the list below.",
  split: "The list on the left, one row's detail on the right.",
  focus: "One element carries the page, with the rest in a column beside it.",
  gallery: "A card per row, with the numbers as a header strip.",
};

// EntryRole is the EMPHASIS one entry carries within its layout.
//
// `hero` is scarce by design: it is the one element the page is about, and a
// page with two heroes has none. Sanitize does not enforce scarcity by
// deleting a second hero -- it renders the first and treats the rest as
// standard, because deleting somebody's element to enforce a design rule is
// worse than laying it out plainly.
export type EntryRole = "hero" | "supporting" | "standard";

export const ENTRY_ROLES: readonly EntryRole[] = ["hero", "supporting", "standard"];

export interface ArrangedElement {
  // ElementSpec.id. A string rather than the spec itself: an arrangement is
  // stored, sent over a wire and re-read against whatever element library the
  // reader has.
  readonly element: string;
  readonly band: BandRole;
  // The band caption, when the composer or the model had something better to
  // say than the element's own title. Omitted uses the element's title.
  readonly title?: string;
  readonly bindings?: Readonly<Record<string, readonly string[]>>;
  // How much of the page this entry carries. Omitted is `standard`, which is
  // what every entry stored before roles existed means.
  readonly role?: EntryRole;
  // Element-kind options: which SCENE (`sceneId`) or which WIDGET
  // (`widgetId`) this entry names. Carried on the entry rather than in
  // `bindings` because a binding names FIELDS and these name a registered
  // module -- putting them in the same map would make "the field called
  // goalMap" a thing a reader has to rule out.
  //
  // Both registries are CLOSED: sanitize drops an entry naming one that does
  // not exist, so an arrangement can place a scene or a widget and can never
  // invent one.
  readonly options?: Readonly<Record<string, string>>;
}

export interface Arrangement {
  // The concept whose rows this arrangement is for. Carried so a stored
  // arrangement cannot be silently re-applied to a different row set, and so
  // a reader can profile the right concept before validating it.
  readonly conceptId: string;
  // How this section places its bands. Omitted is `stack`.
  readonly layout?: SectionLayout;
  readonly elements: readonly ArrangedElement[];
}

export const EMPTY_ARRANGEMENT: Arrangement = { conceptId: "", elements: [] };

// arrangementLayout is the ONE place absent-means-stack is decided, so no
// consumer has to remember the default and no two consumers can pick
// different ones.
export function arrangementLayout(arrangement: Arrangement): SectionLayout {
  const named = SECTION_LAYOUTS.find((l) => l === arrangement.layout);
  return named ?? "stack";
}

// entryRole is the same, for the entry dimension.
export function entryRole(entry: ArrangedElement): EntryRole {
  const named = ENTRY_ROLES.find((r) => r === entry.role);
  return named ?? "standard";
}

// elementOptions turns one entry into the options its renderer takes. The
// single place the conversion happens, so a host cannot forget that an entry's
// bindings are the caller-override half of the fitness contract.
export function elementOptions(entry: ArrangedElement): ElementOptions {
  return entry.bindings === undefined ? {} : { bindings: entry.bindings };
}

// ---------------------------------------------------------------------------
// Candidacy
// ---------------------------------------------------------------------------

// One element, judged against one concept. This is what a picker renders: the
// element, whether it is offerable, which fields it would use, and view-kit's
// own sentence for why -- never a sentence the composer invented.
export interface ElementCandidate {
  readonly element: ElementSpec;
  readonly band: BandRole;
  readonly fit: ElementFit;
  // fit.verdict !== "unfit". Named separately because it is the only question
  // a picker asks of the verdict, and `partial` reading as usable is the
  // point: a degraded element is offerable, and `explanation` says how.
  readonly usable: boolean;
  // explainFit's prose. Present for every candidate, fitting or not: the
  // greyed-out entries are the ones a person most wants an explanation for.
  readonly explanation: string;
}

// elementCandidates judges a whole library against one concept, best first.
//
// DETERMINISTIC AND TOTAL. Every element in the library gets an entry --
// including the ones that do not fit -- because a composer that silently
// dropped the calendar could not answer "why can't I pick the calendar". The
// order is fitElements' ranking, which is a pure function of the profile.
//
// No AI, no network, no configuration. This is the whole of step 3 of
// memql#3320, and everything below is layered on top of it.
export function elementCandidates(
  profile: ConceptProfile,
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): readonly ElementCandidate[] {
  const byId = new Map(library.map((element) => [element.id, element]));
  return rankFits(library, profile).map((fit) => {
    const element = byId.get(fit.element)!;
    return {
      element,
      band: elementBand(element),
      fit,
      usable: fit.verdict !== "unfit",
      explanation: explainFit(element, fit, profile),
    };
  });
}

// rankFits is fitElements' ordering, recomputed here rather than imported so
// this module can rank a library the caller passed in. Same comparator: full
// before partial before unfit, then score, then specificity (so the universal
// fallbacks sort below anything that engaged with the concept's shape), then
// declaration order.
function rankFits(
  library: readonly ElementSpec[],
  profile: ConceptProfile,
): readonly ElementFit[] {
  const rank: Record<string, number> = { full: 0, partial: 1, unfit: 2 };
  const specificity = new Map(
    library.map((e) => [e.id, e.requires.filter((r) => (r.min ?? 1) > 0).length]),
  );
  const order = new Map(library.map((e, i) => [e.id, i]));
  return library
    .map((element) => fitElement(element, profile))
    .sort((a, b) => {
      if (rank[a.verdict] !== rank[b.verdict]) return rank[a.verdict] - rank[b.verdict];
      if (a.score !== b.score) return b.score - a.score;
      const sa = specificity.get(a.element) ?? 0;
      const sb = specificity.get(b.element) ?? 0;
      if (sa !== sb) return sb - sa;
      return (order.get(a.element) ?? 0) - (order.get(b.element) ?? 0);
    });
}

// ---------------------------------------------------------------------------
// The deterministic proposal
// ---------------------------------------------------------------------------

// proposeArrangement builds a view for a concept nobody wrote code for, from
// the concept's shape alone.
//
// THE RULE, in one sentence: take the best-fitting element in each band, in
// band order. That is the same grammar the five predefined views follow by
// hand (a reading, a shape, a roll), applied by a machine to a concept nobody
// designed for -- which is exactly the claim memql#3320 makes.
//
// THE ROLL BAND ALWAYS FILLS. If nothing in it fits -- the honest case is an
// empty row set, where every element is below its minimum -- the library's
// universal fallback is used anyway. Rendering it produces view-kit's own
// "cannot render, here is why" sentence, which is a better empty state than a
// view with no elements at all, and it means a saved arrangement made against
// an empty concept still has something to show the day rows arrive.
//
// The fallback is FOUND, not named: the FIRST roll-band element with no
// required requirements. An element that requires nothing of a concept fits
// everything by construction, and first-declared is the one fitElements would
// have ranked highest among those equals -- so the fallback and the ranked
// pick agree the moment there is a single row to rank with.
export function proposeArrangement(
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): Arrangement {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const candidates = elementCandidates(profile, library);
  const elements: ArrangedElement[] = [];

  for (const band of BAND_ROLES) {
    // `placedOnly` elements are skipped: a scene or a widget is meaningless
    // without an option naming which module it is, and the proposal has no
    // basis for choosing one. They remain fully offerable in a picker -- see
    // elementCandidates, which does NOT filter them.
    const best = candidates.find((c) => c.band === band && c.usable && !c.element.placedOnly);
    if (best !== undefined) {
      elements.push({ element: best.element.id, band });
      continue;
    }
    if (band === "roll") {
      const fallback = universalFallback(library);
      if (fallback !== undefined) elements.push({ element: fallback.id, band });
    }
  }

  // The proposal names no layout, which means stack. Inferring one from the
  // band mix would make the deterministic answer a design opinion, and it is
  // the one answer in this module that has to be predictable.
  return {
    conceptId: profile.concept.id,
    elements: withRequired(elements, options.required ?? [], profile, options),
  };
}

function universalFallback(library: readonly ElementSpec[]): ElementSpec | undefined {
  return library.find(
    (element) =>
      elementBand(element) === "roll" && !element.requires.some((r) => (r.min ?? 1) > 0),
  );
}

// explainArrangement is one sentence per entry, in the arrangement's order,
// built from explainFit -- so the reasoning a composer shows for a
// deterministic arrangement and the reasoning it shows for a model's are
// written by the same author (the element), in the same voice.
export function explainArrangement(
  arrangement: Arrangement,
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): readonly string[] {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const byId = new Map(library.map((element) => [element.id, element]));
  const out: string[] = [];
  for (const entry of arrangement.elements) {
    const element = byId.get(entry.element);
    if (element === undefined) {
      out.push(`No element called ${entry.element} is available here.`);
      continue;
    }
    out.push(explainFit(element, fitElement(element, profile, elementOptions(entry)), profile));
  }
  return out;
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

export type ArrangementFault =
  // The element id is not in this library. A saved view from a cluster or a
  // release with an element this one does not carry.
  | "unknown-element"
  // The element cannot render these rows at all.
  | "unfit"
  // A `scene` or `widget` entry naming a module no registry in this build
  // carries. Fatal for the same reason unknown-element is: there is nothing
  // to render, and a placeholder would be a page element that does nothing.
  | "unknown-module"
  // The section names a layout this build does not have. Repaired to stack,
  // reported so a composer can say what happened.
  | "unknown-layout"
  // The layout could not be honoured with the entries present -- a focus with
  // nothing that can be a hero, a split with no detail pane. Repaired to
  // stack; the entries are untouched.
  | "layout-unsatisfiable"
  // An entry asked for a role the element cannot express. Ignored, never
  // removed: the element was still a deliberate choice.
  | "role-unexpressible"
  // A binding names a field no row in the sample carries. Not fatal BY
  // ITSELF: a field absent from a 100-row page may well be on page two, so
  // the arrangement is reported rather than rewritten. It often arrives
  // alongside `unfit` and that is the fitness contract working as documented
  // -- naming a slot settles it, so an override the profile cannot honour
  // leaves a REQUIRED slot unbound, and an element with an unbound required
  // slot does not fit. On an OPTIONAL slot the element degrades and survives.
  | "unknown-field"
  // Two entries render the same element in the same band.
  | "duplicate";

export interface ArrangementProblem {
  // Index into arrangement.elements, or -1 for a whole-arrangement problem.
  readonly at: number;
  readonly element: string;
  readonly fault: ArrangementFault;
  // One sentence, shown to a person.
  readonly detail: string;
}

// arrangementProblems reports everything wrong with an arrangement WITHOUT
// changing it. Reporting and repairing are separate because a composer wants
// to show a person what is wrong with their own edit, and a reader of an
// untrusted proposal wants it repaired -- the same rules, two different
// responses (see sanitizeArrangement).
export function arrangementProblems(
  arrangement: Arrangement,
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): readonly ArrangementProblem[] {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const byId = new Map(library.map((element) => [element.id, element]));
  const fields = new Set(profile.fields.map((f) => f.field));
  const seen = new Set<string>();
  const out: ArrangementProblem[] = [];

  // The section dimension first: it is a property of the whole arrangement,
  // so it is reported at -1 rather than against an entry that did not cause
  // it.
  if (arrangement.layout !== undefined && !SECTION_LAYOUTS.includes(arrangement.layout)) {
    out.push({
      at: -1,
      element: "",
      fault: "unknown-layout",
      detail: `There is no layout called ${arrangement.layout} in this build; the section reads as a stack.`,
    });
  }

  arrangement.elements.forEach((entry, at) => {
    const element = byId.get(entry.element);
    if (element === undefined) {
      out.push({
        at,
        element: entry.element,
        fault: "unknown-element",
        detail: `There is no element called ${entry.element} in this build.`,
      });
      return;
    }
    const missingModule = unknownModule(entry, options);
    if (missingModule !== undefined) {
      out.push({ at, element: entry.element, fault: "unknown-module", detail: missingModule });
      return;
    }
    if (entry.role !== undefined && !expresses(element, entry.role)) {
      out.push({
        at,
        element: entry.element,
        fault: "role-unexpressible",
        detail:
          `${element.title} cannot be the ${entry.role} of a page, so it is laid ` +
          `out plainly. The element itself is unaffected.`,
      });
    }
    const key = `${entry.band}/${entry.element}`;
    if (seen.has(key)) {
      out.push({
        at,
        element: entry.element,
        fault: "duplicate",
        detail: `${element.title} already appears in the ${entry.band} band.`,
      });
    }
    seen.add(key);

    const fit = fitElement(element, profile, elementOptions(entry));
    if (fit.verdict === "unfit") {
      out.push({
        at,
        element: entry.element,
        fault: "unfit",
        detail: explainFit(element, fit, profile),
      });
    }

    for (const [slot, named] of Object.entries(entry.bindings ?? {})) {
      for (const field of named) {
        if (fields.has(field)) continue;
        out.push({
          at,
          element: entry.element,
          fault: "unknown-field",
          detail:
            `${element.title} is bound to ${field} for ${slot}, and no row in ` +
            `this sample carries a field by that name.`,
        });
      }
    }
  });

  // Satisfiability is judged against what SURVIVES, so it runs last: a focus
  // whose only hero candidate is an unfit element is unsatisfiable, and
  // judging it before the entry loop would have called it fine.
  const surviving = arrangement.elements.filter(
    (_, at) => !out.some((p) => p.at === at && isFatalFault(p.fault)),
  );
  const unsatisfiable = layoutUnsatisfiable(arrangementLayout(arrangement), surviving, options);
  if (unsatisfiable !== undefined) {
    out.push({ at: -1, element: "", fault: "layout-unsatisfiable", detail: unsatisfiable });
  }

  return out;
}

// ArrangementOptions is the third parameter every function in this module
// takes, replacing the bare library array they used to.
//
// It exists because repair grew a second input that is NOT a property of the
// arrangement being repaired: a page's REQUIRED entries (epic memql#4661,
// living pages). A regeneration that dropped the machines list off the fleet
// page produced a valid arrangement of a page that no longer does its job, and
// the only thing that knows the page has a job is the page's manifest.
export interface ArrangementOptions {
  readonly library?: readonly ElementSpec[];
  // Entries the PAGE declares it cannot be without. Re-inserted by
  // sanitizeArrangement when a stored version or a model's reply dropped
  // them, matched on element id plus module option -- so a manifest can
  // require "the machines table" without pinning its band or its title, and a
  // person may still move and re-caption it.
  //
  // The stored row is NOT rewritten; like every other repair here, this
  // applies to the rendered value.
  readonly required?: readonly ArrangedElement[];
  // Which scene ids this build's scene registry carries. An entry naming one
  // outside the set is dropped -- the registry is CLOSED, so an arrangement
  // places a scene and can never define one.
  //
  // UNDEFINED MEANS "THIS HOST REGISTERS NO SCENES", which is the honest
  // default for view-kit itself: it must not import three.js, so it cannot
  // host a scene and every scene entry is unknown to it. A host that renders
  // scenes passes its registry.
  readonly scenes?: readonly string[];
  // The same, for the widget registry.
  readonly widgets?: readonly string[];
}

// The faults that REMOVE an entry. An unknown field or a duplicate is
// reported and kept -- both are recoverable states a person can see and fix,
// and silently editing somebody's saved view is worse than showing them a
// broken one. A role that cannot be expressed is likewise kept: the element
// was still a deliberate choice, only its emphasis was not honourable.
function isFatalFault(fault: ArrangementFault): boolean {
  return fault === "unknown-element" || fault === "unfit" || fault === "unknown-module";
}

function expresses(element: ElementSpec, role: EntryRole): boolean {
  return (element.roles ?? ENTRY_ROLES).includes(role);
}

// unknownModule reports a `scene` or `widget` entry naming a module this build
// does not carry, as a sentence. Returns undefined for every other entry.
function unknownModule(
  entry: ArrangedElement,
  options: ArrangementOptions,
): string | undefined {
  if (entry.element === SCENE_ELEMENT_ID) {
    const id = entry.options?.["sceneId"] ?? "";
    if (id === "") return "A scene element named no scene.";
    if (!(options.scenes ?? []).includes(id)) {
      return `There is no scene called ${id} in this build.`;
    }
    return undefined;
  }
  if (entry.element === WIDGET_ELEMENT_ID) {
    const id = entry.options?.["widgetId"] ?? "";
    if (id === "") return "A widget element named no widget.";
    if (!(options.widgets ?? []).includes(id)) {
      return `There is no widget called ${id} in this build.`;
    }
    return undefined;
  }
  return undefined;
}

// layoutUnsatisfiable answers "can this layout be honoured with these
// entries", as a sentence to show a person, or undefined when it can.
//
// Only two layouts can fail, and both fail for a structural reason rather
// than an aesthetic one:
//
//   focus needs something that can BE the hero. Its whole shape is one large
//     element and a column beside it; with nothing eligible there is no
//     large element and the layout is a one-column stack with extra CSS.
//   split needs a detail pane. "The list on the left and the list again on
//     the right" is not a split.
//
// dashboard, gallery and stack degrade gracefully with any entry mix -- an
// empty slot is an empty slot -- so they are never unsatisfiable.
function layoutUnsatisfiable(
  layout: SectionLayout,
  entries: readonly ArrangedElement[],
  options: ArrangementOptions,
): string | undefined {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const byId = new Map(library.map((element) => [element.id, element]));

  if (layout === "focus") {
    const eligible = entries.some((entry) => {
      const element = byId.get(entry.element);
      return element !== undefined && expresses(element, "hero");
    });
    if (!eligible) {
      return "No element here can carry a focus layout, so the section reads as a stack.";
    }
    return undefined;
  }

  if (layout === "split") {
    const hasRoll = entries.some((entry) => entry.band === "roll" && !isDetail(byId, entry));
    const hasDetail = entries.some((entry) => isDetail(byId, entry));
    if (!hasRoll || !hasDetail) {
      return "A split needs a list and a detail pane; this section has only one of them, so it reads as a stack.";
    }
    return undefined;
  }

  return undefined;
}

function isDetail(
  byId: ReadonlyMap<string, ElementSpec>,
  entry: ArrangedElement,
): boolean {
  return byId.get(entry.element)?.detail === true;
}

// sanitizeArrangement is the repair: drop what cannot render, keep everything
// else, and never invent an entry.
//
// A DEGRADED ENTRY SURVIVES. Only `unknown-element` and `unfit` remove an
// entry -- an unknown field or a duplicate is reported, not deleted, because
// both are recoverable states a person can see and fix, and silently editing
// somebody's saved view is worse than showing them a broken one.
//
// If everything is dropped the result is the deterministic proposal, so a
// caller reading a hopeless stored arrangement -- or a hopeless model reply --
// still gets a usable view rather than a blank page.
export function sanitizeArrangement(
  arrangement: Arrangement,
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): Arrangement {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const byId = new Map(library.map((element) => [element.id, element]));
  const problems = arrangementProblems(arrangement, profile, options);
  const fatal = new Set(problems.filter((p) => isFatalFault(p.fault)).map((p) => p.at));

  let kept: readonly ArrangedElement[] = arrangement.elements.filter((_, at) => !fatal.has(at));

  // ROLES THE ELEMENT CANNOT EXPRESS ARE DROPPED FROM THE RENDERED VALUE, not
  // from the stored one. The entry survives -- somebody chose that element --
  // and it simply lays out plainly.
  kept = kept.map((entry) =>
    entry.role !== undefined && !expresses(byId.get(entry.element)!, entry.role)
      ? stripRole(entry)
      : entry,
  );

  // The page's REQUIRED entries. Re-inserted before the empty check, because a
  // version that dropped everything but still belongs to a page with required
  // entries should come back as that page rather than as the deterministic
  // proposal for its concept.
  kept = withRequired(kept, options.required ?? [], profile, options);

  if (kept.length === 0) return proposeArrangement(profile, options);

  // Now the layout, judged against what survived.
  let layout = arrangementLayout(arrangement);
  if (layoutUnsatisfiable(layout, kept, options) !== undefined) {
    layout = "stack";
  } else if (layout === "focus" && !kept.some((entry) => entryRole(entry) === "hero")) {
    // PROMOTE RATHER THAN DEMOTE. A focus with no hero is a layout somebody
    // asked for that is one annotation short of working; the best-fitting
    // hero-capable entry becomes the hero. Falling back to stack here would
    // discard a deliberate choice over a missing default.
    //
    // "Best-fitting" is the library's own ranking over this profile, so the
    // promotion agrees with what the deterministic proposal would have picked
    // and is not a second opinion about the concept.
    kept = promoteHero(kept, profile, options);
  }

  const out: Arrangement = {
    conceptId: arrangement.conceptId,
    ...(layout === "stack" ? {} : { layout }),
    elements: kept,
  };
  return out;
}

function stripRole(entry: ArrangedElement): ArrangedElement {
  const { role: _dropped, ...rest } = entry;
  return rest;
}

// withRequired re-inserts a page's required entries, matched on element id
// plus module option.
//
// MATCHING IS DELIBERATELY LOOSE. A manifest requires "the machines table",
// not "the machines table in the roll band captioned Machines" -- a person who
// moved it or re-captioned it has not removed it, and re-inserting a second
// copy because the title changed would be worse than the drop this guards
// against. Band, title, bindings and role are all the arrangement's business.
function withRequired(
  entries: readonly ArrangedElement[],
  required: readonly ArrangedElement[],
  profile: ConceptProfile,
  options: ArrangementOptions,
): readonly ArrangedElement[] {
  if (required.length === 0) return entries;
  const key = (entry: ArrangedElement): string =>
    `${entry.element}/${entry.options?.["sceneId"] ?? ""}/${entry.options?.["widgetId"] ?? ""}`;
  const have = new Set(entries.map(key));
  const missing = required.filter((entry) => !have.has(key(entry)));
  if (missing.length === 0) return entries;

  // A required entry that cannot render is NOT force-fed. The manifest says
  // the page needs this element; it does not overrule the fitness contract,
  // and inserting an unfit element would put view-kit's "cannot render, here
  // is why" sentence on a page as though somebody had chosen it.
  const renderable = missing.filter((entry) => {
    const problems = arrangementProblems(
      { conceptId: profile.concept.id, elements: [entry] },
      profile,
      options,
    );
    return !problems.some((p) => isFatalFault(p.fault));
  });
  return [...entries, ...renderable];
}

// promoteHero marks the best-fitting hero-capable entry as the hero, using the
// library's own ranking so the choice agrees with proposeArrangement's.
function promoteHero(
  entries: readonly ArrangedElement[],
  profile: ConceptProfile,
  options: ArrangementOptions,
): readonly ArrangedElement[] {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const byId = new Map(library.map((element) => [element.id, element]));
  const rank = new Map(
    elementCandidates(profile, library).map((c, index) => [c.element.id, index]),
  );
  let bestAt = -1;
  let bestRank = Number.POSITIVE_INFINITY;
  entries.forEach((entry, at) => {
    const element = byId.get(entry.element);
    if (element === undefined || !expresses(element, "hero")) return;
    const r = rank.get(entry.element) ?? Number.POSITIVE_INFINITY;
    if (r < bestRank) {
      bestRank = r;
      bestAt = at;
    }
  });
  if (bestAt === -1) return entries;
  return entries.map((entry, at) => (at === bestAt ? { ...entry, role: "hero" as const } : entry));
}

// ---------------------------------------------------------------------------
// Reading an untrusted arrangement
// ---------------------------------------------------------------------------

export interface ArrangementProposal {
  readonly arrangement: Arrangement;
  // Why the proposer arranged it this way, in its own words. Empty when it
  // said nothing -- a proposal with no reasoning is still a proposal.
  readonly reasoning: string;
  // Everything that had to be corrected on the way in. Shown rather than
  // swallowed: a model that keeps proposing a map for a concept with no
  // coordinates is a fact about the prompt, and hiding it hides the bug.
  readonly problems: readonly ArrangementProblem[];
}

// readArrangement parses an untrusted object into an Arrangement.
//
// UNTRUSTED IS THE OPERATIVE WORD. The structured-output path constrains a
// reply to a JSON schema; it does not constrain it to elements that exist or
// bindings that fit, and a schema is a promise made by a provider rather than
// a guarantee made by this process. So the reply is read field by field,
// coerced, and then put through exactly the validation a hand-built
// arrangement goes through. A model cannot produce a view a person could not
// have built by hand, which is the property that makes the AI path safe to
// leave switched on and safe to switch off.
//
// It never throws. A reply that is null, a string, an array, or an object
// with nothing recognisable in it yields the deterministic proposal with the
// problems that led there -- which is the same outcome as no model at all.
export function readArrangement(
  raw: unknown,
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): ArrangementProposal {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const conceptId = profile.concept.id;
  const object = isRecord(raw) ? raw : undefined;
  const reasoning = typeof object?.["reasoning"] === "string" ? object["reasoning"] : "";
  const rawElements = Array.isArray(object?.["elements"]) ? object["elements"] : [];

  const byId = new Map(library.map((element) => [element.id, element]));
  const entries: ArrangedElement[] = [];
  const problems: ArrangementProblem[] = [];

  rawElements.forEach((item, index) => {
    if (!isRecord(item)) {
      problems.push({
        at: index,
        element: "",
        fault: "unknown-element",
        detail: "The proposal contained an entry that was not an object.",
      });
      return;
    }
    const id = typeof item["element"] === "string" ? item["element"] : "";
    const element = byId.get(id);
    if (element === undefined) {
      problems.push({
        at: index,
        element: id,
        fault: "unknown-element",
        detail: `The proposal named ${id || "an element with no name"}, which this build does not have.`,
      });
      return;
    }
    // A band the proposer got wrong is corrected to the element's declared
    // one rather than rejected: the element knows which question it answers,
    // and honouring a wrong band would put a stat strip under "which ones".
    const band = readBand(item["band"]) ?? elementBand(element);
    const title = typeof item["title"] === "string" && item["title"] !== "" ? item["title"] : undefined;
    const bindings = readBindings(item["bindings"]);
    // An unrecognised role is DROPPED rather than corrected to standard --
    // which is the same value, but says the difference between "the model
    // said standard" and "the model said something we do not have".
    const role = ENTRY_ROLES.find((r) => r === item["role"]);
    const moduleOptions = readModuleOptions(item["options"]);

    entries.push({
      element: element.id,
      band,
      ...(title === undefined ? {} : { title }),
      ...(bindings === undefined ? {} : { bindings }),
      ...(role === undefined ? {} : { role }),
      ...(moduleOptions === undefined ? {} : { options: moduleOptions }),
    });
  });

  // A layout the reply named that this build does not have is carried through
  // UNCHANGED into the parsed value, so arrangementProblems reports it as
  // `unknown-layout` and the person is told. Silently correcting it here would
  // make a model that keeps naming a layout we removed indistinguishable from
  // one that always says stack.
  const layout = typeof object?.["layout"] === "string" ? object["layout"] : undefined;
  const parsed: Arrangement = {
    conceptId,
    ...(layout === undefined ? {} : { layout: layout as SectionLayout }),
    elements: entries,
  };
  const validated = arrangementProblems(parsed, profile, options);
  const arrangement = sanitizeArrangement(parsed, profile, options);

  return { arrangement, reasoning, problems: [...problems, ...validated] };
}

// readModuleOptions reads the `scene`/`widget` option map. String values only:
// the two keys it carries name registered modules, and a non-string there is
// not a module id under any coercion worth writing.
function readModuleOptions(
  value: unknown,
): Readonly<Record<string, string>> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, string> = {};
  for (const [key, raw] of Object.entries(value)) {
    if (typeof raw === "string" && raw !== "") out[key] = raw;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function readBand(value: unknown): BandRole | undefined {
  return BAND_ROLES.find((band) => band === value);
}

function readBindings(
  value: unknown,
): Readonly<Record<string, readonly string[]>> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, readonly string[]> = {};
  for (const [slot, named] of Object.entries(value)) {
    if (typeof named === "string") {
      out[slot] = [named];
      continue;
    }
    if (!Array.isArray(named)) continue;
    // An empty list survives: it is the fitness contract's way of declining a
    // slot, not a missing value (see ElementOptions.bindings).
    out[slot] = named.filter((f): f is string => typeof f === "string");
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// ---------------------------------------------------------------------------
// What a model is asked, and what it may answer
// ---------------------------------------------------------------------------

// arrangementRequest is everything a proposer needs and nothing it does not.
//
// IT IS DERIVED FROM THE PROFILE AND THE FITS, NOT FROM THE ROWS. No row
// values are in it -- a model asked to arrange a view has no business reading
// the data, and a payload of rows would put whatever the concept holds
// (someone's email, an audit trail) in front of a provider for a layout
// decision. Field names, kinds and cardinalities are the whole of what the
// question needs.
//
// The candidate list is the deterministic answer already computed, with each
// element's own explanation attached. The model is therefore CHOOSING AMONG
// OFFERS rather than inventing elements, which is what keeps its output
// inside the space a person could have reached by hand.
export interface ArrangementRequest {
  readonly concept: { readonly id: string; readonly entity: string };
  readonly rowCount: number;
  readonly fields: readonly {
    readonly field: string;
    readonly kind: string;
    readonly present: number;
    readonly distinct: number;
    readonly slots: readonly string[];
  }[];
  readonly bands: readonly { readonly band: BandRole; readonly question: string }[];
  readonly candidates: readonly {
    readonly element: string;
    readonly title: string;
    readonly summary: string;
    readonly band: BandRole;
    readonly verdict: string;
    readonly bindings: Readonly<Record<string, readonly string[]>>;
    readonly explanation: string;
  }[];
  // The arrangement the system already built. The model is asked to improve
  // on a working answer, not to produce one from nothing -- so a reply that
  // adds nothing is a reply that costs nothing.
  readonly baseline: Arrangement;
}

export function arrangementRequest(
  profile: ConceptProfile,
  options: ArrangementOptions = {},
): ArrangementRequest {
  const library = options.library ?? VIEW_KIT_ELEMENTS;
  const candidates = elementCandidates(profile, library);
  return {
    concept: { id: profile.concept.id, entity: profile.concept.entity },
    rowCount: profile.rowCount,
    fields: profile.fields.map((f) => ({
      field: f.field,
      kind: f.kind,
      present: f.present,
      distinct: f.distinct,
      slots: [...f.slots],
    })),
    bands: BAND_ROLES.map((band) => ({ band, question: bandQuestion(band) })),
    candidates: candidates
      .filter((c) => c.usable)
      .map((c) => ({
        element: c.element.id,
        title: c.element.title,
        summary: c.element.summary,
        band: c.band,
        verdict: c.fit.verdict,
        bindings: slotBindings(c.element, c.fit),
        explanation: c.explanation,
      })),
    baseline: proposeArrangement(profile, options),
  };
}

function slotBindings(
  element: ElementSpec,
  fit: ElementFit,
): Readonly<Record<string, readonly string[]>> {
  const out: Record<string, readonly string[]> = {};
  for (const req of element.requires) {
    const fields = boundFields(fit, req.slot);
    if (fields.length > 0) out[req.slot] = fields;
  }
  return out;
}

function bandQuestion(band: BandRole): string {
  // Indexed rather than imported as a whole so a band added to BAND_ROLES
  // without a question still yields something rather than `undefined`.
  const questions: Readonly<Record<string, string>> = {
    reading: "How many are there?",
    shape: "How does that divide?",
    roll: "Which ones, specifically?",
  };
  return questions[band] ?? band;
}

// ARRANGEMENT_PROPOSAL_SCHEMA is the JSON Schema a structured-output call
// enforces on the reply (ChatStructuredProvider.CallChatStructured, via the
// engine's suggest path). Kept here, beside readArrangement, so the shape the
// provider enforces and the shape this module parses cannot drift: they are
// one file apart and one test asserts they agree.
//
// Deliberately SMALL. Element ids are not enumerated in the schema even
// though they could be -- the enum would be baked into whatever rendered the
// prompt, and an element added to the library would need it regenerated.
// readArrangement rejects an unknown id at parse time instead, which is the
// same outcome one layer later and does not go stale.
export const ARRANGEMENT_PROPOSAL_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["elements", "reasoning"],
  properties: {
    reasoning: {
      type: "string",
      description:
        "One or two sentences on why this arrangement suits these rows. Written for the person composing the view.",
    },
    layout: {
      type: "string",
      enum: [...SECTION_LAYOUTS],
      description:
        "How the section places its bands. Omit for a plain vertical stack, which is always a correct answer.",
    },
    elements: {
      type: "array",
      description: "The elements of the view, in the order they should be read.",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["element", "band"],
        properties: {
          element: {
            type: "string",
            description: "The id of one of the offered candidate elements.",
          },
          band: {
            type: "string",
            enum: [...BAND_ROLES],
            description: "Which question this element answers.",
          },
          title: {
            type: "string",
            description: "An optional caption for the band, when the element's own title is not the clearest thing to say.",
          },
          bindings: {
            type: "object",
            description:
              "Optional per-slot field overrides: slot name -> the field names to use. An empty list declines the slot.",
            additionalProperties: { type: "array", items: { type: "string" } },
          },
          role: {
            type: "string",
            enum: [...ENTRY_ROLES],
            description:
              "How much of the page this element carries. At most ONE hero per section -- it is the element the page is about. Omit for standard.",
          },
        },
      },
    },
  },
} as const;
