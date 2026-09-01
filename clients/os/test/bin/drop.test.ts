import { describe, expect, it } from "vitest";

import { BIN_DROPPABLE_ID, type BinDropPayload } from "../../src/apps/bin/concepts";
import { decideBinDrop } from "../../src/apps/bin/drop";

// What a drop on the Bin means. The two REFUSALS are the reason this is a
// function: from the outside both look like nothing happening, which is also
// what a broken drop target looks like.

const file: BinDropPayload = { artifactId: "a-1", name: "cut-03.mov", folder: false, deskItemId: "" };
const folder: BinDropPayload = { artifactId: "fo-1", name: "Clients", folder: true, deskItemId: "" };

describe("deciding a drop on the Bin", () => {
  it("archives a file when the confirm is off", () => {
    expect(decideBinDrop(BIN_DROPPABLE_ID, file, false)).toEqual({ kind: "archive", drop: file });
  });

  it("asks first when the confirm is on -- the SAME setting the row action reads", () => {
    expect(decideBinDrop(BIN_DROPPABLE_ID, file, true)).toEqual({ kind: "confirm", drop: file });
  });

  it("REFUSES a folder, whatever the confirm says", () => {
    // Archiving a folder is a recursive walk whose confirm names the live
    // count inside it, and a dock icon cannot count that. A confirm that
    // could not name the number would be asking somebody to approve an
    // amount of destruction it declined to state.
    for (const confirm of [true, false]) {
      expect(decideBinDrop(BIN_DROPPABLE_ID, folder, confirm)).toEqual({
        kind: "refuseFolder",
        name: "Clients",
      });
    }
  });

  it("ignores a drop that landed somewhere else entirely", () => {
    expect(decideBinDrop("folder:fo-2", file, false)).toEqual({ kind: "ignore" });
    expect(decideBinDrop(null, file, false)).toEqual({ kind: "ignore" });
  });

  it("ignores a drag carrying nothing the Bin can act on", () => {
    // A window, a widget, an upload still in flight. IGNORED rather than
    // refused: refusing implies the gesture was aimed here, and it was not.
    expect(decideBinDrop(BIN_DROPPABLE_ID, null, false)).toEqual({ kind: "ignore" });
    expect(decideBinDrop(BIN_DROPPABLE_ID, { ...file, artifactId: "" }, false)).toEqual({ kind: "ignore" });
    expect(decideBinDrop(BIN_DROPPABLE_ID, { ...file, artifactId: "   " }, false)).toEqual({ kind: "ignore" });
  });

  it("carries the desk shortcut through, so the icon goes with the archive", () => {
    const fromDesk = { ...file, deskItemId: "item-7" };
    const outcome = decideBinDrop(BIN_DROPPABLE_ID, fromDesk, false);
    expect(outcome).toEqual({ kind: "archive", drop: fromDesk });
  });
});
