import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { QueryClient, Result, type QueryCallOptions } from "@znasllc-io/memql-sdk-core/client";

// Settings > Levels asks for `routerLevelBindings` by name, and while no
// cluster has it the section says the fleet picks the model
// (routingFacts.ts, useLevelBindings). Once the generator emits it, the method
// lands on QueryClient.prototype and calls `this.executeNamed` like every
// other, so the probe has to call it ON ITS CLIENT. Called bare, it throws
// inside the effect and the window's boundary takes Settings down, the way
// Fleet > Routing > History went down when routerDecisionsRecent was called
// bare.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

const { useLevelBindings } = await import("../../src/apps/settings/routingFacts");

afterEach(() => {
  h.connection = null;
});

describe("the level bindings read", () => {
  it("calls the method on its client, which is what a generated method needs", async () => {
    const executeNamed = vi.fn(
      async (_name: string, _call: string, _opts?: QueryCallOptions) =>
        new Result({ data: [{ level: "strong", model: "qwen3-coder", machine: "studio-mac" }] } as never),
    );
    h.connection = {
      query: Object.assign(Object.create(QueryClient.prototype), {
        executeNamed,
        // The shape scripts/sdk-gen gives every method (generated_queries.ts).
        routerLevelBindings(this: QueryClient, _args: Record<string, unknown> = {}, opts?: QueryCallOptions) {
          return this.executeNamed("routerLevelBindings", "query routerLevelBindings()", opts);
        },
      }),
    };
    const { result } = renderHook(() => useLevelBindings(true));
    await act(async () => {});
    expect(result.current.available).toBe(true);
    expect(result.current.bound.strong).toMatchObject({ model: "qwen3-coder", where: "studio-mac" });
    expect(executeNamed.mock.calls.map(([name, call]) => [name, call])).toEqual([
      ["routerLevelBindings", "query routerLevelBindings()"],
    ]);
  });
});
