// The artifacts concept id.
//
// Same decision sites/concepts.ts and integrations/concepts.ts make: naming
// a concept id in a FEATURE module (this one) is not the concept-agnostic
// BROWSE machinery (src/concepts, src/components, src/viewkit) that
// portal_render_path_test.go holds to zero concept-id literals -- a designed
// surface is about a specific population and has to say which one.
//
// No fallback-concept helper here either (contrast integrations/concepts.ts's
// fallbackConcept): RowList.tsx's prop type is the FULL SDK `Concept`, and
// every caller in this directory waits for the real registry entry (see
// ArtifactsPage / ArtifactDetailPage's loading branch) rather than inventing
// a partial one -- the same choice sites/concepts.ts documents and
// SitesPage.tsx makes.
export const ARTIFACT_CONCEPT_ID = "v1:library:artifact";

// The Library's own byte-bearing row (memql#4340), the sixth backing concept.
// Named here rather than in the hook for the same reason as above: this
// module is where this feature says which populations it is about.
export const LIBRARY_FILE_CONCEPT_ID = "v1:library:file";

// The artifact kind whose backing row carries bytes. `kind` is an open,
// appended enum on the index concept, so the page compares against this one
// value rather than switching over the set.
export const FILE_KIND = "file";

// Both members of `lens`, the index concept's required two-value enum
// (artifact | record). CLOSED, unlike `kind` above -- which is why the union
// of the two per-lens reads is the caller's WHOLE Library rather than a
// sample of it, and why useArtifacts can use it to reach the archived rows
// the default list filters out.
export const ARTIFACT_LENSES = ["artifact", "record"] as const;

// fileIdFromSourceRef recovers the backing file's id from an artifact index
// row's `sourceConceptRef`.
//
// THE INDEX ROW CARRIES NO fileId FIELD -- it is a pointer row (memql#693)
// and `sourceConceptRef` IS the pointer, written as the canonical
// `{concept}:{shortId}` the identifiers doc specifies
// (docs/public/concepts/identifiers.md). So the file's BARE id is the last
// segment, and the concept prefix is what says the pointer is a file at all.
//
// Returns "" for anything else, and every caller treats that as "this
// artifact has no file behind it" rather than guessing: a note, a to-do and a
// calendar event all have a perfectly good sourceConceptRef pointing at a
// concept with no bytes and no chunks, so training one is not a degraded
// case -- it is a different kind of row. Export still works for all of them
// (design D9), which is why only the TRAIN control consults this.
export function fileIdFromSourceRef(sourceConceptRef: string): string {
  const prefix = `${LIBRARY_FILE_CONCEPT_ID}:`;
  if (!sourceConceptRef.startsWith(prefix)) return "";
  return sourceConceptRef.slice(prefix.length).trim();
}
