import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SearchPicker, type SearchChoice } from "../../src/kit/SearchPicker";

// The kit's `SearchPicker`: a field that opens a list joined to it, for
// choosing one thing out of many by its name. Type to narrow, then choose.
//
// Its keyboard is what this suite is for, because none of it comes free and
// all of it can be lost to a tidy-up without anything else noticing:
//
//   - the ARROWS move the option reached (`aria-activedescendant`), and stop
//     at the ends rather than wrapping or running off them;
//   - ENTER chooses the option reached, and nothing when none is;
//   - ESCAPE on an open list closes the list and STOPS THERE. A picker sits
//     in dialogs and windows that close on Escape themselves (kit/DialogFrame,
//     the Ask sheet): an Escape that reached them would take the draft with
//     the list. On a closed list it is theirs;
//   - TAB moves on, closing the list as nothing chosen;
//   - while an input method is COMPOSING, every key is the composition's:
//     its Enter commits the characters and its Escape abandons them, and
//     neither may choose or close anything.

const CHOICES: SearchChoice[] = [
  { id: "u-avery", name: "Avery Owner", detail: "avery@example.com" },
  { id: "u-robin", name: "Robin Moderator", detail: "robin@example.com" },
  { id: "u-sam", name: "Sam Writer", detail: "sam@acme.test" },
];

const offShell: Array<() => void> = [];

afterEach(() => {
  while (offShell.length > 0) offShell.pop()?.();
  cleanup();
});

/**
 * The picker inside a parent that handles keys itself, as a dialog does, with
 * a shell listener on the window above both, as the Ask sheet has.
 */
function mount() {
  const onChoose = vi.fn();
  const onDismiss = vi.fn();
  const parentKeys: string[] = [];
  const shellKeys: string[] = [];
  const shell = (event: KeyboardEvent) => void shellKeys.push(event.key);
  window.addEventListener("keydown", shell);
  offShell.push(() => window.removeEventListener("keydown", shell));
  render(
    <div onKeyDown={(event) => void parentKeys.push(event.key)}>
      <SearchPicker
        id="people"
        label="Choose a person"
        listLabel="People"
        choices={CHOICES}
        emptyText="Nobody by that name."
        onChoose={onChoose}
        onDismiss={onDismiss}
      />
    </div>,
  );
  const field = screen.getByRole("combobox", { name: "Choose a person" });
  return { field, onChoose, onDismiss, parentKeys, shellKeys };
}

const list = () => screen.queryByRole("listbox", { name: "People" });
const shown = () => within(screen.getByRole("listbox", { name: "People" })).queryAllByRole("option").map((o) => o.getAttribute("aria-label"));
/** The option the arrows reached, by its name; null when none is. */
const reached = (field: HTMLElement) => {
  const id = field.getAttribute("aria-activedescendant");
  return id === null ? null : (document.getElementById(id)?.getAttribute("aria-label") ?? `missing ${id}`);
};
const key = (field: HTMLElement, name: string, init: KeyboardEventInit = {}) => fireEvent.keyDown(field, { key: name, ...init });

describe("the arrows", () => {
  it("move the option reached, and stop at the ends", () => {
    const { field } = mount();
    expect(field.getAttribute("aria-expanded")).toBe("false");

    // Down on a closed list opens it on the first option.
    key(field, "ArrowDown");
    expect(field.getAttribute("aria-expanded")).toBe("true");
    expect(reached(field)).toBe("Avery Owner");
    expect(within(list()!).getByRole("option", { name: "Avery Owner" }).getAttribute("aria-selected")).toBe("true");

    key(field, "ArrowDown");
    expect(reached(field)).toBe("Robin Moderator");
    key(field, "ArrowDown");
    expect(reached(field)).toBe("Sam Writer");
    // The last is as far as down goes.
    key(field, "ArrowDown");
    expect(reached(field)).toBe("Sam Writer");

    key(field, "ArrowUp");
    expect(reached(field)).toBe("Robin Moderator");
    key(field, "ArrowUp");
    expect(reached(field)).toBe("Avery Owner");
    // The first is as far as up goes.
    key(field, "ArrowUp");
    expect(reached(field)).toBe("Avery Owner");
    expect(within(list()!).getAllByRole("option").filter((o) => o.getAttribute("aria-selected") === "true")).toHaveLength(1);
  });
});

describe("Enter", () => {
  it("chooses the option reached, and closes the list", () => {
    const { field, onChoose, onDismiss } = mount();
    key(field, "ArrowDown");
    key(field, "ArrowDown");
    key(field, "Enter");
    expect(onChoose).toHaveBeenCalledTimes(1);
    expect(onChoose).toHaveBeenCalledWith(CHOICES[1]);
    expect(list()).toBeNull();
    expect(field.getAttribute("aria-expanded")).toBe("false");
    // A choice is not a dismissal.
    expect(onDismiss).not.toHaveBeenCalled();
  });

  it("does nothing when no option is reached", () => {
    const { field, onChoose, onDismiss } = mount();
    // Focus opens the list with nothing reached yet.
    act(() => field.focus());
    expect(list()).not.toBeNull();
    expect(reached(field)).toBeNull();
    // Left alone, not even its default taken away.
    expect(key(field, "Enter")).toBe(true);
    expect(onChoose).not.toHaveBeenCalled();
    expect(onDismiss).not.toHaveBeenCalled();
    expect(list()).not.toBeNull();

    // Nor when what was typed matches nothing.
    fireEvent.change(field, { target: { value: "nobody here" } });
    expect(reached(field)).toBeNull();
    key(field, "Enter");
    expect(onChoose).not.toHaveBeenCalled();
    expect(list()).not.toBeNull();
  });
});

describe("Escape", () => {
  it("closes an open list, and nothing above the picker hears it", () => {
    const { field, onDismiss, parentKeys, shellKeys } = mount();
    key(field, "ArrowDown");
    expect(list()).not.toBeNull();
    key(field, "Escape");
    expect(list()).toBeNull();
    expect(onDismiss).toHaveBeenCalledTimes(1);
    // The list's Escape: the dialog it sits in, and the window, never see it.
    expect(parentKeys).not.toContain("Escape");
    expect(shellKeys).not.toContain("Escape");
  });

  it("on a closed list is the parent's", () => {
    const { field, onDismiss, parentKeys, shellKeys } = mount();
    key(field, "Escape");
    expect(parentKeys).toEqual(["Escape"]);
    expect(shellKeys).toEqual(["Escape"]);
    expect(onDismiss).not.toHaveBeenCalled();

    // Open, the first Escape is the list's; the next is the parent's again.
    key(field, "ArrowDown");
    key(field, "Escape");
    key(field, "Escape");
    expect(parentKeys.filter((k) => k === "Escape")).toHaveLength(2);
    expect(shellKeys.filter((k) => k === "Escape")).toHaveLength(2);
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });
});

describe("Tab", () => {
  it("moves on, closing the list as nothing chosen", () => {
    const { field, onChoose, onDismiss } = mount();
    key(field, "ArrowDown");
    // The browser still moves focus on: the picker takes nothing from Tab.
    expect(key(field, "Tab")).toBe(true);
    expect(list()).toBeNull();
    expect(onDismiss).toHaveBeenCalledTimes(1);
    // The option the arrows reached is not chosen by leaving.
    expect(onChoose).not.toHaveBeenCalled();
  });
});

describe("while an input method is composing", () => {
  it("leaves every key to the composition", () => {
    const { field, onChoose, onDismiss } = mount();
    // Closed, the composition's Down does not open the list.
    key(field, "ArrowDown", { isComposing: true });
    expect(list()).toBeNull();

    key(field, "ArrowDown");
    expect(reached(field)).toBe("Avery Owner");
    key(field, "ArrowDown", { isComposing: true });
    key(field, "ArrowUp", { isComposing: true });
    expect(reached(field)).toBe("Avery Owner");
    // Its Enter commits the characters, and chooses nothing.
    key(field, "Enter", { isComposing: true });
    expect(onChoose).not.toHaveBeenCalled();
    // Its Escape abandons them, and closes nothing.
    key(field, "Escape", { isComposing: true });
    expect(list()).not.toBeNull();
    key(field, "Tab", { isComposing: true });
    expect(list()).not.toBeNull();
    expect(onDismiss).not.toHaveBeenCalled();
  });
});

describe("typing", () => {
  it("narrows the list by name and by detail, and says so when nothing matches", () => {
    const { field, onChoose } = mount();
    // By name, whatever its case.
    fireEvent.change(field, { target: { value: "ROBIN" } });
    expect(shown()).toEqual(["Robin Moderator"]);
    // By the quieter line under the name.
    fireEvent.change(field, { target: { value: "acme.test" } });
    expect(shown()).toEqual(["Sam Writer"]);
    expect(screen.queryByText("Nobody by that name.")).toBeNull();

    fireEvent.change(field, { target: { value: "nobody here" } });
    expect(shown()).toEqual([]);
    expect(screen.getByText("Nobody by that name.")).toBeTruthy();

    fireEvent.change(field, { target: { value: "" } });
    expect(shown()).toEqual(["Avery Owner", "Robin Moderator", "Sam Writer"]);
    expect(screen.queryByText("Nobody by that name.")).toBeNull();

    // What was typed reaches its first match, so Enter takes it.
    fireEvent.change(field, { target: { value: "example.com" } });
    expect(shown()).toEqual(["Avery Owner", "Robin Moderator"]);
    expect(reached(field)).toBe("Avery Owner");
    key(field, "Enter");
    expect(onChoose).toHaveBeenCalledWith(CHOICES[0]);
    // And the field is left empty for the next time.
    expect((field as HTMLInputElement).value).toBe("");
  });
});
