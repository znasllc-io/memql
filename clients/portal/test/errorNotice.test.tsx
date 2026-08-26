// ErrorNotice: how this product tells somebody something failed (decision D5).
//
// The behaviour worth pinning is the SPLIT -- a plain sentence for everyone,
// the cluster's own words for the people who can act on them -- because it is
// the half a future edit would most naturally collapse. "Just show the error"
// is a one-line change that reviews fine.

import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";

import { ErrorNotice } from "../src/ui/ErrorNotice";
import { renderInKit } from "./support/kitHarness";

describe("ErrorNotice", () => {
  it("says what happened, and what to do about it", async () => {
    renderInKit(
      <ErrorNotice
        sentence="Could not read the machines."
        next="Reload the page to read them again."
        detail="rpc error: code = Unavailable"
      />,
      { role: "reader" },
    );
    expect(screen.getByText(/Could not read the machines\./)).toBeTruthy();
    expect(screen.getByText(/Reload the page to read them again\./)).toBeTruthy();
  });

  it("interrupts, because a failed action is not a standing observation", () => {
    renderInKit(<ErrorNotice sentence="Could not read the machines." />);
    expect(screen.getByRole("alert")).toBeTruthy();
  });

  it("keeps the cluster's own words away from a reader", async () => {
    renderInKit(
      <ErrorNotice sentence="Could not read the machines." detail="rpc error: code = Unavailable" />,
      { role: "reader" },
    );
    // Awaited so this is not passing merely because the role has not landed:
    // the sentence proves the component rendered, and the absence is then
    // about the gate rather than about timing.
    await waitFor(() => expect(screen.getByText(/Could not read the machines\./)).toBeTruthy());
    expect(screen.queryByText("Technical details")).toBeNull();
    expect(screen.queryByText(/rpc error/)).toBeNull();
  });

  for (const role of ["owner", "admin"]) {
    it(`files them behind a collapsed disclosure for ${role}`, async () => {
      renderInKit(
        <ErrorNotice
          sentence="Could not read the machines."
          detail="rpc error: code = Unavailable"
        />,
        { role },
      );
      await waitFor(() => expect(screen.getByText("Technical details")).toBeTruthy());
      // COLLAPSED: the summary is inside a <details> with no open attribute,
      // so the raw string is present in the document and not on the screen.
      const disclosure = screen.getByText("Technical details").closest("details");
      expect(disclosure).toBeTruthy();
      expect(disclosure?.hasAttribute("open")).toBe(false);
      expect(screen.getByText(/rpc error/)).toBeTruthy();
    });
  }

  it("offers no disclosure when the failure produced no detail", async () => {
    renderInKit(<ErrorNotice sentence="There is no run at this address." />, { role: "owner" });
    await waitFor(() =>
      expect(screen.getByText(/There is no run at this address\./)).toBeTruthy(),
    );
    // An empty "Technical details" is worse than none: it advertises that
    // something was withheld when nothing was.
    expect(screen.queryByText("Technical details")).toBeNull();
  });
});
