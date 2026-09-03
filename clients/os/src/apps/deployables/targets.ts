import { SITE_STATUSES } from "./rows";

// The target registry: what the rail needs to know about a KIND of deployable.
//
// ===========================================================================
// ONE ENTRY, AND THE OTHER THREE ARE A TABLE RATHER THAN CODE
// ===========================================================================
// A target is everything a stop renders from -- the address's shape, the
// build surface, the live states, the row it lands on -- so the page has no
// branch on "which kind is this" (design section B). Today there is exactly
// one target, web, over the three kinds `v1:platform:site.kind` declares.
//
// iOS, Android and macOS are the design's table and NOT entries here. They
// are artifact distribution -- stores, TestFlight, notarisation -- with no
// hostname for the edge to resolve, no site row and no build surface this
// cluster has. An entry for one would be an entry the OS renders a control
// for, and a control nobody can use is still a control everybody has to read
// past. They are KNOWN, so the engine can say "iOS is not offered on this
// cluster yet" rather than "unknown kind", and that sentence is the whole of
// what this file holds about them.
//
// `OFFERED_KINDS` is written as ONE LINE on purpose: component/memql's
// site_kind_os_parity_test.go reads it with a regex and holds it equal to the
// site enum in both directions. A computed list, or one broken across lines,
// is one that gate cannot read, and it says so rather than passing.

export const OFFERED_KINDS = ["spa", "static", "shopify_storefront"] as const;

export type OfferedKind = (typeof OFFERED_KINDS)[number];

export interface DeployableKind {
  value: OfferedKind;
  label: string;
  blurb: string;
}

/**
 * The picker entries for the three offered kinds, with what each one means
 * for the request that misses. The `value` type is derived from
 * `OFFERED_KINDS`, so an entry for a kind the target does not offer is a type
 * error rather than a picker option with nothing behind it.
 */
export const DEPLOYABLE_KINDS: readonly DeployableKind[] = [
  {
    value: "spa",
    label: "Single-page app",
    blurb: "Any path the bundle does not have falls back to index.html, so client-side routing works.",
  },
  {
    value: "static",
    label: "Website",
    blurb: "A mistyped path answers 404 rather than silently rendering the home page.",
  },
  {
    value: "shopify_storefront",
    label: "Shopify storefront",
    blurb: "A single-page app bound to a Shopify store. Checkout stays Shopify's own hosted checkout.",
  },
];

export const STOREFRONT_KIND: OfferedKind = "shopify_storefront";

export function kindLabel(kind: string): string {
  return DEPLOYABLE_KINDS.find((k) => k.value === kind)?.label ?? kind;
}

// ---------------------------------------------------------------------------
// The kinds this cluster knows and does not offer
// ---------------------------------------------------------------------------

export const KNOWN_UNOFFERED_KINDS = ["ios", "android", "macos"] as const;

export type UnofferedKind = (typeof KNOWN_UNOFFERED_KINDS)[number];

/** How each unoffered kind is spelled when a sentence names it. */
export const UNOFFERED_KIND_LABELS: Readonly<Record<UnofferedKind, string>> = {
  ios: "iOS",
  android: "Android",
  macos: "macOS",
};

/**
 * The one sentence about the three, said once. The create form carries it
 * beneath the kind picker in place of three disabled controls.
 */
export const NOT_OFFERED_SENTENCE =
  "Android, iOS and macOS builds are not deployables. They are distributed through stores and " +
  "signed downloads rather than answered at a hostname, so this cluster's edge has nothing to " +
  "resolve for them.";

// ---------------------------------------------------------------------------
// The stops
// ---------------------------------------------------------------------------

/**
 * The five stops, in the rail's order. Named, not numbered: the order is a
 * law the pipeline enforces, which is what earns it a sequence, and a number
 * would only restate what the position already says.
 */
export const STOP_IDS = ["source", "whatItIs", "whereItLives", "build", "live"] as const;

export type StopId = (typeof STOP_IDS)[number];

export interface StopDef {
  id: StopId;
  label: string;
  /** What the stop is about, shown as its note until it has something to say. */
  blurb: string;
}

const WEB_STOPS: readonly StopDef[] = [
  { id: "source", label: "Source", blurb: "Where it comes from: a repository, a zip in Files, or your CI" },
  { id: "whatItIs", label: "What it is", blurb: "What deploying this source would do, read from the tree" },
  { id: "whereItLives", label: "Where it lives", blurb: "The address it answers at, and the client it is for" },
  { id: "build", label: "Build", blurb: "Turn the source into the files that get served" },
  { id: "live", label: "Live", blurb: "Point the address at the new files and serve them" },
];

// ---------------------------------------------------------------------------
// The registry
// ---------------------------------------------------------------------------

export interface Target {
  id: "web";
  /** The kinds this target is behind; a kind outside this list has no target. */
  offeredKinds: readonly OfferedKind[];
  /** The picker entries for those kinds. */
  kinds: readonly DeployableKind[];
  /** The Where-it-lives stop's shape. */
  address: {
    /** A slug, previewed at keystroke rate as `<slug>.<cluster domain>`. */
    slug: true;
    /** An own domain may be bound as well -- by a cluster owner only. */
    ownDomain: "cluster_owner";
  };
  /**
   * What the Build stop stands on. `prebuilt` today: the source carries its
   * built output and the stop reads skipped, with the reason. The workbench
   * arrives with the Build epic and changes what the stop SAYS -- progress
   * on the surface that built it, and the log -- not where the stop is.
   */
  buildSurface: "prebuilt";
  /** The Live stop's states, the site row's own. */
  liveStates: readonly string[];
  /** The row a deployable of this target lands on. */
  rowConcept: "v1:platform:site";
  stops: readonly StopDef[];
}

export const WEB_TARGET: Target = {
  id: "web",
  offeredKinds: OFFERED_KINDS,
  kinds: DEPLOYABLE_KINDS,
  address: { slug: true, ownDomain: "cluster_owner" },
  buildSurface: "prebuilt",
  liveStates: SITE_STATUSES,
  // The literal rather than an import of SITE_CONCEPT: concepts.ts re-exports
  // the kinds from here, and a module cycle between the two is a `const` read
  // before its initialiser in whichever order the bundler picks. The test
  // holds the two spellings equal.
  rowConcept: "v1:platform:site",
  stops: WEB_STOPS,
};

/** Every registered target. One, and the test pins that it stays one. */
export const TARGETS: readonly Target[] = [WEB_TARGET];

/**
 * The target behind a kind, or null. A known-but-unoffered kind answers null
 * exactly like an unknown one: knowing its display name is not the same as
 * having a target for it, and nothing renders a control for either.
 */
export function targetFor(kind: string): Target | null {
  const wanted = kind.trim();
  return TARGETS.find((t) => (t.offeredKinds as readonly string[]).includes(wanted)) ?? null;
}
