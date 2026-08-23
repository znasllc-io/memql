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
