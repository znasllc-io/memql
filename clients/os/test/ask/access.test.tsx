import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { AskProvider, useAsk, useDiscussApproval, useOpenAskWork } from "../../src/ask/AskProvider";
import { AskSheet } from "../../src/ask/AskSheet";
import { READY_ASK } from "../../src/ask/useAskReadiness";
import { Shell } from "../../src/chrome/Shell";
import { SessionProvider } from "../../src/chrome/access";
import { useWindowAsk } from "../../src/chrome/windowAsk";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { clearEffectiveCapabilities, effectiveAccessEpoch, setEffectiveCapabilities } from "../../src/system/roles";
import { LocalDesktopStore } from "../../src/system/store";
import { installSeededAccess, seededAccessFor } from "../seededAccess";
import { withOs } from "../setup/harness";
import { appTileName } from "../appTile";

afterEach(() => { cleanup(); installSeededAccess("owner"); });

const person = { userId: "v1:identity:user:moderator", primaryEmail: "moderator@example.test", role: "fylo-moderator", roleName: "Moderator", rank: 20 };
const withoutAsk = () => setEffectiveCapabilities(seededAccessFor("owner").filter(cap => cap.resource !== "app:ask"));
function transportFixture() {
  return {
    ask: vi.fn(() => ({ cancel: vi.fn() })),
    startVoice: vi.fn(),
    conversations: {
      list: vi.fn(async () => []),
      create: vi.fn(async () => ({ id: "private-chat", title: "Private chat" })),
      read: vi.fn(async () => []),
    },
  };
}

it.each(["not granted", "denied", "not loaded"])("refuses commands, app callbacks, work/discussion links and the sheet when Ask is %s", async state => {
  if (state === "not loaded") clearEffectiveCapabilities();
  else setEffectiveCapabilities(state === "denied" ? [{ verb: "read", resource: "app:ask", effect: "deny", source: "user" }] : []);
  const transport = transportFixture();
  let ask!: ReturnType<typeof useAsk>;
  function Commands() {
    ask = useAsk();
    const windowAsk = useWindowAsk("app:deployables");
    const work = useOpenAskWork(), discuss = useDiscussApproval();
    expect(work).toBeNull(); expect(discuss).toBeNull();
    return <><button onClick={() => ask.openAsk()}>Command</button><button onClick={() => windowAsk.askAbout("Reviews")}>App command</button></>;
  }
  render(withOs(<SessionProvider value={{ access: person, config: UNKNOWN_RUNTIME_CONFIG }}><AskProvider transport={transport} availability={READY_ASK}><Commands /><AskSheet /></AskProvider></SessionProvider>, "owner"));
  fireEvent.click(screen.getByRole("button", { name: "Command" }));
  fireEvent.keyDown(screen.getByRole("button", { name: "App command" }), { key: "Enter" });
  fireEvent.click(screen.getByRole("button", { name: "App command" }));
  await act(async () => { window.dispatchEvent(new Event("focus")); });
  expect(ask.canAsk).toBe(false); expect(ask.sheet.open).toBe(false);
  expect(screen.queryByRole("dialog", { name: "Ask" })).toBeNull();
  expect(transport.ask).not.toHaveBeenCalled(); expect(transport.startVoice).not.toHaveBeenCalled();
  for (const read of Object.values(transport.conversations)) expect(read).not.toHaveBeenCalled();
});

it("closes an open sheet on revocation, cancels its local stream and resumes only with a fresh viewer", async () => {
  installSeededAccess("owner");
  const transport = transportFixture();
  let ask!: ReturnType<typeof useAsk>;
  let work!: ReturnType<typeof useOpenAskWork>, discuss!: ReturnType<typeof useDiscussApproval>;
  function Observe() { ask = useAsk(); work = useOpenAskWork(); discuss = useDiscussApproval(); return null; }
  const tree = () => withOs(<SessionProvider value={{ access: person, config: UNKNOWN_RUNTIME_CONFIG, accessEpoch: effectiveAccessEpoch() }}><AskProvider transport={transport} availability={READY_ASK}><Observe /><AskSheet /></AskProvider></SessionProvider>, "owner");
  const view = render(tree());
  act(() => ask.openAsk());
  expect(screen.getByRole("dialog", { name: "Ask" })).toBeTruthy();
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Private question" } });
  fireEvent.click(screen.getByRole("button", { name: "Send" }));
  await waitFor(() => expect(transport.ask).toHaveBeenCalledOnce());
  const cancel = transport.ask.mock.results[0]!.value.cancel;
  const prior = ask, oldWork = work, oldDiscuss = discuss;
  const openWork = vi.spyOn(prior.conversation, "openWork"), discussApproval = vi.spyOn(prior.conversation, "discussApproval");
  withoutAsk();
  // Even callbacks retained by a command palette cannot start new work in
  // the interval before React consumes the session's new access epoch.
  act(() => { prior.closeAsk(); prior.openAsk(); oldWork?.("run"); oldDiscuss?.("approval"); });
  expect(openWork).not.toHaveBeenCalled(); expect(discussApproval).not.toHaveBeenCalled();
  view.rerender(tree());
  expect(screen.queryByRole("dialog", { name: "Ask" })).toBeNull();
  expect(cancel).toHaveBeenCalledOnce(); expect(ask.sheet.open).toBe(false);
  expect(ask.conversation).not.toBe(prior.conversation);
  expect(ask.conversation.getSnapshot().draft).toBe("");
  const reads = transport.conversations.list.mock.calls.length;
  await act(async () => { window.dispatchEvent(new Event("pageshow")); });
  expect(transport.conversations.list).toHaveBeenCalledTimes(reads);
  installSeededAccess("owner"); view.rerender(tree());
  expect(ask.canAsk).toBe(true); expect(ask.sheet.open).toBe(false);
  expect(ask.conversation.getSnapshot().selectedId).toBeNull();
  act(() => ask.openAsk());
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "");
});

it("aborts a connecting live voice session when Ask access is revoked", async () => {
  installSeededAccess("owner");
  const transport = transportFixture();
  transport.startVoice.mockImplementation(() => new Promise(() => {}));
  let ask!: ReturnType<typeof useAsk>;
  function Observe() { ask = useAsk(); return null; }
  const tree = () => <SessionProvider value={{ access: person, config: UNKNOWN_RUNTIME_CONFIG, accessEpoch: effectiveAccessEpoch() }}><AskProvider transport={transport} availability={READY_ASK}><Observe /></AskProvider></SessionProvider>;
  const view = render(tree());
  act(() => { void ask.liveVoice.start(null, "female"); });
  await waitFor(() => expect(transport.startVoice).toHaveBeenCalledOnce());
  const signal = transport.startVoice.mock.calls[0]![1] as AbortSignal;
  const oldVoice = ask.liveVoice;
  withoutAsk(); view.rerender(tree());
  expect(signal.aborted).toBe(true);
  expect(oldVoice.getSnapshot().phase).toBe("off");
  expect(ask.liveVoice.getSnapshot().phase).toBe("off");
});

it.each(["desktop", "phone"] as const)("hides Ask from %s chrome and keeps Profile/sign-out reachable", async layout => {
  withoutAsk();
  const transport = transportFixture(), signOut = vi.fn();
  const data = new Map<string, string>();
  const storage = { getItem: (key: string) => data.get(key) ?? null, setItem: (key: string, value: string) => { data.set(key, value); } };
  render(<Shell layout={layout} onSignOut={signOut} access={{ ...person, role: "owner" }} config={UNKNOWN_RUNTIME_CONFIG} ports={{ store: new LocalDesktopStore(storage), disableConnection: true, askTransport: transport, askVoice: null }} />);
  expect(screen.queryByRole("button", { name: "Ask" })).toBeNull();
  expect(document.querySelector("[data-os-widget='ask']")).toBeNull();
  if (layout === "desktop") {
    fireEvent.keyDown(window, { key: " ", code: "Space", ctrlKey: true, shiftKey: true });
    if (!screen.queryByRole("dialog", { name: "Launcher" })) fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name: appTileName("Files") }));
    expect(screen.queryByRole("button", { name: "Ask about Files" })).toBeNull();
    expect(screen.getByRole("button", { name: "Search Files" })).toBeTruthy();
  }
  expect(screen.queryByRole("dialog", { name: "Ask" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
  fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
  expect(signOut).toHaveBeenCalledOnce();
  expect(transport.ask).not.toHaveBeenCalled();
  expect(transport.conversations.list).not.toHaveBeenCalled();
});
