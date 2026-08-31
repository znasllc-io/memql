import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// What the Deployables app is about: the two populations it reads and the
// closed lists it owns.
//
// Naming a concept id here is what a DESIGNED surface does -- this app is
// about sites, and about the Library's zip artifacts specifically, not about
// whatever concept a browser happened to be pointed at.

/** The deployables themselves (`dsl/platform/concepts.memql`). */
export const SITE_CONCEPT = "v1:platform:site";

/**
 * The Library index, for the publish picker ONLY. A deployable's bytes come
 * from a zip the caller already owns; this app never uploads.
 *
 * The Artifacts app (#4721) reads the same concept, and the two share nothing
 * but `kit/` -- no cross-app import, so either app stays independently
 * deletable.
 */
export const ARTIFACT_CONCEPT = "v1:library:artifact";

// ---------------------------------------------------------------------------
// The kinds
// ---------------------------------------------------------------------------

export interface DeployableKind {
  value: string;
  label: string;
  blurb: string;
}

/**
 * The three values `v1:platform:site.kind` declares, with what each one means
 * for the request that misses.
 *
 * ANDROID, iOS AND macOS ARE DELIBERATELY ABSENT, and that is not an omission
 * to be fixed. They are artifact DISTRIBUTION -- stores, TestFlight,
 * notarisation -- not hostname-resolved web surfaces, so there is nothing for
 * the edge to resolve; `TestSiteKindEnumIsExactlyThreeValues` pins the enum to
 * exactly these three precisely so the next addition is a decision rather than
 * a drift. The create form says so in a caption rather than offering three
 * disabled controls: a control nobody can use is still a control everybody has
 * to read past.
 */
export const DEPLOYABLE_KINDS: readonly DeployableKind[] = [
  {
    value: "spa",
    label: "Single-page app",
    blurb:
      "Any path the bundle does not have falls back to index.html, so client-side routing works.",
  },
  {
    value: "static",
    label: "Website",
    blurb: "A mistyped path answers 404 rather than silently rendering the home page.",
  },
  {
    value: "shopify_storefront",
    label: "Shopify storefront",
    blurb:
      "A single-page app bound to a Shopify store. Checkout stays Shopify's own hosted checkout.",
  },
];

export const STOREFRONT_KIND = "shopify_storefront";

export function kindLabel(kind: string): string {
  return DEPLOYABLE_KINDS.find((k) => k.value === kind)?.label ?? kind;
}

// ---------------------------------------------------------------------------
// The Library side of the publish picker
// ---------------------------------------------------------------------------

/** The one artifact kind whose backing row has bytes (memql#4340). */
export const FILE_KIND = "file";

/**
 * The zip MIME types the publish capability accepts, mirroring
 * `sitePublishZipMimeTypes` (integrations/library/site_publish.go).
 *
 * MIRRORED, NOT AUTHORITATIVE. The server refuses a non-zip artifact by name
 * (`artifact_not_a_zip`) whatever this list says; the list is here so the
 * picker offers only artifacts that stand a chance, rather than letting
 * somebody choose a PDF and learn about it from a refusal.
 * `application/octet-stream` is absent for the reason it is absent
 * server-side: it is what an upload with no usable type falls back to, so
 * accepting it would turn "must be a zip" into "must be anything".
 */
const ZIP_MIME_TYPES = new Set([
  "application/zip",
  "application/x-zip",
  "application/x-zip-compressed",
  "application/zip-compressed",
  "multipart/x-zip",
]);

/**
 * Whether a Library index row is a publishable bundle.
 *
 * `archived === true` rather than truthiness: a row promoted before the field
 * existed carries no `archived` member at all, so the two forms disagree about
 * exactly those rows -- the same trap `libraryArtifacts`' own `archived!=true`
 * filter documents on the SQL side.
 */
export function isZipArtifact(row: Row): boolean {
  if (row["archived"] === true) return false;
  if (rowString(row, "kind") !== FILE_KIND) return false;
  return ZIP_MIME_TYPES.has(rowString(row, "mimeType").trim().toLowerCase());
}
