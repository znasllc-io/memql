// Appearance is handoff metadata, never part of a file's identity. Carry only
// color values: no CSS, resource URLs, commands, credentials or user content.
export const EDITOR_COLOR_KEYS = ["ground", "plate", "raised", "ink", "muted", "line", "accent", "accent-fg", "accent-soft", "warn", "error"] as const;
export type EditorColorKey = typeof EDITOR_COLOR_KEYS[number];
export interface EditorAppearance {
  version: 1;
  mode: "dark" | "light";
  colors: Record<EditorColorKey, string>;
}

export function readEditorAppearance(value: string | null | undefined): EditorAppearance | undefined {
  if (!value || value.length > 1500) return undefined;
  try {
    const input = JSON.parse(value);
    if (input?.version !== 1 || !["dark", "light"].includes(input.mode)) return undefined;
    const colors = {} as EditorAppearance["colors"];
    for (const key of EDITOR_COLOR_KEYS) {
      const color = input.colors?.[key];
      if (typeof color !== "string" || !/^#[\da-f]{6}([\da-f]{2})?$/i.test(color)) return undefined;
      colors[key] = color;
    }
    return { version: 1, mode: input.mode, colors };
  } catch { return undefined; }
}
