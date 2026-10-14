import { createRoot } from "react-dom/client";
import { useState } from "react";
import { X } from "lucide-react";
import { Head, Panel, Subhead } from "../src/kit";
import { AddButton, AddLink } from "../src/kit/AddButton";
import { IconButton } from "../src/kit/IconButton";
import { EntryList } from "../src/kit/EntryList";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

const params = new URLSearchParams(location.search);
const mode = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.theme = mode;
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);

function Preview() {
  const [draft, setDraft] = useState("");
  const [entries, setEntries] = useState<string[]>([]);
  return <main className="os-window-content" style={{ maxWidth: 920, margin: "24px auto", height: "calc(100dvh - 48px)", background: "var(--os-glass-solid)" }}>
    <Head title="Creation controls"><AddButton label="Add an item" onClick={() => {}} /></Head>
    <Panel><Subhead actions={<AddLink label="Create a repository" href="#repository" />}>Repositories</Subhead><p className="os-caption">A link that continues in another page.</p></Panel>
    <Panel><EntryList entries={entries} label="Email domains" addLabel="Add domain" placeholder="example.com" draft={draft} error="" onDraft={setDraft} onAdd={() => { if (draft.trim()) { setEntries([...entries, draft.trim()]); setDraft(""); } }} onRemove={value => setEntries(entries.filter(entry => entry !== value))} /></Panel>
    <Panel><Subhead>Compact actions</Subhead><div style={{ display: "flex", alignItems: "center", gap: 12 }}><AddButton label="New conversation" style={{ minWidth: 28, minHeight: 28 }} onClick={() => {}} /><AddButton label="Insert step 1" style={{ minWidth: 28, minHeight: 28 }} onClick={() => {}} /><IconButton label="Close"><X size={16} aria-hidden /></IconButton></div></Panel>
  </main>;
}
createRoot(document.getElementById("root")!).render(<Preview />);
requestAnimationFrame(() => requestAnimationFrame(() => {
  if (params.get("focus") === "1") document.querySelector<HTMLButtonElement>('[aria-label="Add an item"]')?.focus();
  document.title = "loaded";
  const states = [...document.querySelectorAll<HTMLElement>(".os-add-button")].map(el => {
    const css = getComputedStyle(el); const bounds = el.getBoundingClientRect();
    return { label: el.getAttribute("aria-label"), border: css.border, width: bounds.width, height: bounds.height, disabled: el.hasAttribute("disabled"), outline: css.outline };
  });
  const facts = document.createElement("script"); facts.id = "qa-add-button-facts"; facts.type = "application/json"; facts.textContent = JSON.stringify(states); document.body.append(facts);
}));
