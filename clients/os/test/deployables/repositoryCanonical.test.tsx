import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RepositoryPicker } from "../../src/apps/deployables/sources/RepositoryPicker";
import type { RepositoryPage, RepositoryRow } from "../../src/apps/deployables/sources/repositories";

const widget: RepositoryRow = {
  fullName: "acme/widget", owner: "acme", name: "widget", url: "https://github.com/acme/widget",
  private: true, visibility: "private", defaultBranch: "release-candidate", pushedAt: "2026-09-22T00:00:00Z", installationId: "1",
};
const page: RepositoryPage = {
  repositories: [widget, { ...widget, fullName: "acme/docs", name: "docs", private: false, visibility: "public" }],
  installations: [], pending: [{ login: "beta" }], nextPage: 2, reason: "ok",
};
function props() {
  return { page, readAt: "2026-09-22T00:00:00Z", busy: false, refusal: null,
    installUrl: "https://github.com/apps/memql/installations/new", chosen: widget.fullName,
    onChoose: vi.fn(), onLookAgain: vi.fn(), onReadMore: vi.fn() };
}
afterEach(cleanup);

describe("canonical repository choices", () => {
  it("uses semantic shared rows and preserves selection, branch, privacy and push facts", () => {
    const p = props(); render(<RepositoryPicker {...p} />);
    const list = screen.getByRole("list", { name: "acme repositories" });
    expect(list.classList.contains("os-record-list")).toBe(true);
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    const row = within(list).getByRole("button", { name: /widget/ });
    expect(row.classList.contains("os-record-row")).toBe(true);
    expect(row.getAttribute("data-current")).toBe("true");
    expect(row.getAttribute("aria-expanded")).toBe("true");
    expect(within(row).getByTitle("release-candidate")).toBeTruthy();
    expect(within(row).getByText("private")).toBeTruthy();
    expect(within(row).getByText("chosen")).toBeTruthy();
    expect(within(row).getByText(/^pushed /)).toBeTruthy();
    expect(within(list).queryByText("public")).toBeNull();
    fireEvent.click(row);
    expect(p.onChoose).toHaveBeenCalledExactlyOnceWith(widget);
  });

  it("filters grouped counts and leaves pending organizations explicit", () => {
    render(<RepositoryPicker {...props()} />);
    const group = screen.getByRole("group", { name: "acme" });
    const count = () => group.querySelector(".os-subhead-meta")?.textContent;
    expect(count()).toBe("2");
    expect(screen.getByText("Waiting for an owner of beta to approve the app.")).toBeTruthy();
    expect(screen.getByRole("group", { name: "beta" }).querySelector(".os-subhead-meta")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search repositories"), { target: { value: "widget" } });
    expect(count()).toBe("1");
    expect(screen.getByText(/Showing 1 of 2/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Search repositories"), { target: { value: "unmatched" } });
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.getByText(/No repository here matches/)).toBeTruthy();
  });

  it("uses the shared refresh icon and preserves pagination and installation return refresh", () => {
    const p = props(); const view = render(<RepositoryPicker {...p} />);
    const refresh = screen.getByRole("button", { name: "Refresh repositories" });
    expect(refresh.classList.contains("os-refresh")).toBe(true);
    expect(refresh.textContent).toBe("");
    fireEvent.click(refresh); expect(p.onLookAgain).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Read more" }));
    expect(p.onReadMore).toHaveBeenCalledTimes(1);
    const install = screen.getByRole("link", { name: "Install on another organization" });
    expect(install.getAttribute("target")).toBe("_blank");
    expect(install.getAttribute("rel")).toContain("noopener");
    fireEvent.click(install); fireEvent.focus(window); fireEvent.focus(window);
    expect(p.onLookAgain).toHaveBeenCalledTimes(2);
    view.rerender(<RepositoryPicker {...p} busy />);
    expect(refresh.hasAttribute("disabled")).toBe(true);
    expect(refresh.getAttribute("aria-busy")).toBe("true");
    fireEvent.click(refresh); expect(p.onLookAgain).toHaveBeenCalledTimes(2);
  });

  it("keeps prior rows on refusal without claiming a settled count", () => {
    const p = props(); const view = render(<RepositoryPicker {...p} busy />);
    expect(screen.getByRole("group", { name: "acme" }).querySelector(".os-subhead-meta")).toBeNull();
    view.rerender(<RepositoryPicker {...p} refusal={{ code: "rate_limited", message: "Try later" }} />);
    expect(screen.getByRole("group", { name: "acme" }).querySelector(".os-subhead-meta")).toBeNull();
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByText("Try later")).toBeTruthy();
    view.rerender(<RepositoryPicker {...p} readAt="" />);
    expect(screen.getByRole("group", { name: "acme" }).querySelector(".os-subhead-meta")).toBeNull();
  });

  it("distinguishes unread, loading, refused and authoritative empty results", () => {
    const p = { ...props(), page: { ...page, repositories: [], pending: [], nextPage: 0 }, readAt: "" };
    const view = render(<RepositoryPicker {...p} />);
    expect(screen.getByText("Repositories have not been read yet.")).toBeTruthy();
    expect(screen.queryByText("This connection reaches no repositories yet.")).toBeNull();
    view.rerender(<RepositoryPicker {...p} busy />);
    expect(screen.getByText("Reading repositories…")).toBeTruthy();
    expect(screen.queryByText("This connection reaches no repositories yet.")).toBeNull();
    view.rerender(<RepositoryPicker {...p} refusal={{ code: "rate_limited", message: "Try later" }} />);
    expect(screen.getByText("Try later")).toBeTruthy();
    expect(screen.queryByText("This connection reaches no repositories yet.")).toBeNull();
    view.rerender(<RepositoryPicker {...p} readAt="2026-09-22T00:00:00Z" />);
    expect(screen.getByText("This connection reaches no repositories yet.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Install on another organization" })).toBeTruthy();
  });

});
