import { EDITOR_COLOR_KEYS, readEditorAppearance, type EditorAppearance } from "../../../../brand/editorAppearance";

/** Snapshot the actual rendered pack, including imported packs and System mode. */
export function currentEditorAppearance(): EditorAppearance | undefined {
  if (typeof document === "undefined") return undefined;
  const root = document.documentElement;
  const style = getComputedStyle(root);
  const probe = document.createElement("span");
  probe.hidden = true;
  root.append(probe);
  try {
    const colors: Record<string, string> = {};
    for (const key of EDITOR_COLOR_KEYS) {
      const value = style.getPropertyValue(`--os-${key}`).trim();
      if (!value) return undefined;
      probe.style.color = "";
      probe.style.color = value;
      if (!probe.style.color) return undefined;
      const channels = getComputedStyle(probe).color.match(/^rgba?\(([^)]+)\)$/)?.[1]?.split(/\s*,\s*/).map(Number);
      if (!channels || channels.length < 3 || channels.some(c => !Number.isFinite(c))) return undefined;
      colors[key] = "#" + channels.map((c, i) => Math.round(i === 3 ? c * 255 : c).toString(16).padStart(2, "0")).join("");
    }
    const explicit = root.dataset.theme;
    const mode = explicit === "light" || explicit === "dark" ? explicit
      : globalThis.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark";
    return readEditorAppearance(JSON.stringify({ version: 1, mode, colors }));
  } finally { probe.remove(); }
}
