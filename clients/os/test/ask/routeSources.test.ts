import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { routeOptions, settleMachines, type RouteFacts } from "../../src/ask/routeSources";

// What the picker says when it cannot ask: a disconnected read is not a read
// in progress (DESIGN.md, "Loading is the shape of the content"), so no row
// keeps a skeleton for an answer that is not coming.

const NOT_ASKED: RouteFacts = {
  machines: { state: "offline", value: [] },
  doors: { state: "offline", value: null },
};

describe("the machines feed, settled for the picker", () => {
  it("reads a feed with no connection, or a dropped one, as not connected", () => {
    expect(settleMachines("absent", false, []).state).toBe("offline");
    expect(settleMachines("disconnected", false, []).state).toBe("offline");
  });

  it("reads a feed still seeding as pending, and a live one as read", () => {
    expect(settleMachines("seeding", false, []).state).toBe("pending");
    const row = { id: "v1:worker:registration:studio", displayName: "Studio" } as unknown as Row;
    const live = settleMachines("live", true, [row]);
    expect(live.state).toBe("read");
    expect(live.value).toHaveLength(1);
  });
});

describe("rows with no connection", () => {
  it("say Not connected and cannot be chosen; Auto needs nothing", () => {
    const options = routeOptions(NOT_ASKED, "");
    expect(options.find((o) => o.where === "auto")?.state).toBe("ready");
    for (const option of options.filter((o) => o.where !== "auto")) {
      expect(option).toMatchObject({ state: "unready", note: "Not connected" });
    }
  });
});
