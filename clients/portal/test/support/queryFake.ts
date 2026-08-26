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
  // THE FIRST-RUN GATE reads inferenceStatus on every authenticated render
  // (epic memql#4676), for the same reason composedViews is defaulted below:
  // a call the SHELL makes must not be every test's problem. Left to the
  // prototype it would dispatch into the test's own executeNamed, and a fake
  // answering every call with the same rows would decide the gate from a
  // fixture about something else entirely.
  //
  // The default is ELIGIBLE, which is what puts a test straight into the
  // console it is actually about. A test ABOUT the gate provides its own --
  // and the gate's own behaviour is tested directly against gateStep, which
  // needs no fake at all.
  if (typeof s.inferenceStatus !== "function") {
    s.inferenceStatus = async () => ({
      rows: () => [
        {
          eligible: true,
          doorsOpen: ["local"],
          localEligible: true,
          localModelCount: 1,
          eligibleModelIds: ["llama3.1:8b"],
          cloudConfigured: false,
          federationConfigured: false,
          fleetInferenceInstalled: true,
          minimumContextWindow: 8192,
        },
      ],
      rawNodes: () => [],
      single: () => null,
      meta: () => null,
    });
  }
  // fleetModels is read by the Providers page (epic memql#4676). Defaulted to
  // an EMPTY catalog -- a cluster with no machines paired, which is what every
  // test that is not about local models is describing.
  if (typeof s.fleetModels !== "function") {
    s.fleetModels = async () => ({
      rows: () => [],
      rawNodes: () => [],
      single: () => null,
      meta: () => null,
    });
  }
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
