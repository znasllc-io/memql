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

import { VIEW_KIT_ELEMENTS } from "./elements.js";
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
}

export interface Arrangement {
  // The concept whose rows this arrangement is for. Carried so a stored
  // arrangement cannot be silently re-applied to a different row set, and so
  // a reader can profile the right concept before validating it.
  readonly conceptId: string;
  readonly elements: readonly ArrangedElement[];
}

export const EMPTY_ARRANGEMENT: Arrangement = { conceptId: "", elements: [] };

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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): Arrangement {
  const candidates = elementCandidates(profile, library);
  const elements: ArrangedElement[] = [];

  for (const band of BAND_ROLES) {
    const best = candidates.find((c) => c.band === band && c.usable);
    if (best !== undefined) {
      elements.push({ element: best.element.id, band });
      continue;
    }
    if (band === "roll") {
      const fallback = universalFallback(library);
      if (fallback !== undefined) elements.push({ element: fallback.id, band });
    }
  }

  return { conceptId: profile.concept.id, elements };
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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): readonly string[] {
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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): readonly ArrangementProblem[] {
  const byId = new Map(library.map((element) => [element.id, element]));
  const fields = new Set(profile.fields.map((f) => f.field));
  const seen = new Set<string>();
  const out: ArrangementProblem[] = [];

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

  return out;
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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): Arrangement {
  const fatal = new Set(
    arrangementProblems(arrangement, profile, library)
      .filter((p) => p.fault === "unknown-element" || p.fault === "unfit")
      .map((p) => p.at),
  );
  const kept = arrangement.elements.filter((_, at) => !fatal.has(at));
  if (kept.length === 0) return proposeArrangement(profile, library);
  return { conceptId: arrangement.conceptId, elements: kept };
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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): ArrangementProposal {
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

    entries.push({
      element: element.id,
      band,
      ...(title === undefined ? {} : { title }),
      ...(bindings === undefined ? {} : { bindings }),
    });
  });

  const parsed: Arrangement = { conceptId, elements: entries };
  const validated = arrangementProblems(parsed, profile, library);
  const arrangement = sanitizeArrangement(parsed, profile, library);

  return { arrangement, reasoning, problems: [...problems, ...validated] };
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
  library: readonly ElementSpec[] = VIEW_KIT_ELEMENTS,
): ArrangementRequest {
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
    baseline: proposeArrangement(profile, library),
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
        },
      },
    },
  },
} as const;
