import { render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ContextMenu } from "../../src/chrome/ContextMenu";
import { DialogFrame } from "../../src/kit/DialogFrame";

afterEach(() => vi.restoreAllMocks());

// The desktop must never scroll sideways, and a focus call is one of the
// things that scrolls it. Opening a dialog or a menu puts focus somewhere
// without moving the page.
describe("opening focus does not scroll", () => {
  it("passes preventScroll on the dialog's and the context menu's opening focus", () => {
    const focus = vi.spyOn(HTMLElement.prototype, "focus");

    render(
      <DialogFrame title="Edit" onDismiss={() => {}} initialFocus={(dialog) => dialog.querySelector("button")}>
        <button>Name</button>
      </DialogFrame>,
    );
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });

    focus.mockClear();
    render(<ContextMenu x={10} y={10} label="File" entries={[{ id: "open", label: "Open", onSelect: () => {} }]} onClose={() => {}} />);
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });
});
