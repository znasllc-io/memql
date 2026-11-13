import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { NextPage } from "../../src/kit";
import { LoadMore } from "../../src/kit/LoadMore";
import { LiveReadVisibility } from "../../src/live/useGraphRevision";
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
  return { feed: { hasMore: loaded < script.pages, loadingMore, loadMoreError, loadMore, progress: loaded }, loaded };
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

// A feed the test sets by hand, for the two cases a scripted feed cannot
// reach: a load that settles having made no progress, and a feed that stops
// offering more after a failure without a page ever arriving.
interface Manual {
  hasMore: boolean;
  loadMoreError: string;
  progress: number;
}

let setManual: (next: Manual) => void = () => {};
let manualLoads = 0;

function ManualList({ initial }: { initial: Manual }) {
  const [state, set] = useState(initial);
  setManual = set;
  const feed = {
    hasMore: state.hasMore,
    loadingMore: false,
    loadMoreError: state.loadMoreError,
    progress: state.progress,
    loadMore: async () => { manualLoads += 1; },
  };
  return <NextPage feed={feed} />;
}

describe("NextPage, guarded on progress", () => {
  function setupManual(initial: Manual) {
    manualLoads = 0;
    const view = render(<ManualList initial={initial} />);
    return () => view.container.querySelector(".os-next-page-sentinel") as HTMLElement | null;
  }

  it("does not ask again at once after a load that brought nothing, while the sentinel stays in view", async () => {
    const sentinel = setupManual({ hasMore: true, loadMoreError: "", progress: 0 });
    await act(async () => intersect(sentinel()!));
    expect(manualLoads).toBe(1);

    // The re-armed observer reports the sentinel still in view, which a real
    // browser does the moment it starts watching. Nothing changed, so it is
    // not a request: asking now would ask again and again.
    await act(async () => intersect(sentinel()!));
    await act(async () => intersect(sentinel()!));
    expect(manualLoads).toBe(1);

    // Leaving the view and coming back is a request.
    await act(async () => intersect(sentinel()!, false));
    await act(async () => intersect(sentinel()!));
    expect(manualLoads).toBe(2);
  });

  it("asks again at once once the feed has moved on", async () => {
    const sentinel = setupManual({ hasMore: true, loadMoreError: "", progress: 0 });
    await act(async () => intersect(sentinel()!));
    expect(manualLoads).toBe(1);
    await act(async () => setManual({ hasMore: true, loadMoreError: "", progress: 1 }));
    await act(async () => intersect(sentinel()!));
    expect(manualLoads).toBe(2);
  });

  it("does not state the end after a failed load that brought no page", async () => {
    const sentinel = setupManual({ hasMore: true, loadMoreError: "", progress: 0 });
    await act(async () => intersect(sentinel()!));
    await act(async () => setManual({ hasMore: true, loadMoreError: "the cursor expired", progress: 0 }));
    expect(screen.getByText("the cursor expired")).toBeTruthy();
    // The feed gives up on more without a page having arrived.
    await act(async () => setManual({ hasMore: false, loadMoreError: "", progress: 0 }));
    expect(screen.queryByText("End of list")).toBeNull();
  });
});

describe("NextPage, a feed that says nothing of its progress", () => {
  it("does not state the end after a load that rejected without saying why", async () => {
    let hasMore = true;
    let rerender: () => void = () => {};
    function Bare() {
      const [, set] = useState(0);
      rerender = () => set((n) => n + 1);
      const feed = {
        hasMore,
        loadingMore: false,
        loadMoreError: "",
        loadMore: async () => { throw new Error("dropped"); },
      };
      return <NextPage feed={feed} />;
    }
    const view = render(<Bare />);
    await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
    hasMore = false;
    await act(async () => rerender());
    expect(screen.queryByText("End of list")).toBeNull();
  });
});


describe("shared list continuation lifecycle", () => {
  it("moves legacy lists onto scrolling without a paging button", async () => {
    const loadMore = vi.fn(async () => {});
    const view = render(<LoadMore feed={{ hasMore: true, loadingMore: false, loadMoreError: "", loadMore }} />);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
    expect(loadMore).toHaveBeenCalledTimes(1);
  });

  it("does not page a parked window and resumes when it becomes visible", async () => {
    const loadMore = vi.fn(async () => {});
    const feed = { hasMore: true, loadingMore: false, loadMoreError: "", loadMore };
    const view = render(<LiveReadVisibility.Provider value={false}><NextPage feed={feed} /></LiveReadVisibility.Provider>);
    const target = view.container.querySelector(".os-next-page-sentinel")!;
    expect(observed(target)).toBe(false);
    await act(async () => intersect(target));
    expect(loadMore).not.toHaveBeenCalled();
    view.rerender(<LiveReadVisibility.Provider value={true}><NextPage feed={feed} /></LiveReadVisibility.Provider>);
    await act(async () => intersect(target));
    expect(loadMore).toHaveBeenCalledTimes(1);
  });

  it("makes a rejected request retryable even when the feed cannot publish its error", async () => {
    const loadMore = vi.fn().mockRejectedValueOnce(new Error("Page unavailable")).mockResolvedValue(undefined);
    const view = render(<NextPage feed={{ hasMore: true, loadingMore: false, loadMoreError: "", loadMore }} />);
    await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
    expect(screen.getByText("Page unavailable")).toBeTruthy();
    expect(view.container.querySelector(".os-next-page-sentinel")).toBeNull();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Try again" })));
    expect(loadMore).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

it("does not let a previous filtered read settle into the current list", async () => {
  let finish!: () => void;
  const oldLoad = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  const currentLoad = vi.fn(async () => {});
  const initial = { hasMore: true, loadingMore: false, loadMoreError: "", loadMore: oldLoad, scope: "old", progress: 1 };
  const view = render(<NextPage feed={initial} />);
  await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
  view.rerender(<NextPage feed={{ ...initial, hasMore: false, loadMore: currentLoad, scope: "new" }} />);
  await act(async () => finish());
  expect(screen.queryByText("End of list")).toBeNull();
  expect(screen.queryByRole("status")).toBeNull();
  view.rerender(<NextPage feed={{ ...initial, loadMore: currentLoad, scope: "new" }} />);
  await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
  expect(currentLoad).toHaveBeenCalledTimes(1);
});

it("waits for feed recovery before paging and keeps loaded rows in place", async () => {
  const loadMore = vi.fn(async () => {});
  const feed = { hasMore: true, loadingMore: false, loadMoreError: "", loadMore, enabled: false };
  const view = render(<><p>Existing row</p><NextPage feed={feed} /></>);
  const row = screen.getByText("Existing row");
  await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
  expect(loadMore).not.toHaveBeenCalled();
  view.rerender(<><p>Existing row</p><NextPage feed={{ ...feed, enabled: true }} /></>);
  await act(async () => intersect(view.container.querySelector(".os-next-page-sentinel")!));
  expect(loadMore).toHaveBeenCalledTimes(1);
  expect(screen.getByText("Existing row")).toBe(row);
});
