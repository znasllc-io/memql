// `vitest`'s surface, as the browser harness needs it.
//
// The fixture connection lives in `test/deployables/harness.tsx` and imports
// `vi`. Shimming it is deliberate: duplicating the fake would let the harness
// drift from what the suite asserts, which is the one thing a screenshot must
// not do.
type AnyFn = (...args: unknown[]) => unknown;

interface MockFn extends AnyFn {
  mock: { calls: unknown[][] };
  mockReturnValue: (v: unknown) => MockFn;
}

function fn(impl?: AnyFn): MockFn {
  const calls: unknown[][] = [];
  let returns: unknown;
  let hasReturn = false;
  const f = ((...args: unknown[]) => {
    calls.push(args);
    if (hasReturn) return returns;
    return impl ? impl(...args) : undefined;
  }) as MockFn;
  f.mock = { calls };
  f.mockReturnValue = (v: unknown) => {
    returns = v;
    hasReturn = true;
    return f;
  };
  return f;
}

export const vi = { fn, mock: () => {}, hoisted: <T,>(f: () => T) => f() };
export default { vi };
