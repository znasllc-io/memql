import { useLayoutEffect, useRef, useState } from "react";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Head } from "../src/kit";
import { PageNavigationProvider, usePageScroll } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";
import { createPageScroll } from "../src/kit/pageScroll";

// jsdom has no layout. Model the browser's clamping when the outgoing long
// page is replaced by a short detail/loading state before layout cleanup.
function geometry(node: HTMLElement, initial = 1600) {
  let height = initial, top = 0, left = 0;
  Object.defineProperties(node, {
    clientHeight: { configurable: true, get: () => 200 },
    scrollHeight: { configurable: true, get: () => height },
    scrollTop: { configurable: true, get: () => top, set: value => { top = Math.max(0, Math.min(value, height - 200)); } },
    scrollLeft: { configurable: true, get: () => left, set: value => { left = value; } },
    scrollTo: { configurable: true, value: (options: ScrollToOptions) => {
      node.scrollTop = options.top ?? node.scrollTop;
      node.scrollLeft = options.left ?? node.scrollLeft;
    } },
  });
  return (next: number) => { height = next; node.scrollTop = top; };
}
const settle = () => act(async () => { await Promise.resolve(); });

function Window({ name = "window" }: { name?: string }) {
  const root = useRef<HTMLDivElement>(null);
  const size = useRef<(height: number) => void>(() => {});
  const [page, setPage] = useState("Machines");
  useLayoutEffect(() => { size.current = geometry(root.current!); }, []);
  useLayoutEffect(() => { size.current(page === "Machines" ? 1600 : 200); }, [page]);
  return <section role="dialog" aria-label={name}>
    <PageNavigationProvider root={root} trail={[]}>
      <TrailRow fallback="Fleet" />
      <div ref={root} data-testid={`${name}-scroll`}>
        <Head title={page} back={page === "Machines" ? undefined : { label: "Machines", onSelect: () => setPage("Machines") }} />
        <button onClick={() => setPage("Machine")}>Inspect machine</button>
      </div>
    </PageNavigationProvider>
  </section>;
}

describe("window scroll restoration", () => {
  it.each(["Back to Machines", "Machines"])("returns to the position before a short detail page through %s", async label => {
    render(<Window />); await settle();
    const root = screen.getByTestId("window-scroll");
    root.scrollTop = 740;
    // Capture at navigation also covers a scroll whose browser event is queued.
    fireEvent.click(screen.getByText("Inspect machine")); await settle();
    expect(root.scrollTop).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: label })); await settle();
    expect(root.scrollTop).toBe(740);
  });

  it("isolates positions in separate windows and drops them when a window closes", async () => {
    const view = render(<><Window name="first" /><Window name="second" /></>); await settle();
    for (const [name, top] of [["first", 380], ["second", 920]] as const) {
      const root = screen.getByTestId(`${name}-scroll`); root.scrollTop = top;
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByText("Inspect machine"));
      await settle();
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByRole("button", { name: "Back to Machines" }));
      await settle(); expect(root.scrollTop).toBe(top);
    }
    view.unmount(); render(<Window name="first" />); await settle();
    expect(screen.getByTestId("first-scroll").scrollTop).toBe(0);
  });

  it("uses stable workspace identities even when pages share the same heading", async () => {
    function Workspace() {
      const root = useRef<HTMLDivElement>(null);
      const [id, setId] = useState("one");
      const page = usePageScroll(id);
      return <section><button onClick={() => setId(id === "one" ? "two" : "one")}>Switch</button>
        <PageNavigationProvider root={root} trail={[]}><div ref={root} data-testid="workspace">
          <div ref={page}><Head title="Settings" /></div>
        </div></PageNavigationProvider></section>;
    }
    render(<Workspace />); await settle();
    const root = screen.getByTestId("workspace"); geometry(root);
    root.scrollTop = 420; fireEvent.scroll(root);
    fireEvent.click(screen.getByText("Switch")); await settle(); expect(root.scrollTop).toBe(0);
    root.scrollTop = 180; fireEvent.scroll(root);
    fireEvent.click(screen.getByText("Switch")); await settle(); expect(root.scrollTop).toBe(420);
  });
});

function memory() {
  const frame = document.createElement("section"); frame.setAttribute("role", "dialog");
  const root = document.createElement("div"); frame.append(root); document.body.append(frame);
  const size = geometry(root);
  let key: string | null = "list";
  const host = createPageScroll(root, () => key);
  return { root, size, host, go(next: string | null) { key = next; host.schedule(); }, dispose() { host.dispose(); frame.remove(); } };
}

it("waits through short loading content, restores both axes, and leaves rerenders alone", async () => {
  const test = memory(); await settle();
  try {
    test.root.scrollTop = 850; test.root.scrollLeft = 35; fireEvent.scroll(test.root);
    test.go("detail"); test.size(200); await settle();
    test.go("list"); await settle(); expect(test.root.scrollTop).toBe(0);
    test.size(1600); test.root.append(document.createElement("p")); await settle();
    expect(test.root.scrollTop).toBe(850); expect(test.root.scrollLeft).toBe(35);
    test.root.scrollTop = 120; fireEvent.scroll(test.root); test.host.schedule(); await settle();
    expect(test.root.scrollTop).toBe(120);
    test.root.scrollTop = 0; fireEvent.scroll(test.root);
    test.go("detail"); await settle(); test.go("list"); await settle(); expect(test.root.scrollTop).toBe(0);
  } finally { test.dispose(); }
});

it("lets user input cancel a pending restore and ignores late content after navigation", async () => {
  const test = memory(); await settle();
  try {
    test.root.scrollTop = 800; fireEvent.scroll(test.root);
    test.go("detail"); test.size(200); await settle(); test.go("list"); await settle();
    fireEvent.wheel(test.root); test.size(1600); test.root.append(document.createElement("p")); await settle();
    expect(test.root.scrollTop).toBe(0);
    test.go("other"); await settle(); test.root.append(document.createElement("p")); await settle();
    expect(test.root.scrollTop).toBe(0);
  } finally { test.dispose(); }
});

it("remembers nested scrollers across unmounts and excludes hidden retained panes", async () => {
  const test = memory();
  const pane = () => { const node = document.createElement("div"); node.style.overflowY = "auto"; node.dataset.osScrollId = "list"; geometry(node); test.root.append(node); return node; };
  let list = pane(); await settle();
  try {
    list.scrollTop = 620; fireEvent.scroll(list);
    test.go("detail"); list.remove(); await settle();
    list = pane(); test.go("list"); await settle(); expect(list.scrollTop).toBe(620);
    list.hidden = true; list.scrollTop = 0; fireEvent.scroll(list); test.go(null); await settle();
    list.hidden = false; test.go("list"); await settle(); expect(list.scrollTop).toBe(620);
  } finally { test.dispose(); }
});

it("does not reset a renamed page and separates records with identical titles", async () => {
  function Records() {
    const root = useRef<HTMLDivElement>(null);
    const [id, select] = useState("a");
    const [name, rename] = useState("Shared name");
    return <section><PageNavigationProvider root={root} trail={[]}>
      <button onClick={() => select(id === "a" ? "b" : "a")}>Other record</button>
      <div ref={root} data-testid="records"><Head title={name} navigationKey={id} />
        <button onClick={() => rename("Renamed")}>Rename</button>
      </div>
    </PageNavigationProvider></section>;
  }
  render(<Records />); await settle();
  const root = screen.getByTestId("records"); geometry(root);
  root.scrollTop = 460; fireEvent.scroll(root);
  fireEvent.click(screen.getByText("Other record")); await settle(); expect(root.scrollTop).toBe(0);
  fireEvent.click(screen.getByText("Other record")); await settle(); expect(root.scrollTop).toBe(460);
  fireEvent.click(screen.getByText("Rename")); await settle(); expect(root.scrollTop).toBe(460);
});

it("restores sections when the phone scroller wraps the navigation provider", async () => {
  function Phone() {
    const root = useRef<HTMLDivElement>(null);
    const [section, select] = useState("machines");
    useLayoutEffect(() => { geometry(root.current!); }, []);
    return <div ref={root} data-testid="phone-scroll">
      <PageNavigationProvider root={root} scope={section} trail={[]}>
        <button onClick={() => select(section === "machines" ? "settings" : "machines")}>Switch section</button>
        <Head title={section} />
      </PageNavigationProvider>
    </div>;
  }
  render(<Phone />); await settle();
  const root = screen.getByTestId("phone-scroll");
  root.scrollTop = 530; fireEvent.scroll(root);
  fireEvent.click(screen.getByText("Switch section")); await settle(); expect(root.scrollTop).toBe(0);
  fireEvent.click(screen.getByText("Switch section")); await settle(); expect(root.scrollTop).toBe(530);
});

it("leaves log followers and dialog scrolling under their own control", async () => {
  const test = memory();
  const log = document.createElement("div"); log.dataset.osScrollIgnore = "";
  const dialog = document.createElement("div"); dialog.setAttribute("role", "dialog");
  const nativeDialog = document.createElement("dialog"); nativeDialog.open = true;
  const popover = document.createElement("div"); popover.setAttribute("popover", "");
  const independent = [log, dialog, nativeDialog, popover];
  for (const node of independent) {
    node.style.overflowY = "auto"; geometry(node); node.scrollTop = 760; test.root.append(node);
  }
  try {
    await settle(); independent.forEach(node => expect(node.scrollTop).toBe(760));
    test.go("detail"); await settle(); independent.forEach(node => expect(node.scrollTop).toBe(760));
  } finally { test.dispose(); }
});
