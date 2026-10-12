import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { Subhead } from "../../src/kit";
import { AddButton } from "../../src/kit/AddButton";

afterEach(cleanup);

// DESIGN.md rule 1: a subject (GitHub accounts, Senders) is a section beneath
// the page title, "with their Add control on the section heading". Each place
// that drew one wrote the row by hand -- `.os-head`, the Subhead, a
// `.os-head-actions` -- so the row was whatever each author remembered. The
// Subhead draws it now.

describe("Subhead", () => {
  it("puts a subject's Add control on its heading", () => {
    render(<Subhead actions={<AddButton label="Add a sender" onClick={() => {}} />}>Senders</Subhead>);
    const heading = screen.getByRole("heading", { level: 4, name: "Senders" });
    const add = screen.getByRole("button", { name: "Add a sender" });
    const row = heading.parentElement as HTMLElement;
    expect(row.classList.contains("os-head")).toBe(true);
    expect(add.closest(".os-head")).toBe(row);
    // The control stands in the row's action slot, the same one a page Head uses.
    expect(add.closest(".os-head-actions")?.parentElement).toBe(row);
    expect(heading.classList.contains("os-subhead")).toBe(true);
  });

  it("is the plain heading when it has no actions", () => {
    const { container } = render(<Subhead>Senders</Subhead>);
    expect(container.children).toHaveLength(1);
    const only = container.firstElementChild as HTMLElement;
    expect(only.tagName).toBe("H4");
    expect(only.className).toBe("os-subhead");
    expect(container.querySelector(".os-head")).toBeNull();
    expect(container.querySelector(".os-head-actions")).toBeNull();
  });

  it("keeps a quiet count inside the heading, with or without actions", () => {
    const { rerender } = render(<Subhead meta={3}>Senders</Subhead>);
    expect(screen.getByRole("heading", { level: 4 }).querySelector(".os-subhead-meta")?.textContent).toBe("3");
    rerender(<Subhead meta={3} actions={<AddButton label="Add a sender" onClick={() => {}} />}>Senders</Subhead>);
    expect(screen.getByRole("heading", { level: 4 }).querySelector(".os-subhead-meta")?.textContent).toBe("3");
    expect(screen.getByRole("button", { name: "Add a sender" })).toBeTruthy();
  });

  it("keeps its row while the act is absent, so the heading does not change shape when it comes", () => {
    // An act that is not legal is absent (rule 12), and a heading whose act
    // appears later (an Add that waits for the cluster) must not jump a few
    // pixels when it does: the row is there because `actions` was passed, the
    // slot is there only when it has something in it.
    const { container, rerender } = render(<Subhead actions={null}>Sending domain</Subhead>);
    const heading = screen.getByRole("heading", { level: 4, name: "Sending domain" });
    expect(heading.parentElement?.classList.contains("os-head")).toBe(true);
    expect(container.querySelector(".os-head-actions")).toBeNull();

    rerender(<Subhead actions={<AddButton label="Add sending domain" onClick={() => {}} />}>Sending domain</Subhead>);
    expect(screen.getByRole("heading", { level: 4, name: "Sending domain" }).parentElement?.classList.contains("os-head")).toBe(true);
    expect(container.querySelector(".os-head-actions")).not.toBeNull();
  });
});
