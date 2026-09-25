import { createRoot } from "react-dom/client";
import { BUILT_IN_PACKS } from "../src/themes/builtins";
import { DeskNumeral, FoldWallpaper } from "../src/wallpaper/FoldWallpaper";
import "../src/styles/index.css";

const params = new URLSearchParams(location.search);
const pack = BUILT_IN_PACKS.find((p) => p.id === params.get("theme")) ?? BUILT_IN_PACKS[0]!;
const mode = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.theme = mode;
document.documentElement.dataset.osTheme = pack.id;
for (const [token, value] of Object.entries(pack.tokens[mode])) {
  document.documentElement.style.setProperty(`--os-${token}`, value);
}
createRoot(document.getElementById("root")!).render(
  <main className="os-root">
    <FoldWallpaper />
    <DeskNumeral index={0} />
    {params.has("window") && <section className="os-window" data-os-window style={{ position: "absolute", top: "16%", left: "16%", width: "68%", height: "64%" }}>
      <header className="os-window-bar"><strong>Settings</strong></header>
      <div style={{ padding: "24px" }}><h2>Settings</h2><h3>Connections</h3><p>GitHub Accounts</p><hr /><p>Shopify Stores</p></div>
    </section>}
  </main>,
);
