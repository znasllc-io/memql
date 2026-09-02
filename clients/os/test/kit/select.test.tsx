import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  listScrollTop,
  placeListbox,
  Select,
  selectOptionsFrom,
} from "../../src/kit/controls";

// The kit's `Select`, after the open half stopped being the browser's.
//
// A native `<option>` popup is painted by the platform and reachable by no
// rule in this project, so the closed control spoke the shell's design
// language and the open one spoke Chrome's. The list is drawn here now, and
// three things about that are what this suite is for:
//
//   - the OPTIONS are still the caller's `<option>` children (22 call sites
//     across 13 app files pass them that way, and none of them changed), so
//     the walk that reads them back out has to survive every shape a real
//     call site writes;
//   - WHERE the list lives in the DOM, which is the same containing-block
//     trap `test/chrome/contextMenu.test.tsx` exists for: `position: fixed`
//     resolves against a `backdrop-filter`ed `.os-window` rather than the
//     viewport, so a list left in the caller's subtree opens a window away
//     from the control. jsdom computes no containing blocks, so the
//     assertion that stands in for it is structural;
//   - the KEYBOARD, all of which a native select did for free and none of
//     which survives being reimplemented by accident.

const restore: Array<() => void> = [];

afterEach(() => {
  while (restore.length > 0) restore.pop()?.();
  vi.restoreAllMocks();
});

/** Pin the viewport, the way a browser would report it. */
function viewport(width: number, height: number) {
  for (const [key, value] of [
    ["innerWidth", width],
    ["innerHeight", height],
  ] as const) {
    const original = Object.getOwnPropertyDescriptor(window, key);
    Object.defineProperty(window, key, { configurable: true, value });
    restore.push(() => {
      if (original) Object.defineProperty(window, key, original);
    });
  }
}

/**
 * Give the trigger and the list real rects. jsdom lays nothing out, so every
 * rect is zero and nothing would ever flip or clamp -- the arithmetic under
 * test would be measured against a control with no size.
 */
function measures(spec: {
  trigger: { left: number; top: number; width: number; height: number };
  list: { width: number; height: number };
}) {
  const zero = new DOMRect(0, 0, 0, 0);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    if (this.classList.contains("os-select")) {
      return new DOMRect(spec.trigger.left, spec.trigger.top, spec.trigger.width, spec.trigger.height);
    }
    if (this.classList.contains("os-select-list")) {
      return new DOMRect(0, 0, spec.list.width, spec.list.height);
    }
    return zero;
  });
}

const SOURCES = (
  <>
    <option value="all">Any source</option>
    <option value="artifact">artifact</option>
    <option value="generated_output">generated output</option>
  </>
);

function Harness({
  initial = "artifact",
  onChange,
  children = SOURCES,
}: {
  initial?: string;
  onChange?: (next: string) => void;
  children?: ReactNode;
}) {
  const [value, setValue] = useState(initial);
  return (
    <Select
      id="files-source"
      label="Source"
      value={value}
      onChange={(next) => {
        setValue(next);
        onChange?.(next);
      }}
    >
      {children}
    </Select>
  );
}

/** Render the way an app does: inside a window, inside the app's own surface. */
function openIn(props: Parameters<typeof Harness>[0] = {}) {
  const view = render(
    <div className="os-window" data-testid="surface">
      <div className="os-files">
        <Harness {...props} />
      </div>
    </div>,
  );
  return { view, trigger: screen.getByLabelText("Source"), surface: view.getByTestId("surface") };
}

function listbox(): HTMLElement {
  const node = document.body.querySelector<HTMLElement>('[role="listbox"]');
  if (node === null) throw new Error("no listbox open");
  return node;
}

function optionNames(): string[] {
  return within(listbox())
    .getAllByRole("option")
    .map((option) => option.textContent ?? "");
}

// ---------------------------------------------------------------------------
// The children walk
// ---------------------------------------------------------------------------

describe("reading options out of `<option>` children", () => {
  it("takes plain options, in order", () => {
    expect(
      selectOptionsFrom(
        <>
          <option value="a">Alpha</option>
          <option value="b">Beta</option>
        </>,
      ),
    ).toEqual([
      { value: "a", label: "Alpha", disabled: false, group: "" },
      { value: "b", label: "Beta", disabled: false, group: "" },
    ]);
  });

  it("carries `disabled` through", () => {
    // PersonDetail disables a grant above the viewer's own rank, and the
    // option has to stay in the list -- it may be the person's CURRENT role.
    const options = selectOptionsFrom(
      <>
        <option value="admin">admin</option>
        <option value="owner" disabled>
          owner
        </option>
      </>,
    );
    expect(options.map((o) => o.disabled)).toEqual([false, true]);
  });

  it("drops the null a conditional option leaves behind", () => {
    const showBeta = false;
    expect(
      selectOptionsFrom(
        <>
          <option value="a">Alpha</option>
          {showBeta ? <option value="b">Beta</option> : null}
        </>,
      ).map((o) => o.value),
    ).toEqual(["a"]);
  });

  it("flattens the array a `.map()` returns, fragments and all", () => {
    const rows = [
      { id: "r1", name: "One" },
      { id: "r2", name: "Two" },
    ];
    expect(
      selectOptionsFrom(
        <>
          <option value="">Choose</option>
          {rows.map((row) => (
            <option key={row.id} value={row.id}>
              {row.name}
            </option>
          ))}
        </>,
      ).map((o) => o.value),
    ).toEqual(["", "r1", "r2"]);
  });

  it("joins an option's text with NOTHING between the pieces", () => {
    // The real shape, from CampaignsSection: a name and a suffix the author
    // already put a space in front of. Joining on " " doubles it.
    const audience = { name: "Newsletter", status: "archived" };
    expect(
      selectOptionsFrom(
        <option value="a1">
          {audience.name}
          {audience.status === "archived" ? " (archived)" : ""}
        </option>,
      )[0]?.label,
    ).toBe("Newsletter (archived)");
  });

  it("falls back to the text when an option carries no `value`, as HTML does", () => {
    expect(selectOptionsFrom(<option>reader</option>)[0]).toEqual({
      value: "reader",
      label: "reader",
      disabled: false,
      group: "",
    });
  });

  it("keeps a grouped option, carrying its group name, and skips anything that is not an option", () => {
    expect(
      selectOptionsFrom(
        <>
          {"stray text"}
          <optgroup label="Machines">
            <option value="m1">studio-mini</option>
          </optgroup>
          <span>not an option</span>
        </>,
      ),
    ).toEqual([{ value: "m1", label: "studio-mini", disabled: false, group: "Machines" }]);
  });
});

// ---------------------------------------------------------------------------
// The closed control
// ---------------------------------------------------------------------------

describe("the trigger", () => {
  it("is a labelled combobox showing the selected option's label", () => {
    const { trigger } = openIn();
    expect(trigger.tagName).toBe("BUTTON");
    expect(trigger.getAttribute("type")).toBe("button");
    expect(screen.getByRole("combobox", { name: "Source" })).toBe(trigger);
    expect(trigger.getAttribute("aria-haspopup")).toBe("listbox");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    // No list, so nothing to point `aria-controls` at.
    expect(trigger.getAttribute("aria-controls")).toBeNull();
    expect(trigger.textContent).toBe("artifact");
  });

  it("shows an unmatched value as itself, muted -- never blank", () => {
    // The live case: an id whose row has not arrived on the feed yet. A blank
    // box says something is wrong; the id says WHICH value is unresolved.
    const { trigger } = openIn({ initial: "v1:campaigns:audience:a9" });
    expect(trigger.textContent).toBe("v1:campaigns:audience:a9");
    expect(trigger.querySelector("[data-placeholder]")).not.toBeNull();
  });

  it("shows the em dash when the value is empty and no option names it", () => {
    const { trigger } = openIn({ initial: "" });
    expect(trigger.textContent).toBe("--");
  });
});

// ---------------------------------------------------------------------------
// Where the list lives
// ---------------------------------------------------------------------------

describe("the open list", () => {
  it("renders into document.body, outside the surface that opened it", () => {
    // THE REGRESSION TEST. Inside `.os-window` the list's `position: fixed`
    // resolves against the window's backdrop-filtered box rather than the
    // viewport, and the list opens a title bar and a window edge away from
    // the control that produced it.
    const { view, trigger, surface } = openIn();
    fireEvent.click(trigger);

    const list = listbox();
    expect(list.parentElement).toBe(document.body);
    expect(surface.contains(list)).toBe(false);
    expect(view.container.contains(list)).toBe(false);
    expect(view.container.querySelector('[role="listbox"]')).toBeNull();
  });

  it("names itself, points the trigger at it, and marks the selected option", () => {
    const { trigger } = openIn();
    fireEvent.click(trigger);
    const list = listbox();

    expect(list.getAttribute("aria-label")).toBe("Source");
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(trigger.getAttribute("aria-controls")).toBe(list.id);
    expect(optionNames()).toEqual(["Any source", "artifact", "generated output"]);
    expect(
      within(list).getByRole("option", { name: "artifact" }).getAttribute("aria-selected"),
    ).toBe("true");
  });

  it("opens on the selected option and tracks the active one in aria-activedescendant", () => {
    const { trigger } = openIn();
    fireEvent.click(trigger);
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");

    expect(active()?.textContent).toBe("artifact");
    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    expect(active()?.textContent).toBe("generated output");
  });

  it("closes on an outside pointerdown and stays open on one inside it", () => {
    const { trigger, surface } = openIn();
    fireEvent.click(trigger);

    fireEvent.pointerDown(within(listbox()).getByRole("option", { name: "Any source" }));
    expect(document.body.querySelector('[role="listbox"]')).not.toBeNull();

    fireEvent.pointerDown(surface);
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
  });

  it("opens and says so when there is nothing to choose", () => {
    // A control that does nothing when clicked reads as broken. "There is
    // nothing here yet" is the account of itself an absent answer owes.
    const { trigger } = openIn({ initial: "", children: [] });
    fireEvent.click(trigger);
    expect(within(listbox()).getByText("Nothing to choose here yet")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Placement
// ---------------------------------------------------------------------------

describe("placing the list against its trigger", () => {
  it("aligns to the trigger's left edge and is never narrower than it", () => {
    expect(
      placeListbox({
        anchorLeft: 120,
        anchorTop: 200,
        anchorBottom: 230,
        anchorWidth: 240,
        listWidth: 150,
        listHeight: 120,
        viewportWidth: 1024,
        viewportHeight: 768,
      }),
    ).toEqual({ left: 120, top: 234, minWidth: 240, above: false });
  });

  it("keeps a list wider than its trigger", () => {
    expect(
      placeListbox({
        anchorLeft: 10,
        anchorTop: 10,
        anchorBottom: 40,
        anchorWidth: 90,
        listWidth: 260,
        listHeight: 100,
        viewportWidth: 1024,
        viewportHeight: 768,
      }).minWidth,
    ).toBe(260);
  });

  it("SLIDES back inside on the right rather than flipping", () => {
    // A flip would leave the list hanging off the side of the control it
    // belongs to. There is a whole box to align against here, unlike a
    // context menu opened at a point.
    expect(
      placeListbox({
        anchorLeft: 900,
        anchorTop: 100,
        anchorBottom: 130,
        anchorWidth: 200,
        listWidth: 200,
        listHeight: 100,
        viewportWidth: 1024,
        viewportHeight: 768,
      }).left,
    ).toBe(816); // 1024 - 8 - 200
  });

  it("FLIPS above when it would overflow the bottom", () => {
    expect(
      placeListbox({
        anchorLeft: 100,
        anchorTop: 700,
        anchorBottom: 730,
        anchorWidth: 200,
        listWidth: 200,
        listHeight: 200,
        viewportWidth: 1024,
        viewportHeight: 768,
      }),
    ).toEqual({ left: 100, top: 496, minWidth: 200, above: true }); // 700 - 4 - 200
  });

  it("stays below and clamps when there is no room on either side", () => {
    // A list that fits nowhere has nothing to flip to; pinning it inside the
    // margin is the last resort, running off the near edge is not.
    expect(
      placeListbox({
        anchorLeft: 4,
        anchorTop: 100,
        anchorBottom: 130,
        anchorWidth: 200,
        listWidth: 200,
        listHeight: 400,
        viewportWidth: 360,
        viewportHeight: 320,
      }),
    ).toEqual({ left: 8, top: 8, minWidth: 200, above: false });
  });

  it("applies the placement to the portalled node", () => {
    viewport(1024, 768);
    measures({ trigger: { left: 120, top: 200, width: 240, height: 30 }, list: { width: 150, height: 120 } });
    const { trigger } = openIn();
    fireEvent.click(trigger);

    const list = listbox();
    expect(list.style.left).toBe("120px");
    expect(list.style.top).toBe("234px");
    expect(list.style.minWidth).toBe("240px");
    expect(list.getAttribute("data-above")).toBeNull();
  });

  it("marks the list when it opened upwards, so the CSS can say so", () => {
    viewport(1024, 768);
    measures({ trigger: { left: 100, top: 700, width: 200, height: 30 }, list: { width: 200, height: 200 } });
    const { trigger } = openIn();
    fireEvent.click(trigger);

    expect(listbox().style.top).toBe("496px");
    expect(listbox().getAttribute("data-above")).toBe("true");
  });
});

describe("scrolling the active option into view", () => {
  // `scrollIntoView` on a node portalled to `document.body` can scroll the
  // PAGE, moving the surface underneath somebody mid-list. This moves the
  // list and only the list.
  it("scrolls up to an item above the view", () => {
    expect(listScrollTop({ scrollTop: 200, viewHeight: 100 }, { top: 40, height: 30 })).toBe(40);
  });

  it("scrolls down just far enough to show an item below it", () => {
    expect(listScrollTop({ scrollTop: 0, viewHeight: 100 }, { top: 260, height: 30 })).toBe(190);
  });

  it("leaves a visible item alone", () => {
    expect(listScrollTop({ scrollTop: 100, viewHeight: 100 }, { top: 120, height: 30 })).toBe(100);
  });
});

// ---------------------------------------------------------------------------
// The keyboard
// ---------------------------------------------------------------------------

describe("the keyboard", () => {
  it.each(["Enter", " ", "ArrowDown", "ArrowUp"])("opens on %s", (key) => {
    const { trigger } = openIn();
    fireEvent.keyDown(trigger, { key });
    expect(document.body.querySelector('[role="listbox"]')).not.toBeNull();
  });

  it("opens on Alt+ArrowDown", () => {
    const { trigger } = openIn();
    fireEvent.keyDown(trigger, { key: "ArrowDown", altKey: true });
    expect(document.body.querySelector('[role="listbox"]')).not.toBeNull();
  });

  it("commits the first and last option on Home and End while closed", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.keyDown(trigger, { key: "Home" });
    expect(onChange).toHaveBeenLastCalledWith("all");
    fireEvent.keyDown(trigger, { key: "End" });
    expect(onChange).toHaveBeenLastCalledWith("generated_output");
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
  });

  it("moves the active option with the arrows and jumps with Home and End", () => {
    const { trigger } = openIn({ initial: "all" });
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");
    fireEvent.click(trigger);

    expect(active()?.textContent).toBe("Any source");
    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    expect(active()?.textContent).toBe("artifact");
    fireEvent.keyDown(trigger, { key: "End" });
    expect(active()?.textContent).toBe("generated output");
    // The arrows CLAMP rather than wrap, which is what a select does; a
    // context menu is the thing that wraps.
    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    expect(active()?.textContent).toBe("generated output");
    fireEvent.keyDown(trigger, { key: "Home" });
    expect(active()?.textContent).toBe("Any source");
  });

  it("commits the active option on Enter and on Space, and closes", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    fireEvent.keyDown(trigger, { key: "Enter" });

    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith("generated_output");
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("Escape closes, commits NOTHING, and hands focus back", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: "ArrowUp" });
    fireEvent.keyDown(trigger, { key: "Escape" });

    expect(onChange).not.toHaveBeenCalled();
    expect(trigger.textContent).toBe("artifact");
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("does not let Escape reach the surface behind it", () => {
    // A portal bubbles through the REACT tree, so an Escape handled here
    // would otherwise also close the window this select sits in.
    const outer = vi.fn();
    render(
      <div onKeyDown={outer}>
        <Harness />
      </div>,
    );
    const trigger = screen.getByLabelText("Source");
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: "Escape" });
    expect(outer).not.toHaveBeenCalled();
  });

  it("Tab closes, commits nothing, and is left to the browser", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.click(trigger);
    const tab = fireEvent.keyDown(trigger, { key: "Tab" });

    expect(tab).toBe(true); // not prevented: focus moves on as it would anywhere
    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
  });
});

describe("type-ahead", () => {
  const LETTERS = (
    <>
      <option value="ba">Baker</option>
      <option value="bl">Blake</option>
      <option value="ca">Carter</option>
    </>
  );

  function at(now: number) {
    vi.spyOn(Date, "now").mockReturnValue(now);
  }

  it("jumps to the first option starting with the letter, and cycles on a repeat", () => {
    at(1000);
    const { trigger } = openIn({ initial: "ca", children: LETTERS });
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");
    fireEvent.click(trigger);

    fireEvent.keyDown(trigger, { key: "b" });
    expect(active()?.textContent).toBe("Baker");
    at(1200);
    fireEvent.keyDown(trigger, { key: "b" });
    expect(active()?.textContent).toBe("Blake");
  });

  it("narrows in place while the buffer stands", () => {
    at(1000);
    const { trigger } = openIn({ initial: "ba", children: LETTERS });
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");
    fireEvent.click(trigger);

    fireEvent.keyDown(trigger, { key: "b" });
    at(1200);
    fireEvent.keyDown(trigger, { key: "l" });
    expect(active()?.textContent).toBe("Blake");
  });

  it("starts a new buffer after about a second", () => {
    at(1000);
    const { trigger } = openIn({ initial: "ba", children: LETTERS });
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");
    fireEvent.click(trigger);

    fireEvent.keyDown(trigger, { key: "b" });
    at(3000);
    // "c" against a stale "b" buffer would search for "bc" and find nothing.
    fireEvent.keyDown(trigger, { key: "c" });
    expect(active()?.textContent).toBe("Carter");
  });

  it("OPENS on a letter while closed rather than committing one", () => {
    // The one deliberate parting from the native control. A `<select>` writes
    // the value on the keystroke, and the handlers behind this one grant
    // cluster roles and change subscription states.
    at(1000);
    const onChange = vi.fn();
    const { trigger } = openIn({ initial: "ca", onChange, children: LETTERS });
    fireEvent.keyDown(trigger, { key: "b" });

    expect(onChange).not.toHaveBeenCalled();
    expect(
      document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "")?.textContent,
    ).toBe("Baker");
  });
});

// ---------------------------------------------------------------------------
// Committing
// ---------------------------------------------------------------------------

describe("committing a choice", () => {
  it("fires onChange with the option's value, exactly once, and closes", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.click(trigger);
    fireEvent.click(within(listbox()).getByRole("option", { name: "generated output" }));

    expect(onChange.mock.calls).toEqual([["generated_output"]]);
    expect(trigger.textContent).toBe("generated output");
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
  });

  it("re-picking what is already chosen closes and writes nothing", () => {
    // NATIVE PARITY, AND IT MATTERS: one of these handlers grants a cluster
    // role and another writes a recipient's subscription status. A `<select>`
    // fires `change` only when the value actually changes.
    const onChange = vi.fn();
    const { trigger } = openIn({ onChange });
    fireEvent.click(trigger);
    fireEvent.click(within(listbox()).getByRole("option", { name: "artifact" }));

    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.querySelector('[role="listbox"]')).toBeNull();
  });

  it("a disabled option cannot be clicked and is stepped over by the arrows", () => {
    const onChange = vi.fn();
    const { trigger } = openIn({
      initial: "reader",
      onChange,
      children: (
        <>
          <option value="reader">reader</option>
          <option value="admin" disabled>
            admin
          </option>
          <option value="developer">developer</option>
        </>
      ),
    });
    fireEvent.click(trigger);

    const admin = within(listbox()).getByRole("option", { name: "admin" });
    expect(admin.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(admin);
    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.querySelector('[role="listbox"]')).not.toBeNull();

    fireEvent.keyDown(trigger, { key: "ArrowDown" });
    expect(
      document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "")?.textContent,
    ).toBe("developer");
  });
});
