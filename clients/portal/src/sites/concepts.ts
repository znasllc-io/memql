// The sites concept id.
//
// Same decision integrations/concepts.ts makes: naming a concept id in a
// FEATURE module (this one) is not the concept-agnostic BROWSE machinery
// (src/concepts, src/components, src/viewkit) that portal_render_path_test.go
// holds to zero concept-id literals. A designed surface is about a specific
// population and has to say which one.
//
// There is no TS `Concepts.*` generated map to import instead (Makefile:364-370
// retired the TS SDK-gen target; sdk/ts/src/client/subscriptions.ts's comment
// pointing at one is stale -- see the #3717 recon). A local constant mirroring
// this one exists at every other portal feature's concepts module.
//
// No fallback-concept helper here (contrast integrations/concepts.ts's
// fallbackConcept): RowList.tsx's prop type is the FULL SDK `Concept`
// (version/domain/description/type), not view-kit's looser `ConceptLike`, and
// every existing RowList caller (ConceptRowsPane) waits for the real
// registry entry rather than inventing a partial one. SitesPage does the
// same -- see its Loading branch.
export const SITE_CONCEPT_ID = "v1:platform:site";
