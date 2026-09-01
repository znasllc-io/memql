import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { BIN_APP_ID } from "../../src/apps/bin/concepts";
import { dockOrder, emptyDock, pin } from "../../src/system/dock";
import { appById, fixturesForRole, isDockFixture } from "../../src/system/registry";

// THE BIN IS ALWAYS IN THE DOCK AND CANNOT BE TAKEN OUT OF IT (memql#4784).
//
// This is the AC the whole app hangs from: a trash can somebody can unpin is
// one they can lose, and archiving then becomes a thing with no visible
// destination. It is a property of the SHELL rather than of a stored
// preference, which is why none of these assertions goes near DesktopStore.

describe("the Bin as a dock fixture", () => {
  it("is declared as one, and is the only one", () => {
    expect(isDockFixture(OS_REGISTRY, BIN_APP_ID)).toBe(true);
    expect(fixturesForRole(OS_REGISTRY, "owner").map((a) => a.id)).toEqual([BIN_APP_ID]);
  });

  it("carries no role, so every signed-in person has one", () => {
    // v1:library:artifact declares the composite tier; the engine decides how
    // far the read goes. Gating here would be presentation pretending to be
    // authorization -- and a reader with no Bin has nowhere for their
    // archives to be.
    expect(appById(OS_REGISTRY, BIN_APP_ID)?.roles).toBeUndefined();
    for (const role of ["reader", "writer", "admin", "owner"]) {
      expect(fixturesForRole(OS_REGISTRY, role).map((a) => a.id)).toEqual([BIN_APP_ID]);
    }
  });

  it("never appears in the pin strip, even when it is RUNNING", () => {
    // Without this it is drawn twice the moment somebody opens it: once as a
    // running app in the strip, once in its own slot.
    const order = dockOrder(emptyDock(), [BIN_APP_ID, "files"], [BIN_APP_ID]);
    expect(order).toEqual(["files"]);
  });

  it("never appears in the pin strip even when a STORED document pins it", () => {
    // A desktop written before the app was a fixture can still name it, and a
    // fixture that is also a pin can be dragged out of the strip and lost.
    const dock = pin(emptyDock(), BIN_APP_ID);
    expect(dockOrder(dock, [], [BIN_APP_ID])).toEqual([]);
  });

  it("leaves every other app pinnable and reorderable", () => {
    // The reachable positive: if `fixtures` filtered too widely, the
    // assertions above would pass on a dock that had stopped working.
    const dock = pin(pin(emptyDock(), "files"), "fleet");
    expect(dockOrder(dock, ["users"], [BIN_APP_ID])).toEqual(["files", "fleet", "users"]);
    expect(isDockFixture(OS_REGISTRY, "files")).toBe(false);
  });
});
