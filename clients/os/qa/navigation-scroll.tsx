import { createRoot } from "react-dom/client";
import { useRef, useState } from "react";
import { Head, Panel, RecordList, RecordRow } from "../src/kit";
import { PageContent } from "../src/kit/PageContent";
import { PageNavigationProvider } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Synthetic data only. ?check=1 exercises real browser layout/clamping,
// delayed reads and both return controls. No cluster connection or mutations.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
function Preview({ id }: { id: string }) {
  const root = useRef<HTMLDivElement>(null);
  const [record, setRecord] = useState<number | null>(null);
  const [section, setSection] = useState("machines");
  const [loading, setLoading] = useState(false);
  const back = () => {
    setRecord(null); setLoading(true);
    setTimeout(() => setLoading(false), 300);
  };
  return <section role="dialog" data-preview={id} aria-label={`Fleet ${id}`} style={{ minWidth: 0, background: "var(--os-glass-solid)", border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "hidden" }}>
    <header style={{ padding: "14px 20px" }}><strong>Fleet</strong></header>
    <PageNavigationProvider root={root} scope={section} trail={[]}>
      <nav className="os-window-nav" aria-label="Fleet sections">
        {["machines", "settings"].map(item => <button data-os-scroll-navigation="reset" className="os-window-nav-item" key={item} aria-current={section === item ? "page" : undefined} onClick={() => setSection(item)}>{item === "machines" ? "Machines" : "Settings"}</button>)}
      </nav>
      <TrailRow fallback="Fleet" />
      <PageContent ref={root} className="os-window-content" data-testid="scroll" style={{ height: 380 }}>
        {section === "settings" ? <><Head title="Settings" /><Panel>Settings are kept in this window.</Panel></>
          : record !== null ? <><Head title={`Machine ${record}`} navigationKey={`machine:${record}`} back={{ label: "Machines", onSelect: back }} /><Panel>A short machine detail page.</Panel></>
          : <><Head title="Machines" navigationKey="machines" />{loading ? <div role="status" aria-label="Loading machines" style={{ height: 80 }} /> : <RecordList label="Machines">{Array.from({ length: 40 }, (_, i) => <RecordRow key={i} name={`Machine ${i + 1}`} secondary="Local machine" onOpen={() => setRecord(i + 1)} />)}</RecordList>}</>}
      </PageContent>
    </PageNavigationProvider>
  </section>;
}
createRoot(document.getElementById("root")!).render(<main style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(min(100%, 360px), 1fr))", gap: 24, maxWidth: params.has("narrow") ? 392 : 1160, padding: 16, margin: "20px auto" }}><Preview id="one" /><Preview id="two" /></main>);

if (params.has("check")) {
  const wait = (ms = 50) => new Promise(resolve => setTimeout(resolve, ms));
  const checks: Record<string, boolean> = {};
  const click = (scope: Element, text: string) => {
    const button = Array.from(scope.querySelectorAll("button")).find(node => node.getAttribute("aria-label") === text || node.textContent === text || node.textContent?.startsWith(text + "Local machine"));
    if (!button) throw new Error(`Missing button: ${text}`);
    button.click();
  };
  void (async () => {
    try {
      await wait(400);
      const first = document.querySelector('[data-preview="one"]')!;
      const second = document.querySelector('[data-preview="two"]')!;
      const root = first.querySelector<HTMLElement>('[data-testid="scroll"]')!;
      const other = second.querySelector<HTMLElement>('[data-testid="scroll"]')!;
      root.scrollTop = 980; other.scrollTop = 480; await wait();
      click(root, "Machine 22"); await wait(); checks.newPageStartsAtTop = root.scrollTop === 0;
      click(first, "Back to Machines"); await wait(); checks.loadingIsShort = root.scrollTop === 0;
      await wait(450); checks.backRestores = Math.abs(root.scrollTop - 980) < 1;
      checks.windowsIndependent = Math.abs(other.scrollTop - 480) < 1;
      click(root, "Machine 22"); await wait();
      click(first.querySelector('[aria-label="Breadcrumbs"]')!, "Machines"); await wait(450);
      checks.breadcrumbRestores = Math.abs(root.scrollTop - 980) < 1;
      click(first.querySelector('[aria-label="Fleet sections"]')!, "Settings"); await wait();
      checks.newSectionStartsAtTop = root.scrollTop === 0;
      click(first.querySelector('[aria-label="Fleet sections"]')!, "Machines"); await wait();
      checks.visitedTabStartsAtTop = root.scrollTop === 0;
      root.scrollTop = 980; await wait();
      click(first.querySelector('[aria-label="Fleet sections"]')!, "Machines"); await wait();
      checks.activeTabStartsAtTop = root.scrollTop === 0;
      root.scrollTop = 760; await wait();
      click(root, "Machine 22"); await wait(); click(first, "Back to Machines"); await wait(450);
      checks.backUsesPositionAfterTabReset = Math.abs(root.scrollTop - 760) < 1;
      click(root, "Machine 22"); await wait(); click(first, "Back to Machines"); await wait();
      root.dispatchEvent(new WheelEvent("wheel", { deltaY: 100, bubbles: true }));
      await wait(450); checks.userCancelsPendingRestore = root.scrollTop === 0;
      root.scrollTop = 980; await wait();
      document.title = Object.values(checks).every(Boolean) ? "PASS: scroll restoration" : "FAIL: scroll restoration";
    } catch (error) { document.title = `ERROR: ${error}`; }
    const result = document.createElement("script"); result.id = "scroll-checks"; result.type = "application/json"; result.textContent = JSON.stringify(checks); document.body.append(result);
  })();
}
