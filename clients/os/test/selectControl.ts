import { fireEvent, within } from "@testing-library/react";

// Driving the kit's `Select` from a test.
//
// The control is not a native `<select>` any more (the browser's `<option>`
// popup is painted by the platform and reachable by no rule in this project,
// so the open half never spoke the shell's design language). It is a trigger
// plus a listbox the shell draws, and the listbox is PORTALLED to
// `document.body` -- which is what stops `position: fixed` resolving against a
// backdrop-filtered `.os-window` and opening the list a window away from the
// control.
//
// Both of those change how a test reaches an option:
//
//   - `fireEvent.change(select, { target: { value } })` has nothing to change.
//     A commit is a click on an option, or Enter on the active one.
//   - the options DO NOT EXIST until the list is open, so a test that looks
//     for an option's text without opening it is asking about a node the
//     browser has not been asked to render.
//
// Hence this helper rather than the same six lines in every suite: one place
// says how the control is driven, and a change to the control changes one
// file. `chooseOption` is the whole interaction a person performs.

/**
 * Open a `Select` and hand back its listbox.
 *
 * The trigger is found by its label exactly as before
 * (`screen.getByLabelText("Source")`) -- the visually-hidden `<label for>` is
 * still what names it.
 */
export function openSelect(trigger: HTMLElement): HTMLElement {
  if (trigger.getAttribute("aria-expanded") !== "true") fireEvent.click(trigger);
  const id = trigger.getAttribute("aria-controls");
  const list = id === null ? null : document.getElementById(id);
  if (list === null) {
    throw new Error("the select did not open a listbox -- is this a kit Select trigger?");
  }
  return list;
}

/** Open a `Select` and click one option, by the text a person would read. */
export function chooseOption(trigger: HTMLElement, name: string | RegExp): void {
  fireEvent.click(within(openSelect(trigger)).getByRole("option", { name }));
}

/** The label a closed `Select` is currently showing. */
export function selectedLabel(trigger: HTMLElement): string {
  return trigger.textContent ?? "";
}
