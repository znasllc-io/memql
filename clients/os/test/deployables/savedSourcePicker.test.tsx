import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { SavedSourcePicker } from "../../src/apps/deployables/page/stops/compose/SavedSourcePicker";
import { packageFromRow } from "../../src/apps/deployables/packages/rows";

const shop = packageFromRow({ id: "shop", name: "Shop", sourceKind: "repo", repoUrl: "https://github.com/acme/shop.git", repoRef: "main", status: "active" });
const docs = packageFromRow({ id: "docs", name: "Docs", sourceKind: "repo", repoUrl: "https://github.com/acme/docs", status: "active" });
const props = () => ({ sources: [shop, docs], selectedId: "", onChoose: vi.fn(), onAdd: vi.fn(), canAdd: true, busy: false });

describe("saved source picker", () => {
  it("chooses the saved source from a focusable record row, with its repository and branch", () => {
    const actions = props();
    render(<SavedSourcePicker {...actions} />);
    expect(screen.getByRole("heading", { name: "Sources 2" })).toBeTruthy();
    expect(screen.getByRole("list", { name: "Sources" }).classList.contains("os-record-list")).toBe(true);
    const row = screen.getByRole("button", { name: /Shop acme\/shop at main/ });
    expect(row.classList.contains("os-record-row")).toBe(true);
    row.focus();
    expect(document.activeElement).toBe(row);
    expect(row.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(row);
    expect(actions.onChoose).toHaveBeenCalledExactlyOnceWith(shop);
    expect(actions.onAdd).not.toHaveBeenCalled();
    expect(screen.getByText("acme/docs at default branch")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Connect GitHub/ })).toBeNull();
  });

  it("announces the selected source and updates selection without stealing focus", () => {
    const actions = props();
    const view = render(<SavedSourcePicker {...actions} selectedId="shop" />);
    const row = screen.getByRole("button", { name: /Docs acme\/docs/ });
    row.focus();
    view.rerender(<SavedSourcePicker {...actions} selectedId="docs" />);
    expect(document.activeElement).toBe(row);
    expect(row.getAttribute("aria-pressed")).toBe("true");
    expect(row.textContent).toContain("chosen");
    expect(screen.getByRole("button", { name: /Shop acme\/shop/ }).getAttribute("aria-pressed")).toBe("false");
    expect(row.hasAttribute("aria-expanded")).toBe(false);
  });

  it("offers Add source separately and hides it when creation is not authorized", () => {
    const actions = props();
    const view = render(<SavedSourcePicker {...actions} />);
    fireEvent.click(screen.getByRole("button", { name: "Add source" }));
    expect(actions.onAdd).toHaveBeenCalledOnce();
    expect(actions.onChoose).not.toHaveBeenCalled();
    view.rerender(<SavedSourcePicker {...actions} canAdd={false} />);
    expect(screen.queryByRole("button", { name: "Add source" })).toBeNull();
    expect(screen.getByRole("button", { name: /Shop acme\/shop/ })).toBeTruthy();
  });

  it("prevents changing or adding a source during an operation", () => {
    const actions = props();
    render(<SavedSourcePicker {...actions} busy />);
    for (const button of screen.getAllByRole("button")) {
      expect((button as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(button);
    }
    expect(screen.getByRole("region", { name: "Saved sources" }).getAttribute("aria-busy")).toBe("true");
    expect(actions.onChoose).not.toHaveBeenCalled();
    expect(actions.onAdd).not.toHaveBeenCalled();
  });

  it("distinguishes loading, failed, disconnected and settled empty collections without false zero counts", () => {
    const actions = { ...props(), sources: [] };
    const retry = vi.fn();
    const view = render(<SavedSourcePicker {...actions} state="seeding" onRetry={retry} />);
    expect(screen.getByRole("heading", { name: "Sources" }).querySelector(".os-head-meta")).toBeNull();
    expect(screen.getByText("Reading sources…")).toBeTruthy();
    expect(screen.queryByRole("list")).toBeNull();
    view.rerender(<SavedSourcePicker {...actions} error="Access was revoked." onRetry={retry} />);
    expect(screen.getByRole("alert").textContent).toContain("Access was revoked.");
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.getByRole("heading", { name: "Sources" }).querySelector(".os-head-meta")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Refresh sources" }));
    expect(retry).toHaveBeenCalledOnce();
    view.rerender(<SavedSourcePicker {...actions} state="disconnected" onRetry={retry} />);
    expect(screen.getByText(/Sources are unavailable/)).toBeTruthy();
    expect(screen.queryByRole("list")).toBeNull();
    view.rerender(<SavedSourcePicker {...actions} sources={[]} />);
    expect(screen.getByRole("heading", { name: "Sources 0" })).toBeTruthy();
    expect(screen.getByText("No saved sources are available for this account.")).toBeTruthy();
  });
});
