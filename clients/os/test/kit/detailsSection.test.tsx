import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { DetailsSection, Input } from "../../src/kit";
import { AddButton } from "../../src/kit/AddButton";

afterEach(cleanup);

// A DETAILS SECTION CAN CARRY ITS OWN ADD CONTROL on its heading (DESIGN.md
// rule 1: a subject's Add control stands on the section heading, `Subhead
// actions`), and a section without one draws exactly as it always did.

describe("DetailsSection", () => {
  it("puts its actions on its heading's row, inside the named section", () => {
    render(<DetailsSection title="Price sheet" actions={<AddButton label="Publish a new version of the price sheet" onClick={() => {}} />}><p>Body</p></DetailsSection>);
    const section = screen.getByRole("region", { name: "Price sheet" });
    const heading = within(section).getByRole("heading", { level: 4, name: "Price sheet" });
    const add = within(section).getByRole("button", { name: "Publish a new version of the price sheet" });
    const row = heading.parentElement as HTMLElement;
    expect(row.classList.contains("os-head")).toBe(true);
    expect(row.parentElement).toBe(section);
    expect(add.closest(".os-head-actions")?.parentElement).toBe(row);
    // The body stays beside the heading, not inside its row.
    expect(within(section).getByText("Body").closest(".os-details-section-body")).toBeTruthy();
    expect(within(section).getByText("Body").closest(".os-head")).toBeNull();
  });

  it("draws the plain heading when it has no actions", () => {
    render(<DetailsSection title="Access"><p>Body</p></DetailsSection>);
    const section = screen.getByRole("region", { name: "Access" });
    const heading = within(section).getByRole("heading", { level: 4, name: "Access" });
    expect(heading.parentElement).toBe(section);
    expect(section.querySelector(".os-head")).toBeNull();
    expect(section.querySelector(".os-head-actions")).toBeNull();
  });
});

// AN ADDRESS IS ASKED IN AN EMAIL FIELD, so a phone offers the address keyboard.
describe("Input", () => {
  it("is an email field when asked for one", () => {
    render(<Input id="buyer-email" label="Email" type="email" value="" onChange={() => {}} />);
    expect((screen.getByLabelText("Email") as HTMLInputElement).type).toBe("email");
  });
});
