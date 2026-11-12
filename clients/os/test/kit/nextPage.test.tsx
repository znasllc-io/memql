import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { NextPage } from "../../src/kit";
import { intersect, observed } from "../intersectionObserver";

afterEach(cleanup);

// THE NEXT PAGE, LOADED ON SCROLL (DESIGN.md, Record lists). It takes the feed
// `LoadMore` takes, so a list switches by changing one line; what differs is
// that nobody presses anything. The sentinel below the rows coming into view
// is the request, the list's own skeleton rows stand where the next page will
// be, a failure offers its retry in that same place, and the end is said once,
// only when the list did grow.

const INDEX_CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");
const NEXT_PAGE_CSS = readFileSync(join(__dirname, "..", "..", "src", "kit", "nextPage.css"), "utf8");

interface Script {
  /** Pages after the first. */
  pages: number;
  /** Fail the next load with this sentence, once. */
  failWith?: string;
}

/** A paged feed the test drives: each load is held until `finish()`. */
function useScriptedFeed(script: Script) {
  const [loaded, setLoaded] = useState(0);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState("");
  const [failWith, setFailWith] = useState(script.failWith ?? "");
  const loadMore = vi.fn(() => {
    setLoadingMore(true);
    setLoadMoreError("");
    return new Promise<void>((resolve) => {
      pending.push(() => {
        if (failWith) {
          setLoadMoreError(failWith);
          setFailWith("");
        } else {
          setLoaded((n) => n + 1);
        }
        setLoadingMore(false);
        resolve();
      });
    });
  });
  return { feed: { hasMore: loaded < script.pages, loadingMore, loadMoreError, loadMore }, loaded };
}

let pending: (() => void)[] = [];
let calls: ReturnType<typeof vi.fn>[] = [];

/** Settle the held load. */
async function finish() {
  const next = pending.shift();
  if (!next) throw new Error("no load is waiting");
  await act(async () => next());
}

function List({ script }: { script: Script }) {
  const { feed, loaded } = useScriptedFeed(script);
  calls.push(feed.loadMore);
  return (
    <div>
      <ul aria-label="Rows">
        {Array.from({ length: 1 + loaded }, (_, i) => <li key={i}>Page {i + 1}</li>)}
      </ul>
      <NextPage feed={feed} />
    </div>
  );
}

function setup(script: Script) {
  pending = [];
  calls = [];
  const view = render(<List script={script} />);
  return { view, sentinel: () => view.container.querySelector(".os-next-page-sentinel") as HTMLElement | null };
}

/** Loads asked for, across every render's copy of the feed. */
const loadCount = () => calls.reduce((sum, fn) => sum + fn.mock.calls.length, 0);

describe("NextPage", () => {
  it("loads the next page when its sentinel comes into view, and only then", async () => {
    const { sentinel } = setup({ pages: 2 });
    expect(sentinel()).toBeTruthy();
    expect(observed(sentinel()!)).toBe(true);
    expect(loadCount()).toBe(0);

    await act(async () => intersect(sentinel()!, false));
    expect(loadCount()).toBe(0);

    await act(async () => intersect(sentinel()!));
    expect(loadCount()).toBe(1);
    await finish();
    expect(screen.getByText("Page 2")).toBeTruthy();

    // A sentinel still in view after a short page asks again.
    await act(async () => intersect(sentinel()!));
    expect(loadCount()).toBe(2);
    await finish();
    expect(screen.getByText("Page 3")).toBeTruthy();
    expect(sentinel()).toBeNull();
  });

  it("never draws a Load more button", async () => {
    const { sentinel } = setup({ pages: 1 });
    expect(screen.queryByRole("button")).toBeNull();
    await act(async () => intersect(sentinel()!));
    expect(screen.queryByRole("button")).toBeNull();
    await finish();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("holds the list's place with its skeleton rows while the page loads, and asks once", async () => {
    const { view, sentinel } = setup({ pages: 1 });
    const target = sentinel()!;
    await act(async () => intersect(target));
    const status = screen.getByRole("status");
    expect(status.getAttribute("aria-busy")).toBe("true");
    expect(status.closest(".os-next-page")).toBeTruthy();
    expect(view.container.querySelectorAll(".os-record-skeleton .os-record-row").length).toBeGreaterThan(0);
    // No fetch copy on screen: the words are for assistive technology only.
    expect(screen.getByText("Loading more").classList.contains("os-sr-only")).toBe(true);

    // Nothing is watched while a page is out, so a second sighting asks nothing.
    expect(observed(target)).toBe(false);
    await act(async () => intersect(target));
    expect(loadCount()).toBe(1);

    await finish();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("offers a retry in place after a failure, in the engine's words, and does not retry on its own", async () => {
    const { sentinel } = setup({ pages: 1, failWith: "the cursor expired" });
    await act(async () => intersect(sentinel()!));
    await finish();

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("the cursor expired")).toBeTruthy();
    expect(screen.getByText("Page 1")).toBeTruthy();
    // Scrolling past a failure is not a request to fail again.
    expect(sentinel()).toBeNull();
    expect(loadCount()).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(loadCount()).toBe(2);
    expect(screen.getByRole("status")).toBeTruthy();
    await finish();
    expect(screen.getByText("Page 2")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("states the end once, and only after a further page was loaded", async () => {
    // A list that fitted on its first page has no end to announce.
    const single = setup({ pages: 0 });
    expect(screen.queryByText("End of list")).toBeNull();
    expect(single.sentinel()).toBeNull();
    single.view.unmount();

    const { sentinel } = setup({ pages: 1 });
    expect(screen.queryByText("End of list")).toBeNull();
    await act(async () => intersect(sentinel()!));
    expect(screen.queryByText("End of list")).toBeNull();
    await finish();
    expect(screen.getAllByText("End of list")).toHaveLength(1);
  });

  it("moves nothing under reduced motion: its only shapes are the kit's, which stop", async () => {
    const { view, sentinel } = setup({ pages: 1 });
    await act(async () => intersect(sentinel()!));
    const shapes = view.container.querySelectorAll(".os-next-page [aria-hidden] *");
    expect(shapes.length).toBeGreaterThan(0);
    for (const shape of view.container.querySelectorAll(".os-next-page [class*='skeleton-']")) {
      expect(shape.classList.contains("os-skeleton-block")).toBe(true);
    }
    await finish();
    // The sentinel and the end line carry no motion of their own.
    expect(NEXT_PAGE_CSS).not.toMatch(/animation|transition/);
    // The loading rows are the record skeleton, whose pulse the shared
    // stylesheet stops for a reader who asked for less movement.
    expect(INDEX_CSS).toMatch(/@media \(prefers-reduced-motion: reduce\) \{ \.os-skeleton-block \{ animation: none; \} \}/);
  });
});
