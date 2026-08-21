import { QueryClient } from "@znasllc-io/memql-sdk-core/client";

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
  return Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient & T;
}
