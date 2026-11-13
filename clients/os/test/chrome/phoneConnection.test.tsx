import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { AskProvider } from "../../src/ask/AskProvider";
import { READY_ASK } from "../../src/ask/useAskReadiness";
import { OS_REGISTRY } from "../../src/apps/registry";
import { ConnectionRecoveryContext, ConnectionStatusContext } from "../../src/chrome/connection";
import { PhoneShell } from "../../src/chrome/PhoneShell";
import { OsProvider } from "../../src/chrome/state";
import { withSession } from "../cluster/harness";
import { installSeededAccess } from "../seededAccess";
import { coreAt } from "../setup/harness";

// A DROPPED CONNECTION MOVED EVERY PAGE ON A PHONE. "Reconnecting…" had a line of its
// own in the header, so two seconds after a drop everything under the header moved
// 24.5 px down, and thirty seconds after it Retry moved it 15.5 px further, then back
// up on recovery (measured at 375x812 with the server killed, on 15 pages; on a
// machine's Equipment page 15 of the 19 controls a tap could reach were no longer under
// their old centre). On the header's own line nothing moves. jsdom lays nothing out, so
// this holds the two halves the browser combines: the notice is on that line, and the
// stylesheet gives the line no way to grow for it.

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");
const rule = (selector: string) => {
  const at = CSS.indexOf(`\n${selector} {`);
  expect(at, `the stylesheet has no rule for ${selector}`).toBeGreaterThan(-1);
  return CSS.slice(at, CSS.indexOf("}", at));
};

const registry = { ...OS_REGISTRY, apps: OS_REGISTRY.apps.map(app => app.id === "nexus" ? { ...app, component: () => <p>A page</p> } : app) };
const transport = { ask: vi.fn(() => ({ cancel() {} })), conversations: { list: async () => [], create: async () => ({ id: "chat", title: "Chat" }), read: async () => [] } };

async function dropped(retry = vi.fn()) {
  vi.useFakeTimers();
  installSeededAccess("owner");
  render(withSession(<ConnectionRecoveryContext.Provider value={{ managed: true, retry }}><ConnectionStatusContext.Provider value="reconnecting"><OsProvider registry={registry} actorRole="owner" grid={{ cols: 12, rows: 8 }} layout="phone"><AskProvider transport={transport} availability={READY_ASK}><div className="os-root" data-layout="phone"><PhoneShell onSignOut={vi.fn()} /></div></AskProvider></OsProvider></ConnectionStatusContext.Provider></ConnectionRecoveryContext.Provider>, { readiness: coreAt("configured", "configured", "configured") }));
  await act(() => vi.advanceTimersByTimeAsync(2_000));
  return retry;
}
const notice = () => screen.getByRole("status").closest(".os-connection-notice")!;
const title = () => document.querySelector(".os-phone-title")!;
const profile = () => screen.getByRole("button", { name: "Open Profile" });
const onTheLine = () => {
  expect(notice().parentElement).toBe(document.querySelector(".os-phone-bar"));
  expect(notice().previousElementSibling).toBe(title());
  expect(notice().nextElementSibling).toBe(profile());
};

afterEach(() => { cleanup(); vi.useRealTimers(); });

it("says it is reconnecting on the header's own line, between the title and Profile", async () => {
  await dropped();
  expect(screen.getByRole("status").textContent).toBe("Reconnecting…");
  onTheLine();
  fireEvent.click(screen.getByRole("button", { name: "Nexus" }));
  expect(screen.getByRole("button", { name: "Back to home" }).parentElement).toBe(notice().parentElement);
  onTheLine();
});

it("offers Retry on that same line, under the name it has everywhere", async () => {
  const retry = await dropped();
  await act(() => vi.advanceTimersByTimeAsync(28_000));
  expect(screen.getByRole("status").textContent).toBe("Can’t reconnect");
  onTheLine();
  // The whole name does not fit beside the status and a title at 375 wide.
  const button = screen.getByRole("button", { name: "Retry connection" });
  expect(button.textContent).toBe("Retry");
  expect(button.parentElement).toBe(notice());
  fireEvent.click(button);
  expect(retry).toHaveBeenCalledOnce();
});

it("gives the header's line no way to grow, with the title giving way first", () => {
  expect(rule(".os-phone-bar")).not.toContain("flex-wrap");
  expect(rule(".os-phone-connection")).not.toContain("flex-basis");
  // One line whatever it is told, grown only from the room the notice leaves. A shrink
  // ratio cannot do this: at 320 wide it cost the notice 0.2 px and read "Can't reconne...".
  const heading = rule(".os-phone-title");
  for (const declaration of ["flex: 1 1 0;", "min-width: 0;", "max-width: max-content;", "overflow: hidden;", "white-space: nowrap;", "text-overflow: ellipsis;"]) expect(heading).toContain(declaration);
  expect(rule(".os-phone-connection")).toContain("flex: 0 1 auto;");
  expect(rule(".os-phone-connection")).toContain("min-width: 0;");
  // Nothing that does not give way can wrap or be squeezed.
  expect(rule(".os-phone-mark, .os-phone-bar > .os-icon-button, .os-phone-bar > .os-avatar-button")).toContain("flex: none;");
  // Profile keeps its edge, with the same protected gap from Retry.
  expect(rule(".os-phone-bar > .os-avatar-button")).toContain("margin-inline-start: auto;");
  expect(rule(".os-phone-connection")).toContain("margin-inline-end: 10px;");
  // A 30 px control in a 20 px line: its margins leave it 20 px of the line's height.
  expect(CSS).toContain("--os-control-h");
  expect(rule(".os-phone-connection .os-button")).toContain("margin-block: calc((20px - var(--os-control-h)) / 2);");
});
