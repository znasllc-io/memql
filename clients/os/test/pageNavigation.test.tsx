import { useRef, useState } from "react";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Head } from "../src/kit";
import { PageNavigationProvider } from "../src/kit/pageNavigation";

function Harness({ returnToOverview, returnToList }: { returnToOverview: () => void; returnToList: () => void }) {
  const root = useRef<HTMLDivElement>(null);
  const [page, setPage] = useState("detail");
  return <div ref={root}>
    <button onClick={() => setPage("list")}>Show retained list</button>
    <PageNavigationProvider root={root} trail={[{ label: "Overview", onSelect: returnToOverview }]}>
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

describe("shared page return navigation", () => {
  it("merges the window origin with the page trail and keeps the local back action", async () => {
    const overview = vi.fn(), list = vi.fn();
    render(<Harness returnToOverview={overview} returnToList={list} />);
    const path = await screen.findByRole("navigation", { name: "Breadcrumbs" });
    expect(screen.getAllByRole("navigation", { name: "Breadcrumbs" })).toHaveLength(1);
    expect(within(path).getAllByRole("listitem").map(item => item.textContent)).toEqual(["Overview", "Deployables", "MemQL OS"]);
    expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Back to Deployables" }));
    expect(list).toHaveBeenCalledOnce();
    fireEvent.click(within(path).getByRole("button", { name: "Overview" }));
    expect(overview).toHaveBeenCalledOnce();
  });

  it("moves return navigation to the revealed page, excluding parked panes and secondary headings", async () => {
    render(<Harness returnToOverview={vi.fn()} returnToList={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Show retained list" }));
    await waitFor(() => expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1));
    expect(screen.getByRole("button", { name: "Back to Overview" }).closest(".os-head")?.textContent).toBe("Deployables");
    const path = screen.getByRole("navigation", { name: "Breadcrumbs" });
    expect(within(path).getAllByRole("listitem").map(item => item.textContent)).toEqual(["Overview", "Deployables"]);
  });

  it("allows a workspace toolbar to leave the one trail with its nested inspector", async () => {
    function Workspace() {
      const root = useRef<HTMLDivElement>(null);
      return <div ref={root}><PageNavigationProvider root={root} trail={[{ label: "Overview", onSelect: vi.fn() }]}>
        <Head title="Machines" navigation={false} />
        <Head title="Apps" breadcrumbs={[{ label: "Machines" }, { label: "local" }, { label: "Apps" }]} back={{ label: "local", onSelect: vi.fn() }} />
      </PageNavigationProvider></div>;
    }
    render(<Workspace />);
    await waitFor(() => expect(screen.getByRole("navigation", { name: "Breadcrumbs" }).textContent).toBe("OverviewMachineslocalApps"));
    expect(screen.getAllByRole("button", { name: /^Back to/ })).toHaveLength(1);
  });
});
