import { useState } from "react";
import { createRoot } from "react-dom/client";
import { TrainingApp } from "../src/apps/training/TrainingApp";
import { LocalTrainingSettingsStore } from "../src/apps/training/settings";
import { LocalTabs } from "../src/kit/LocalTabs";
import { applyPackStyles } from "../src/themes/registry";
import { installQaConnection } from "./connectionShim";
import { chunkRow, domainLiteRow, fakeConnection, withSession } from "../test/training/harness";
import "../src/styles/index.css";

// Synthetic records only: no live cluster, upload, or model calls.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
const text = `${"Preserve the reference and distinguish attempts from verified results. ".repeat(7)}Final qualification: the reported outcome still needs independent verification.`;
const chunks = [
  chunkRow({ id: "upload", text, sourceRef: `artifact:${"abcdef0123456789".repeat(6)}` }),
  chunkRow({ id: "chat", text: "Check the request before making changes.", source: "augment", sourceRef: "chat:example-message" }),
  chunkRow({ id: "seed", text: "Seeded guidance stays separately attributed.", source: "llmSeeded", sourceRef: "catalog:example" }),
];
const empty = params.get("state") === "empty";
installQaConnection(fakeConnection({
  domainRows: empty ? [] : chunks.map(() => domainLiteRow("software-development", "unvalidated")),
  chunkPages: { "software-development": [empty ? [] : chunks.map((chunk) => ({ ...chunk, domainId: "software-development" }))] },
  decisionError: params.get("state") === "error" ? "The review could not be saved. Try again." : undefined,
}));
const data = new Map<string, string>();
const store = new LocalTrainingSettingsStore({ getItem: (key) => data.get(key) ?? null, setItem: (key, value) => void data.set(key, value) });
function Fixture() {
  const [section, navigate] = useState(params.get("view") === "domains" ? "domains" : "review");
  return withSession(<main style={{ margin: "20px auto", width: "min(100% - 24px, 920px)", height: "calc(100dvh - 40px)", padding: 16, border: "1px solid var(--os-line)", borderRadius: "var(--os-radius)", overflow: "auto", background: "var(--os-glass-solid)" }}>
    <LocalTabs label="Training sections" value={section} onChange={navigate} options={[["review", "Review"], ["domains", "Domains"]]} />
    <TrainingApp sectionId={section} navigate={navigate} askContext={() => {}} store={store} />
  </main>);
}
createRoot(document.getElementById("root")!).render(<Fixture />);
