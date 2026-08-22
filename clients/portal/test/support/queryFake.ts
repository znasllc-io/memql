import {
  QueryClient,
  type Concept,
  type ConceptRegistryDelta,
  type ConceptRegistryFollow,
} from "@znasllc-io/memql-sdk-core/client";

// asQueryClient turns a plain stub object into a QueryClient the app code
// under test cannot tell from the real one (memql#4232).
//
// The generated typed methods live on QueryClient.PROTOTYPE and every one of
// them dispatches through this.executeNamed -- so a fake built as a bare
// object literal satisfies the type via a cast but has none of the generated
// methods at runtime, and `query.campaigns({})` explodes the moment a hook is
// migrated onto the typed surface. Re-parenting the stub onto the real
// prototype keeps the fake at the honest seam: tests keep stubbing
// executeNamed (the wire boundary), while the REAL generated builders run
// above it -- which means a test now also exercises the composed call string
// it asserts on, instead of a hand-typed copy of it.
export function asQueryClient<T extends object>(stub: T): QueryClient & T {
  const s = stub as Record<string, unknown>;
  // Bridge fakes written against the OLD one-shot registry read (memql#4238):
  // useConcepts now consumes the follow-mode delta stream
  // (QueryClient.subscribeConceptRegistry), so a stub that only implements
  // listConcepts gets a synthesized subscribeConceptRegistry that delivers that
  // list as a single reset snapshot. A stub that drives deltas itself (the
  // registry-delta hook test) provides its own and is left untouched.
  if (typeof s.subscribeConceptRegistry !== "function" && typeof s.listConcepts === "function") {
    s.subscribeConceptRegistry = (
      onDelta: (delta: ConceptRegistryDelta) => void,
    ): ConceptRegistryFollow => {
      let live = true;
      const list = s.listConcepts as () => Promise<Concept[]>;
      void Promise.resolve(list()).then((concepts) => {
        if (live) onDelta({ generation: 1, added: concepts, removed: [], reset: true });
      });
      return {
        unsubscribe: () => {
          live = false;
        },
      };
    };
  }
  // The NAV RAIL reads composed views now (memql#4264), so every test that
  // renders the shell makes this call whether or not it is about saved views.
  // Left to the prototype it would dispatch into the test's own executeNamed,
  // and a fake that answers every call with the same rows would fill the rail's
  // Custom section with them -- duplicating that test's fixture text into the
  // chrome and breaking assertions that have nothing to do with the composer.
  //
  // So the default is an EMPTY list. A test about saved views provides its own.
  if (typeof s.composedViews !== "function") {
    s.composedViews = async () => ({
      rows: () => [],
      rawNodes: () => [],
      single: () => null,
      meta: () => null,
    });
  }

  return Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient & T;
}
