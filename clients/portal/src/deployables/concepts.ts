import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// What this feature is about: the populations it names and the two closed
// lists it owns.
//
// Naming a concept id in a FEATURE module is not the concept-agnostic BROWSE
// machinery (src/concepts, src/components, src/viewkit) that
// portal_render_path_test.go holds to zero concept-id literals -- the same
// standing sites/concepts.ts and artifacts/concepts.ts documented before it. A
// designed surface is about a specific population and has to say which one.
export const SITE_CONCEPT_ID = "v1:platform:site";

// The Library index, for the deploy picker only. A deployable's bytes come
// from a zip artifact the caller already owns; this page never uploads.
export const ARTIFACT_CONCEPT_ID = "v1:library:artifact";

// ===========================================================================
// THE KIND LIST IS THE PORTAL'S, NOT THE SCHEMA'S (design D5)
// ===========================================================================
//
// The Sites form read `kind` off the row's `schema` intrinsic (through a
// concepts/schema.ts helper, since removed with its last caller), so a value
// added to the concept appeared in the selector with no UI edit. That was right when every value in
// the enum was a thing the edge could resolve. It stopped being right the
// moment the product had kinds it will not have SCHEMA for.
//
// Android, iOS and macOS are artifact DISTRIBUTION -- stores, TestFlight,
// notarisation -- not hostname-resolved web surfaces. There is nothing for the
// edge to resolve, so adding enum values for them would be a promise the
// engine cannot keep: `TestSiteKindEnumIsExactlyThreeValues` pins
// v1:platform:site.kind to exactly spa / static / shopify_storefront, and it
// pins it precisely so the next addition is a decision rather than a drift.
//
// So the three "coming soon" entries below exist ONLY here. Reading them from
// the enum is impossible by construction, and a test asserts they render
// anyway -- which is the property that tells a portal-side list apart from a
// schema-driven one.
//
// The three LIVE entries repeat the enum, and that repetition is checked, not
// assumed: deployables.test.tsx drives the create form from a schema fixture
// carrying exactly the three declared values and asserts the picker offers
// each of them.
export interface DeployableKind {
  // The `kind` value written to the row, or "" for a kind with no schema.
  value: string;
  label: string;
  blurb: string;
  // false renders the entry disabled with "coming soon" beside it.
  available: boolean;
}

export const DEPLOYABLE_KINDS: readonly DeployableKind[] = [
  {
    value: "spa",
    label: "Single-page app",
    blurb: "A client-routed bundle. Any path the bundle does not have falls back to index.html.",
    available: true,
  },
  {
    value: "static",
    label: "Website",
    blurb: "A plain multi-page site. A mistyped path answers 404 rather than silently rendering the home page.",
    available: true,
  },
  {
    value: "shopify_storefront",
    label: "Shopify storefront",
    blurb: "A single-page app bound to a Shopify store. Checkout stays Shopify's own hosted checkout.",
    available: true,
  },
  {
    value: "",
    label: "Android app",
    blurb: "Coming soon. An Android build is distributed through a store, not resolved at a hostname, so it is not a site.",
    available: false,
  },
  {
    value: "",
    label: "iOS app",
    blurb: "Coming soon. Distribution is TestFlight and the App Store, not a hostname this cluster answers for.",
    available: false,
  },
  {
    value: "",
    label: "macOS app",
    blurb: "Coming soon. Distribution is notarisation and a signed download, not a hostname this cluster answers for.",
    available: false,
  },
];

export const STOREFRONT_KIND = "shopify_storefront";

// The three lifecycle values. Not read from the schema either, and for a
// duller reason than `kind`: siteById's rows are shaped through siteFull, and
// a struct-form shape projects only the paths it names -- it does not carry
// the `schema` row intrinsic. The list is pinned by the concept.
export const SITE_STATUS_VALUES = ["draft", "live", "disabled"] as const;

// The artifact kind whose backing row has bytes (memql#4340). Anything else in
// the Library -- a note, a to-do, a memory -- has no file behind it and cannot
// be a bundle.
export const FILE_KIND = "file";

// The zip MIME types the publish capability accepts, mirroring
// sitePublishZipMimeTypes (integrations/library/site_publish.go).
//
// MIRRORED, NOT AUTHORITATIVE. The server refuses a non-zip artifact by name
// (`artifact_not_a_zip`) whatever this list says; the list is here so the
// picker offers only artifacts that stand a chance, rather than letting a
// person choose a PDF and learn about it from a refusal. `application/octet-
// stream` is deliberately absent for the same reason it is absent server-side:
// it is what an upload with no usable type falls back to, so accepting it
// would turn "must be a zip" into "must be anything".
const ZIP_MIME_TYPES = new Set([
  "application/zip",
  "application/x-zip",
  "application/x-zip-compressed",
  "application/zip-compressed",
  "multipart/x-zip",
]);

// isZipArtifact reports whether a Library index row is a deployable bundle:
// a file artifact whose MIME type is a zip, and not archived.
//
// `archived` is read as `=== true` rather than by truthiness because a row
// promoted before the field existed carries no `archived` member at all --
// the same trap libraryArtifacts' own `archived != true` filter documents on
// the SQL side. libraryArtifacts already excludes archived rows server-side;
// this second check costs nothing and keeps the predicate honest for a caller
// that hands it rows from anywhere else.
export function isZipArtifact(row: Row): boolean {
  if (row["archived"] === true) return false;
  if (rowString(row, "kind") !== FILE_KIND) return false;
  return ZIP_MIME_TYPES.has(rowString(row, "mimeType").trim().toLowerCase());
}
