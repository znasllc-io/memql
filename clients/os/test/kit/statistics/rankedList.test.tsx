import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RankedList, type RankedRow } from "../../../src/kit/statistics/RankedList";

afterEach(cleanup);

// THE RANKED LIST (spec 8): the leading values of one dimension -- pages,
// sources, countries, websites, campaigns -- each a label, a figure, and a
// faint bar behind it sized against the largest. It shows the top eight; the
// rest is a page of its own, opened from the list's heading, never a pop-up.

function pages(count: number): RankedRow[] {
  return Array.from({ length: count }, (_, index) => ({ key: `/page-${index}`, label: `/page-${index}`, value: (count - index) * 10 }));
}

describe("the ranked list", () => {
  it("ranked list shows eight and offers the rest", () => {
    const onShowAll = vi.fn();
    render(<RankedList title="Pages" rows={pages(12)} onShowAll={onShowAll} showAllLabel="All pages" />);
    const section = screen.getByRole("region", { name: "Pages" });
    expect(within(section).getByRole("heading", { level: 4, name: "Pages" })).toBeTruthy();
    const items = within(section).getAllByRole("listitem");
    expect(items).toHaveLength(8);
    expect(items[0]!.querySelector(".os-ranked-label")?.textContent).toBe("/page-0");
    expect(items[0]!.querySelector(".os-ranked-value")?.textContent).toBe("120");
    // The way to the rest stands on the list's heading, as the same up-right arrow used in Fleet.
    const all = within(section).getByRole("button", { name: "All pages" });
    expect(all.closest(".os-head-actions")).not.toBeNull();
    expect(all.classList.contains("os-icon-button")).toBe(true);
    expect(all.querySelector("svg.lucide-arrow-up-right")).toBeTruthy();
    expect(all.textContent).toBe("");
    expect(all.title).toBe("All pages");
    fireEvent.click(all);
    expect(onShowAll).toHaveBeenCalledTimes(1);
  });

  it("offers nothing more when every row is already shown", () => {
    const { rerender } = render(<RankedList title="Pages" rows={pages(8)} onShowAll={() => {}} showAllLabel="All pages" />);
    expect(screen.getAllByRole("listitem")).toHaveLength(8);
    expect(screen.queryByRole("button", { name: "All pages" })).toBeNull();
    // A list of its own size takes the limit it is given.
    rerender(<RankedList title="Pages" rows={pages(3)} limit={2} onShowAll={() => {}} showAllLabel="All pages" />);
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "All pages" })).toBeTruthy();
  });

  it("bars are proportional to the largest value", () => {
    const { container } = render(
      <RankedList
        title="Sources"
        rows={[
          { key: "direct", label: "Direct", value: 200 },
          { key: "search", label: "search.example", value: 100 },
          { key: "news", label: "news.example", value: 50 },
          { key: "none", label: "quiet.example", value: 0 },
        ]}
      />,
    );
    const bars = [...container.querySelectorAll<HTMLElement>(".os-ranked-bar")];
    expect(bars.map((bar) => bar.style.width)).toEqual(["100%", "50%", "25%", "0%"]);
    expect(bars.every((bar) => bar.getAttribute("aria-hidden") === "true")).toBe(true);
  });

  it("writes each value in the list's format, grouped by default", () => {
    const { rerender } = render(<RankedList title="Countries" rows={[{ key: "de", label: "Germany", value: 12040 }]} />);
    expect(screen.getByText("12,040")).toBeTruthy();
    rerender(<RankedList title="Clients" rows={[{ key: "a", label: "First client", value: 0.42 }]} format={(value) => `${Math.round(value * 100)}%`} />);
    expect(screen.getByText("42%")).toBeTruthy();
  });

  it("opens a row that can be opened, and draws the others as plain rows", () => {
    const onOpen = vi.fn();
    render(
      <RankedList
        title="Websites"
        rows={[
          { key: "shop", label: "shop.example", value: 90, secondary: "42% opened", onOpen },
          { key: "docs", label: "docs.example", value: 40 },
        ]}
      />,
    );
    const [first, second] = screen.getAllByRole("listitem");
    const open = within(first!).getByRole("button");
    expect(open.textContent).toContain("42% opened");
    fireEvent.click(open);
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(within(second!).queryByRole("button")).toBeNull();
  });

  it("cuts a long label short on screen and keeps all of it in its title", () => {
    const label = "/collections/winter/a-very-long-product-handle-that-will-not-fit?variant=1234567890";
    render(<RankedList title="Pages" rows={[{ key: label, label, value: 3 }]} />);
    const shown = screen.getByText(label);
    expect(shown.classList.contains("os-ranked-label")).toBe(true);
    expect(shown.getAttribute("title")).toBe(label);
  });
});

describe("its states", () => {
  it("says it is empty only once a read came back empty", () => {
    const { rerender } = render(<RankedList title="Countries" rows={[]} loading />);
    const shape = screen.getByRole("status");
    expect(shape.getAttribute("aria-busy")).toBe("true");
    expect(shape.closest(".os-record-skeleton")).not.toBeNull();
    expect(screen.queryByText("Nothing in this period.")).toBeNull();

    rerender(<RankedList title="Countries" rows={[]} empty="Nothing in this period." />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getByText("Nothing in this period.").classList.contains("os-caption")).toBe(true);
  });

  it("says what a page gives it for an empty answer, a sentence or more", () => {
    render(<RankedList title="Products" rows={[]} empty={<><span className="marked">-</span> Not known.</>} />);
    const caption = screen.getByRole("region", { name: "Products" }).querySelector(".os-caption");
    expect(caption?.querySelector(".marked")).not.toBeNull();
    expect(caption?.textContent).toBe("- Not known.");
  });

  it("keeps its rows while a refresh is read", () => {
    render(<RankedList title="Pages" rows={pages(3)} loading />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
  });

  it("stands its footer under the rows, and only under rows", () => {
    const footer = <a href="https://db-ip.com">IP Geolocation by DB-IP</a>;
    const { rerender } = render(<RankedList title="Countries" rows={pages(2)} footer={footer} />);
    const credit = screen.getByRole("link", { name: "IP Geolocation by DB-IP" });
    expect(credit.closest(".os-ranked-footer")).not.toBeNull();
    rerender(<RankedList title="Countries" rows={[]} footer={footer} />);
    expect(screen.queryByRole("link", { name: "IP Geolocation by DB-IP" })).toBeNull();
  });

  it("stands a control under its heading and above its rows, in every state", () => {
    const control = <button type="button">Browser</button>;
    const { rerender } = render(<RankedList title="Devices" rows={pages(2)} control={control} />);
    const section = screen.getByRole("region", { name: "Devices" });
    const heading = within(section).getByRole("heading", { name: "Devices" });
    const tab = within(section).getByRole("button", { name: "Browser" });
    const list = within(section).getByRole("list");
    // Document order: heading, then the control, then the rows.
    expect(heading.compareDocumentPosition(tab) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(tab.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(tab.closest(".os-ranked-control")).not.toBeNull();
    // The control stays while the list reads and when it comes back empty.
    rerender(<RankedList title="Devices" rows={[]} control={control} loading />);
    expect(screen.getByRole("button", { name: "Browser" })).toBeTruthy();
    rerender(<RankedList title="Devices" rows={[]} control={control} />);
    expect(screen.getByRole("button", { name: "Browser" })).toBeTruthy();
  });
});

describe("its look", () => {
  it("uses the kit's pieces as they are: a heading's Button and the record list's loading shape", () => {
    // "All pages" looks like every other act on a Subhead, and the loading
    // shape is the record list's own, so neither is restyled from here.
    const css = readFileSync(join(__dirname, "..", "..", "..", "src", "kit", "statistics", "statistics.css"), "utf8");
    expect(css).not.toMatch(/\.os-button/);
    expect(css).not.toMatch(/os-record-skeleton|os-record-row/);
  });
});
