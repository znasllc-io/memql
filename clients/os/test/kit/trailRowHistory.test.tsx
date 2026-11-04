import { useRef, useState } from "react";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Head } from "../../src/kit";
import { PageNavigationProvider } from "../../src/kit/pageNavigation";
import { TrailRow } from "../../src/kit/TrailRow";

// THE TRAIL ROW WITH BROWSER HISTORY ON (kit/browserHistory), against jsdom's
// own history: the browser's Back goes where the row's Back goes, the row's
// Back gives the browser's entry back, and a row without it touches nothing.

function Harness({ history }: { history: boolean }) {
  const root = useRef<HTMLDivElement>(null);
  const [page, setPage] = useState<"list" | "detail">("list");
  const toList = () => setPage("list");
  return <div ref={root}>
    <PageNavigationProvider root={root} trail={[]}>
      <TrailRow fallback="Deployables" history={history} />
      {page === "list"
        ? <><Head title="Deployables" /><button type="button" onClick={() => setPage("detail")}>Open MemQL OS</button></>
        : <Head title="MemQL OS" back={{ label: "Deployables", onSelect: toList }} breadcrumbs={[{ label: "Deployables", onSelect: toList }, { label: "MemQL OS" }]} />}
    </PageNavigationProvider>
  </div>;
}

const marked = () => {
  const state = window.history.state as Record<string, unknown> | null;
  return state !== null && typeof state === "object" && "memqlOsTrail" in state;
};

describe("the trail row and the browser's Back", () => {
  it("steps out of a page with the browser's Back, the way the row's Back does", async () => {
    render(<Harness history />);
    fireEvent.click(screen.getByRole("button", { name: "Open MemQL OS" }));
    await waitFor(() => expect(marked()).toBe(true));
    window.history.back();
    await waitFor(() => expect(screen.getByRole("button", { name: "Open MemQL OS" })).toBeTruthy());
    await waitFor(() => expect(marked()).toBe(false));
  });

  it("gives the browser's entry back when the row's own Back is used", async () => {
    render(<Harness history />);
    fireEvent.click(screen.getByRole("button", { name: "Open MemQL OS" }));
    await waitFor(() => expect(marked()).toBe(true));
    fireEvent.click(await screen.findByRole("button", { name: "Back to Deployables" }));
    await waitFor(() => expect(marked()).toBe(false));
    expect(screen.getByRole("button", { name: "Open MemQL OS" })).toBeTruthy();
  });

  it("touches no history when the app has not asked for it", async () => {
    const before = window.history.length;
    render(<Harness history={false} />);
    fireEvent.click(screen.getByRole("button", { name: "Open MemQL OS" }));
    await screen.findByRole("button", { name: "Back to Deployables" });
    expect(window.history.length).toBe(before);
    expect(marked()).toBe(false);
  });
});
