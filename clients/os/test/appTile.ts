/** Exact accessible name of an app tile in the Launcher or phone home. */
export function appTileName(app: string): RegExp {
  return new RegExp(`^${app.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`);
}
