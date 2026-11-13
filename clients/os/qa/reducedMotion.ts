/** A page-local visual fixture. It leaves the OS/browser preference untouched.
 * Copy the actual reduced-motion CSS blocks instead of maintaining a second
 * set of production animation declarations in the fixture. */
export function applyReducedMotionFixture() {
  const condition = "(prefers-reduced-motion: reduce)";
  const matchMedia = window.matchMedia.bind(window);
  window.matchMedia = query => {
    const result = matchMedia(query);
    if (query === condition) Object.defineProperty(result, "matches", { value: true });
    return result;
  };
  const style = document.createElement("style");
  style.dataset.qaReducedMotion = "true";
  style.textContent = Array.from(document.styleSheets).flatMap(sheet => {
    try {
      return Array.from(sheet.cssRules).flatMap(rule => rule instanceof CSSMediaRule && rule.conditionText === condition
        ? Array.from(rule.cssRules).map(child => child.cssText) : []);
    } catch { return []; }
  }).join("\n");
  document.head.append(style);
}
