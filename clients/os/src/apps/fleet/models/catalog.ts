// The curated catalog, joined against what this fleet actually serves
// (epic memql#5137, task memql#5140).
//
// ===========================================================================
// THIS ANSWERS A DIFFERENT QUESTION FROM THE LIST ABOVE IT
// ===========================================================================
// `ordering.ts` answers "what WILL be used", ranked in the order the router
// picks from. This answers "what SHOULD this fleet run, and what can it not
// run -- and why". The second question is the one an operator has when a
// capability is missing, and the ranked list cannot answer it at all: a model
// nobody pulled is not IN the ranked list, so its absence is silent.
//
// ===========================================================================
// THE GAP IS THE INFORMATION
// ===========================================================================
// A full alphabetical dump of the catalog beside a full dump of the fleet
// leaves the reader to diff two lists. What they came for is the difference,
// so the join computes it: every profile is either SERVED (and by which
// machines) or BLOCKED with one of three reasons, and every fleet model the
// catalog does not know is reported rather than hidden.
//
// ===========================================================================
// EXACT ID EQUALITY, NEVER A FUZZY MATCH
// ===========================================================================
// The model id is byte-identical from the cockpit's label through the catalog
// row to a policy naming `fleet:<modelId>`, and that is deliberate so this
// comparison can be a string equality. `qwen3.5:9b` and `qwen3.5:9b-q4` are
// different models -- different weights, possibly different capabilities --
// and matching one to the other would tell an operator their fleet serves a
// model it does not have.

/** One `v1:models:modelProfile` row, as the surface reads it. */
export interface ModelProfile {
  modelId: string;
  category: string;
  runtime: string;
  family: string;
  params: number;
  quant: string;
  sizeBytes: number;
  contextWindow: number;
  /** The capability names that are TRUE. Absence is false. */
  flags: string[];
  dimensions: number;
  license: string;
  recommendedFor: string[];
  minMachineClass: string;
  offeredOn: string[];
  notes: string;
  curated: boolean;
  unavailable: boolean;
}

/** What a machine on this fleet reports, reduced to what the join needs. */
export interface FleetMachineFacts {
  name: string;
  runtimes: string[];
  online: boolean;
  /** macos | linux | "" when the machine has not said. */
  platform: string;
  /** Unified memory or VRAM in gigabytes; 0 when the machine has not said. */
  memoryGb: number;
}

/** A fleet model, reduced to what the join needs. */
export interface FleetModelFacts {
  modelId: string;
  online: boolean;
  machineCount: number;
}

export type BlockedKind = "no-machine-of-class" | "runtime-missing" | "not-offered-on-platform";

export interface BlockedReason {
  kind: BlockedKind;
  /** One sentence an operator can act on. Never an icon, never a code. */
  detail: string;
}

export interface CatalogRow {
  profile: ModelProfile;
  /** True when a fleet model matches this profile's id exactly. */
  served: boolean;
  /** Machine names serving it, empty when not served. */
  servedBy: string[];
  /**
   * Why this fleet cannot serve it, or null.
   *
   * NULL WHEN SERVED, and also null when the fleet COULD serve it and simply
   * has not pulled it -- those are different states and only the second is an
   * invitation. A blocked profile is one no machine here could run even after
   * a pull, which is the only case where "why" is a question with an answer.
   */
  blocked: BlockedReason | null;
  /**
   * Whether the machine-class floor could actually be CHECKED for this row.
   *
   * FALSE IS NOT "TOO SMALL" AND IT IS NOT "BIG ENOUGH" -- it is "no machine
   * on this fleet has reported its memory". The row is deliberately not
   * blocked in that state, because guessing would tell an operator their
   * machine is too small when the truth is nobody has asked it yet.
   *
   * IT CAN NOW BE TRUE, which it could not before memql#5195. The three links
   * are wired: the fleetModel row's machine entries carry `memoryGb` and
   * `platform` (component/memql/fleet_catalog_read.go, stamped in
   * integrations/agent/worker/fleet_inference.go from the hardware epic
   * memql#5146's scanner writes), `useInference.ts` parses them onto
   * `CatalogMachine`, and `machineFactsFrom` folds them here. So this flag now
   * separates a fleet that has genuinely said nothing -- a cockpit predating the
   * scanner -- from one that has, rather than describing every fleet there is.
   *
   * IT IS STILL NOT THE SAME QUESTION AS "does it fit". A fleet that reported
   * and is under every rung has `classKnown: true` and a `no-machine-of-class`
   * block; only silence reads false. Collapsing the two would tell somebody with
   * an 8 GB laptop that their machine has not reported, which it has.
   *
   * `blocked: null` alone made the SENTENCE claim the opposite: an uncheckable
   * row counted toward "N of them run on a machine you already have", which is a
   * positive claim about somebody's hardware built out of the absence of data
   * about it. This flag is how the sentence tells the two apart.
   */
  classKnown: boolean;
}

/** A model the fleet serves that the catalog has never heard of. */
export interface UncataloguedModel {
  modelId: string;
  online: boolean;
  machineCount: number;
}

export interface CatalogReading {
  rows: CatalogRow[];
  /**
   * Fleet models with no catalog entry.
   *
   * REPORTED, NOT HIDDEN. An operator who pulled a model by hand is entitled
   * to see it acknowledged; a page that silently omitted it would read as
   * though the pull had failed.
   */
  uncatalogued: UncataloguedModel[];
}

/** Machine classes in ascending order. minMachineClass is a FLOOR. */
const MACHINE_CLASSES = ["16", "24", "32", "64", "128"];

function classIndex(value: string): number {
  return MACHINE_CLASSES.indexOf(value);
}

/**
 * What this fleet's machines have said about their memory.
 *
 * TWO QUESTIONS, NOT ONE, AND THEY WERE COLLAPSED. The previous reading was a
 * single class index where -1 meant both "nobody has reported" and "somebody
 * reported and it is under the smallest rung" -- fine while no machine reported
 * anything, and a live defect the moment one did: an 8 GB laptop that answered
 * perfectly would have been told "your machines have not reported their memory
 * yet". The engine has never had them confused (component/memql/fleet_class.go
 * calls them ClassUnknown and ClassUnsupported, and its file comment is about
 * exactly this), and now neither do we.
 *
 * `known` is the one that decides whether the floor can be CHECKED. `classIndex`
 * of -1 under `known: true` is a real, checkable answer -- a fleet too small for
 * every entry that names a size -- and it blocks, where unknown blocks nothing.
 */
interface FleetMemory {
  /** Whether ANY machine has reported its memory. False blocks nothing. */
  known: boolean;
  /** The largest usable figure reported, in GB. 0 when `known` is false. */
  largestGb: number;
  /** Index into MACHINE_CLASSES; -1 when known and under the smallest rung. */
  classIndex: number;
}

/**
 * Read the fleet's memory.
 *
 * A MACHINE THAT HAS NOT SAID DOES NOT COUNT AS SMALL. It counts as unknown, and
 * an unknown fleet blocks nothing -- reporting "no machine of this class" to
 * somebody whose 64 GB laptop simply has not reported its memory yet would be
 * confidently wrong, and the operator has no way to tell that from the truth.
 */
function fleetMemory(machines: FleetMachineFacts[]): FleetMemory {
  let largestGb = 0;
  for (const m of machines) {
    if (m.memoryGb > largestGb) largestGb = m.memoryGb;
  }
  if (largestGb <= 0) return { known: false, largestGb: 0, classIndex: -1 };

  let classIndex = -1;
  for (let i = MACHINE_CLASSES.length - 1; i >= 0; i--) {
    if (largestGb >= Number(MACHINE_CLASSES[i])) {
      classIndex = i;
      break;
    }
  }
  return { known: true, largestGb, classIndex };
}

function platforms(machines: FleetMachineFacts[]): Set<string> {
  const out = new Set<string>();
  for (const m of machines) {
    if (m.platform) out.add(m.platform);
  }
  return out;
}

function runtimes(machines: FleetMachineFacts[]): Set<string> {
  const out = new Set<string>();
  for (const m of machines) {
    for (const r of m.runtimes) out.add(r);
  }
  return out;
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

/**
 * Join the catalog against the fleet.
 *
 * The three blocked reasons are checked in the order an operator would act on
 * them -- platform first (nothing to be done), then runtime (installable),
 * then machine class (needs hardware) -- so the sentence they read names the
 * cheapest thing that would fix it last.
 */
export function joinCatalog(
  profiles: ModelProfile[],
  fleetModels: FleetModelFacts[],
  machines: FleetMachineFacts[],
): CatalogReading {
  const byId = new Map<string, FleetModelFacts>();
  for (const m of fleetModels) {
    if (m.modelId) byId.set(m.modelId, m);
  }

  const fleetPlatforms = platforms(machines);
  const fleetRuntimes = runtimes(machines);
  const memory = fleetMemory(machines);

  const rows: CatalogRow[] = profiles.map((profile) => {
    const hit = byId.get(profile.modelId);
    if (hit) {
      const servedBy = machines
        .filter((m) => m.runtimes.includes(profile.runtime))
        .map((m) => m.name)
        .filter((n) => n !== "");
      // A served row needs no floor check: the fleet is demonstrably running
      // it, which outranks any comparison against a reported class.
      return { profile, served: true, servedBy, blocked: null, classKnown: true };
    }

    // Not pulled. Could this fleet run it at all?
    if (profile.offeredOn.length > 0 && fleetPlatforms.size > 0) {
      const anyMatch = profile.offeredOn.some((os) => fleetPlatforms.has(os));
      if (!anyMatch) {
        const where = profile.offeredOn.join(" or ");
        return {
          profile,
          served: false,
          servedBy: [],
          blocked: {
            kind: "not-offered-on-platform",
            detail: `Runs on ${where}. No machine on your fleet is one.`,
          },
          classKnown: memory.known,
        };
      }
    }

    if (fleetRuntimes.size > 0 && !fleetRuntimes.has(profile.runtime)) {
      return {
        profile,
        served: false,
        servedBy: [],
        blocked: {
          kind: "runtime-missing",
          detail: `Needs the ${profile.runtime} runtime. No machine on your fleet has it installed.`,
        },
        classKnown: memory.known,
      };
    }

    const floor = classIndex(profile.minMachineClass);
    if (floor >= 0 && memory.known && memory.classIndex < floor) {
      return {
        profile,
        served: false,
        servedBy: [],
        blocked: {
          kind: "no-machine-of-class",
          // THE SECOND SENTENCE REPORTS THE MACHINE, NOT THE RUNG IT LANDED ON.
          // It used to print MACHINE_CLASSES[biggest], so a 48 GB Mac read "your
          // largest machine has 32 GB" -- true of the class and false of the
          // machine, and a lie the operator can check. The usable figure is what
          // the comparison actually ran on, and "for a model" is what makes 36
          // rather than 48 make sense: on unified memory the model gets 75
          // percent of the pool, and a reader told the raw number would
          // reasonably conclude the page is broken.
          //
          // It also covers the fleet that is under EVERY rung, which needs no
          // sentence of its own: classIndex -1 under known: true blocks here
          // and reads "Needs a 16 GB machine. Your largest has 6 GB for a model."
          detail: `Needs a ${profile.minMachineClass} GB machine. Your largest has ${memory.largestGb} GB for a model.`,
        },
        classKnown: true,
      };
    }

    // Could be served, and has not been pulled. Not blocked -- an invitation.
    // `classKnown` says whether that invitation rests on a checked floor or on
    // a fleet that has not reported its memory.
    return { profile, served: false, servedBy: [], blocked: null, classKnown: memory.known };
  });

  const known = new Set(profiles.map((p) => p.modelId));
  const uncatalogued: UncataloguedModel[] = fleetModels
    .filter((m) => m.modelId && !known.has(m.modelId))
    .map((m) => ({ modelId: m.modelId, online: m.online, machineCount: m.machineCount }));

  return { rows, uncatalogued };
}

/** The nine categories, in the order the surface shows them. */
export const CATEGORY_ORDER = [
  "text",
  "reasoning",
  "omni",
  "vision",
  "audioIn",
  "audioOut",
  "imageGen",
  "videoGen",
  "embeddings",
] as const;

/** What each category is called on screen, in the reader's words. */
export const CATEGORY_LABEL: Record<string, string> = {
  text: "Everyday work",
  reasoning: "Hard problems",
  omni: "Everything at once",
  vision: "Seeing",
  audioIn: "Listening",
  audioOut: "Speaking",
  imageGen: "Making images",
  videoGen: "Making video",
  embeddings: "Search and memory",
};

export interface CategoryGroup {
  category: string;
  label: string;
  /**
   * Every row in this category, unfiltered.
   *
   * THE SENTENCE IS COMPUTED FROM THIS, NOT FROM `shown`, and that is the whole
   * reason the two are separate. "Your fleet serves 2 of 5" describes the
   * CATEGORY; computing it over a filtered view would make it describe the
   * filter, so narrowing to "what this fleet lacks" would report 0 of 2 and
   * read as a fleet that serves nothing.
   */
  rows: CatalogRow[];
  /**
   * The rows to render, after any facets. Equal to `rows` when nothing is
   * narrowing.
   */
  shown: CatalogRow[];
  /** How many of this category's entries the fleet serves. */
  servedCount: number;
  /**
   * Whether this fleet has any machine at all.
   *
   * IT CHANGES WHAT AN UNPULLED ENTRY MEANS, which is why the group carries it
   * rather than the sentence guessing. With machines, "not pulled" is an
   * invitation one command away. With none, an unknown fleet blocks nothing --
   * so every entry reads as pullable, and saying "runs on a machine you already
   * have" to somebody who has paired nothing is a claim about hardware that
   * does not exist.
   */
  fleetHasMachines: boolean;
}

/**
 * Group the join by category, in CATEGORY_ORDER, dropping categories the
 * catalog has no entry for.
 *
 * A category with no entries is DROPPED rather than rendered empty: `vision`
 * has none by design -- the text models see, so a separate vision pull would
 * be a second copy of weights the fleet already holds -- and an empty heading
 * would read as a gap in the fleet rather than a decision about the catalog.
 */
export function groupByCategory(
  reading: CatalogReading,
  fleetHasMachines = true,
): CategoryGroup[] {
  const out: CategoryGroup[] = [];
  for (const category of CATEGORY_ORDER) {
    const rows = reading.rows.filter((r) => r.profile.category === category);
    if (rows.length === 0) continue;
    out.push({
      category,
      label: CATEGORY_LABEL[category] ?? category,
      rows,
      shown: rows,
      servedCount: rows.filter((r) => r.served).length,
      fleetHasMachines,
    });
  }
  return out;
}

/**
 * One sentence for a category group's scope line.
 *
 * IT NAMES THE STATE, not a count on its own. "3 of 4" tells a reader nothing
 * about whether that is fine; "Served by your fleet" and "Nothing here runs on
 * your fleet yet" are answers.
 */
export function categorySentence(group: CategoryGroup): string {
  if (group.servedCount === group.rows.length) return "Your fleet serves every recommendation here.";
  if (group.servedCount > 0) {
    return `Your fleet serves ${group.servedCount} of ${group.rows.length}.`;
  }
  // A FLEET WITH NO MACHINES GETS NO PER-CATEGORY SENTENCE AT ALL.
  //
  // The state is a property of the FLEET, not of each category, so saying it
  // per group printed "Pair a machine and these become available" nine times
  // down one screen -- rule 7, say it once. The section says it once above the
  // groups instead, and every group falls silent. The empty string is what the
  // surface checks for.
  //
  // It matters that this is not just repetition. Without a machine, every entry
  // is unblocked -- an unknown fleet blocks nothing -- so the honest per-group
  // sentence would have been "these run on a machine you already have", said to
  // somebody who has paired none.
  if (!group.fleetHasMachines) return "";
  const blocked = group.rows.filter((r) => r.blocked !== null).length;
  if (blocked === group.rows.length) {
    return "Nothing here runs on your fleet, and each entry says why.";
  }
  // AN UNCHECKABLE FLOOR IS NOT A PASSED ONE. A row whose machine-class floor
  // could not be evaluated -- because no machine on this fleet has reported its
  // memory, which since memql#5195 means a cockpit that predates the hardware
  // scanner rather than every fleet there is -- is deliberately not blocked.
  // Counting it as pullable, though, turns "we could not check" into "it runs on
  // a machine you already have": a positive claim about somebody's hardware
  // built out of the absence of data about it, and wrong in the direction that
  // gets a 122B pull started on a laptop.
  //
  // So the sentence names the gap instead, and names the thing that would close
  // it, rather than reporting a count it cannot stand behind.
  //
  // The MEMORY half is a fact about the FLEET, not about this category, so the
  // section says it once above the list and the group says only its own half
  // (rule 7). Said per group it printed the same clause under every category
  // with a floor -- the same repetition the no-machines branch above exists to
  // avoid, arriving by a different route.
  const unknown = group.rows.filter((r) => r.blocked === null && !r.classKnown).length;
  if (unknown > 0) return "Nothing here is pulled yet.";
  const pullable = group.rows.length - blocked;
  return `Nothing here is pulled yet. ${pullable} of them ${plural(pullable, "runs", "run")} on a machine you already have.`;
}

/**
 * Whether any entry's machine-class floor could not be checked, because no
 * machine on this fleet has reported its memory.
 *
 * ASKED OF THE WHOLE READING, because it is a fact about the fleet: the answer
 * is the same under every category, and the section says it once rather than
 * each group repeating it (rule 7).
 */
export function hasUncheckableClass(groups: CategoryGroup[]): boolean {
  return groups.some((g) => g.rows.some((r) => r.blocked === null && !r.classKnown));
}

/**
 * What a Refine control on the Models Head can narrow the catalog by
 * (epic memql#5153's D3, seam agreed with that epic's session).
 *
 * EVERY FIELD IS OPTIONAL AND ABSENT MEANS "DO NOT NARROW", the same contract
 * the `modelProfiles` query's arguments carry. A facet set with nothing in it
 * is the ordinary state and must render exactly as no facets at all -- a
 * surface that behaved differently when handed an empty object would make
 * "the control is closed" and "the control is open with nothing chosen" two
 * different pages.
 */
export interface CatalogFacets {
  search?: string;
  /** One of the nine categories. */
  category?: string;
  /** One of the seven runtimes. */
  runtime?: string;
  /**
   * Show only entries this fleet cannot serve.
   *
   * BLOCKED, NOT MERELY UNPULLED. "What this fleet lacks" is the set with a
   * REASON -- no machine of the class, no runtime, wrong platform -- because
   * those are the entries a person can do nothing about from this page. An
   * entry that is simply not pulled yet is an invitation, and folding the two
   * together would put "run one command" in the same list as "buy hardware".
   */
  lackingOnly?: boolean;
}

/**
 * Narrow each group's `shown` rows by the facets, dropping groups left empty.
 *
 * It does NOT touch `rows`, `servedCount` or `fleetHasMachines`, so
 * `categorySentence` keeps describing the category rather than the filter.
 */
export function applyFacets(groups: CategoryGroup[], facets: CatalogFacets): CategoryGroup[] {
  const search = (facets.search ?? "").trim().toLowerCase();
  const category = (facets.category ?? "").trim();
  const runtime = (facets.runtime ?? "").trim();
  const lackingOnly = facets.lackingOnly === true;
  if (search === "" && category === "" && runtime === "" && !lackingOnly) return groups;

  const out: CategoryGroup[] = [];
  for (const group of groups) {
    if (category !== "" && group.category !== category) continue;
    const shown = group.shown.filter((row) => {
      if (search && !`${row.profile.modelId} ${row.profile.category} ${row.profile.runtime}`.toLowerCase().includes(search)) return false;
      if (runtime !== "" && row.profile.runtime !== runtime) return false;
      if (lackingOnly && row.blocked === null) return false;
      return true;
    });
    if (shown.length === 0) continue;
    out.push({ ...group, shown });
  }
  return out;
}
