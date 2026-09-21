import { useRef, useState, type ReactNode } from "react";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Head } from "../src/kit";
import { PageNavigationProvider } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";

// The window frame draws ONE trail row and headings publish to it. These
// render the pair the way the frame does: the row first, the app body after.

function Harness({ returnToOverview, returnToList }: { returnToOverview: () => void; returnToList: () => void }) {
  const root = useRef<HTMLDivElement>(null);
  const [page, setPage] = useState("detail");
  return <div ref={root}>
    <button onClick={() => setPage("list")}>Show retained list</button>
    <PageNavigationProvider root={root} trail={[{ label: "Overview", onSelect: returnToOverview }]}>
      <TrailRow fallback="Deployables" />
      <div hidden={page !== "list"}>
        <Head title="Deployables" />
        <Head title="Summary" />
      </div>
      <div hidden={page !== "detail"}>
        <Head title="MemQL OS" back={{ label: "Deployables", onSelect: returnToList }} breadcrumbs={[{ label: "Deployables", onSelect: returnToList }, { label: "MemQL OS" }]} />
      </div>
    </PageNavigationProvider>
  </div>;
}

const crumbs = (path: HTMLElement) => within(path).getAllByRole("listitem").map(item => item.textContent);

describe("shared page return navigation", () => {
  it("merges the window origin with the page trail and keeps the local back action", async () => {
    const overview = vi.fn(), list = vi.fn();
    render(<Harness returnToOverview={overview} returnToList={list} />);
    const path = await screen.findByRole("navigation", { name: "Breadcrumbs" });
    await waitFor(() => expect(crumbs(path)).toEqual(["Overview", "Deployables", "MemQL OS"]));
    expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Back to Deployables" }));
    expect(list).toHaveBeenCalledOnce();
    fireEvent.click(within(path).getByRole("button", { name: "Overview" }));
    expect(overview).toHaveBeenCalledOnce();
  });

  it("moves return navigation to the revealed page, excluding parked panes and secondary headings", async () => {
    render(<Harness returnToOverview={vi.fn()} returnToList={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Show retained list" }));
    const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
    // "Summary" is a second visible heading on the revealed page and must not
    // claim the trail: the first visible heading is the page.
    await waitFor(() => expect(crumbs(path)).toEqual(["Overview", "Deployables"]));
    expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Back to Overview" })).toBeTruthy();
  });

  it("allows a workspace toolbar to leave the one trail with its nested inspector", async () => {
    function Workspace() {
      const root = useRef<HTMLDivElement>(null);
      return <div ref={root}><PageNavigationProvider root={root} trail={[{ label: "Overview", onSelect: vi.fn() }]}>
        <TrailRow fallback="Machines" />
        <Head title="Machines" navigation={false} />
        <Head title="Apps" breadcrumbs={[{ label: "Machines" }, { label: "local" }, { label: "Apps" }]} back={{ label: "local", onSelect: vi.fn() }} />
      </PageNavigationProvider></div>;
    }
    render(<Workspace />);
    await waitFor(() => expect(screen.getByRole("navigation", { name: "Breadcrumbs" }).textContent).toBe("OverviewMachineslocalApps"));
    expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1);
  });
});

describe("the one trail row", () => {
  function Window({ children, trail = [] }: { children: ReactNode; trail?: { label: string; onSelect?: () => void }[] }) {
    const root = useRef<HTMLDivElement>(null);
    return <div ref={root}><PageNavigationProvider root={root} trail={trail}>
      <TrailRow fallback="Deployables" />
      {children}
    </PageNavigationProvider></div>;
  }

  it("is drawn at an app's root, naming the page, with Back in place and inert", async () => {
    render(<Window><Head title="Deployables" /></Window>);
    const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
    await waitFor(() => expect(crumbs(path)).toEqual(["Deployables"]));
    // The slot is always there, so the trail never shifts sideways on the
    // first drill-down -- it simply has nowhere to go yet.
    const back = screen.getByRole("button", { name: "Back" }) as HTMLButtonElement;
    expect(back.disabled).toBe(true);
  });

  it("names the place from the section when no heading has published", () => {
    render(<Window><p>An app body with no heading</p></Window>);
    expect(crumbs(screen.getByRole("navigation", { name: "Breadcrumbs" }))).toEqual(["Deployables"]);
  });

  it("never makes the current page a link, even when it carries a handler", async () => {
    const onSelect = vi.fn();
    render(<Window><Head title="Reviews" breadcrumbs={[{ label: "storefront", onSelect: vi.fn() }, { label: "Reviews", onSelect }]} back={{ label: "storefront", onSelect: vi.fn() }} /></Window>);
    const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
    await waitFor(() => expect(crumbs(path)).toEqual(["storefront", "Reviews"]));
    expect(within(path).queryByRole("button", { name: "Reviews" })).toBeNull();
    expect(within(path).getByText("Reviews").getAttribute("aria-current")).toBe("page");
  });

  it("puts Back before the trail, and headings draw no navigation of their own", async () => {
    render(<Window><Head title="MemQL OS" back={{ label: "Deployables", onSelect: vi.fn() }} /></Window>);
    const back = await screen.findByRole("button", { name: "Back to Deployables" });
    const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
    expect(back.compareDocumentPosition(path) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // One Back in the whole window, and it is the row's -- not the heading's.
    expect(screen.getAllByRole("button", { name: /^Back/ })).toHaveLength(1);
    expect(back.closest(".os-head")).toBeNull();
    expect(back.closest("[data-os-trail-row]")).not.toBeNull();
  });

  it("falls back to the nearest ancestor that goes somewhere when a page names no back", async () => {
    const up = vi.fn();
    render(<Window><Head title="Sandbox" breadcrumbs={[{ label: "Deployables", onSelect: vi.fn() }, { label: "storefront", onSelect: up }, { label: "Sandbox" }]} /></Window>);
    fireEvent.click(await screen.findByRole("button", { name: "Back to storefront" }));
    expect(up).toHaveBeenCalledOnce();
  });

  it("calls the handler a heading published LAST, not the one it rendered with", async () => {
    const first = vi.fn(), second = vi.fn();
    function Page() {
      const [handler, setHandler] = useState(() => first);
      return <>
        <button onClick={() => setHandler(() => second)}>Swap handler</button>
        <Head title="MemQL OS" back={{ label: "Deployables", onSelect: handler }} />
      </>;
    }
    render(<Window><Page /></Window>);
    await screen.findByRole("button", { name: "Back to Deployables" });
    // Same labels, new closure: the signature does not change, so the row does
    // not re-render -- and must still reach the new handler.
    fireEvent.click(screen.getByRole("button", { name: "Swap handler" }));
    fireEvent.click(screen.getByRole("button", { name: "Back to Deployables" }));
    expect(second).toHaveBeenCalledOnce();
    expect(first).not.toHaveBeenCalled();
  });

  it("a heading outside any window keeps its own inline navigation", () => {
    const list = vi.fn();
    render(<Head title="MemQL OS" back={{ label: "Deployables", onSelect: list }} />);
    fireEvent.click(screen.getByRole("button", { name: "Back to Deployables" }));
    expect(list).toHaveBeenCalledOnce();
    expect(screen.getByRole("navigation", { name: "Breadcrumbs" })).toBeTruthy();
  });
});
