// The per-page guide: the Eye, the dialog, and the role gate on its
// internals (memql#4652, decision D5).
//
// The guide's CONTENT is not asserted here -- prose changes and a test that
// pinned a sentence would fail on every improvement. What is pinned is the
// mechanism: a page with no entry grows no control, the video slot exists
// before any video does, and the technical half is offered to owners and
// admins and to nobody else.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";

import { GUIDE_ENTRIES, guideFor, guideIds } from "../src/guides";
import { PageHeader } from "../src/ui/PageHeader";
import { renderInKit } from "./support/kitHarness";

function openGuide(name: RegExp): void {
  fireEvent.click(screen.getByRole("button", { name }));
}

describe("the guide registry", () => {
  it("has no duplicate ids", () => {
    expect(new Set(guideIds()).size).toBe(GUIDE_ENTRIES.length);
  });

  it("answers undefined for a page nobody wrote a guide for", () => {
    expect(guideFor("no-such-page")).toBeUndefined();
    expect(guideFor(undefined)).toBeUndefined();
  });

  it("gives every entry something to say in both sections", () => {
    // A registry row that exists to satisfy the coverage gate and carries an
    // empty body would pass that gate and teach a reader nothing.
    for (const entry of GUIDE_ENTRIES) {
      expect(entry.title.length).toBeGreaterThan(0);
      expect(entry.body.length).toBeGreaterThan(40);
      expect(entry.how.length).toBeGreaterThan(0);
    }
  });
});

describe("the Eye in PageHeader", () => {
  it("is absent on a page with no guide", () => {
    renderInKit(<PageHeader title="Somewhere" pageId="no-such-page" />);
    expect(screen.queryByRole("button", { name: /What is/ })).toBeNull();
  });

  it("is absent when the page asks for no guide at all", () => {
    renderInKit(<PageHeader title="Somewhere" />);
    expect(screen.queryByRole("button", { name: /What is/ })).toBeNull();
  });

  it("opens the guide for a page that has one", async () => {
    renderInKit(<PageHeader title="Machines" pageId="fleet.machines" />);
    openGuide(/What is Machines\?/);
    await waitFor(() => expect(screen.getByText("What you’re looking at")).toBeTruthy());
    expect(screen.getByText("How it works")).toBeTruthy();
  });

  it("shows the placeholder until a video for the page exists", async () => {
    renderInKit(<PageHeader title="Machines" pageId="fleet.machines" />);
    openGuide(/What is Machines\?/);
    await waitFor(() => expect(screen.getByText("Guide video coming soon")).toBeTruthy());
    // The slot is drawn either way, so the layout does not shift on the day
    // the first video lands.
    expect(document.querySelector("video")).toBeNull();
    expect(document.querySelector(".aspect-video")).toBeTruthy();
  });
});

describe("the guide's technical half", () => {
  it("is not offered to a reader", async () => {
    renderInKit(<PageHeader title="Machines" pageId="fleet.machines" />, { role: "reader" });
    openGuide(/What is Machines\?/);
    await waitFor(() => expect(screen.getByText("How it works")).toBeTruthy());
    expect(screen.queryByText("Technical details")).toBeNull();
    // ...and the concept id that lives in it is nowhere in the document.
    expect(screen.queryByText("v1:worker:registration")).toBeNull();
  });

  for (const role of ["owner", "admin"]) {
    it(`is offered to ${role}, collapsed`, async () => {
      renderInKit(<PageHeader title="Machines" pageId="fleet.machines" />, { role });
      openGuide(/What is Machines\?/);
      await waitFor(() => expect(screen.getByText("Technical details")).toBeTruthy());
      const disclosure = screen.getByText("Technical details").closest("details");
      expect(disclosure?.hasAttribute("open")).toBe(false);
      // The concept id the copy sweep took off the page lives HERE now.
      expect(screen.getByText("v1:worker:registration")).toBeTruthy();
    });
  }
});
