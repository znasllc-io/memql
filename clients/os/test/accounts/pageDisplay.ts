import { readFileSync } from "node:fs";
import { join } from "node:path";

/** Apply the actual display rules involved in retaining an account page.
 * jsdom does not load Vite CSS imports, and role queries treat `hidden` as
 * hidden even when an author display rule makes the element visible. Reading
 * the real rules lets the navigation tests exercise that cascade as well. */
export function installAccountPageDisplayRules(): () => void {
  const css = readFileSync(join(__dirname, "../../src/styles/index.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
  const selectors = new Set([".os-app-stack", ".os-account-page[hidden]"]);
  const rules = [...css.matchAll(/([^{}]+)\{[^{}]*\}/g)]
    .filter((rule) => rule[1]!.split(",").some((selector) => selectors.has(selector.trim())))
    .map((rule) => rule[0]);
  const style = document.createElement("style");
  style.textContent = rules.join("\n");
  document.head.append(style);
  return () => style.remove();
}
