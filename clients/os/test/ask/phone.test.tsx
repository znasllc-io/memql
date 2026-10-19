import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskProvider, useAsk } from "../../src/ask/AskProvider";
import { PhoneShell } from "../../src/chrome/PhoneShell";
import { OsProvider } from "../../src/chrome/state";
import { OS_REGISTRY } from "../../src/apps/registry";
import { READY_ASK } from "../../src/ask/useAskReadiness";
import type { AskTurn } from "../../src/ask/conversationSession";
import { withSession } from "../cluster/harness";
import { coreAt } from "../setup/harness";
import { installSeededAccess } from "../seededAccess";

afterEach(cleanup);

it("keeps the phone header reachable and opens paused work in the phone app with its exact intent", async () => {
  installSeededAccess("owner");
  let ask!: ReturnType<typeof useAsk>;
  function Observe() { ask = useAsk(); return null; }
  const nexus = vi.fn(({ intent }: { intent?: { payload: Record<string, unknown> } }) => <p>Selected run: {String(intent?.payload.runId ?? "none")}</p>);
  const registry = { ...OS_REGISTRY, apps: OS_REGISTRY.apps.map(app => app.id === "nexus" ? {...app, component:nexus} : app) };
  const turn: AskTurn = { id:"turn", runId:"paused-run", goalId:"goal", workTitle:"Create the dog image", prompt:"Make a dog image", answer:"I’ll create it.", acknowledgement:"I’ll create it.", background:true, state:"waiting", startedAt:"2026-10-18T13:00:00Z", activity:[] };
  const transport = { ask:vi.fn(() => ({cancel(){}})), conversations:{list:async()=>[], create:async()=>({id:"chat",title:"Chat"}), read:async()=>[turn]} };
  const signOut = vi.fn();
  render(withSession(<OsProvider registry={registry} actorRole="owner" grid={{cols:12,rows:8}} layout="phone"><AskProvider transport={transport} availability={READY_ASK}><Observe /><div className="os-root" data-layout="phone"><PhoneShell onSignOut={signOut}/></div></AskProvider></OsProvider>, {readiness:coreAt("configured","configured","configured")}));
  await act(async () => { await ask.conversation.select("chat"); });
  fireEvent.click(screen.getByRole("button", {name:"Ask"}));
  const sheet = screen.getByRole("dialog", {name:"Ask"});
  const content = document.querySelector(".os-phone-content")!;
  expect(content.contains(sheet)).toBe(true);
  expect(content.contains(screen.getByRole("button", {name:"Sign out"}))).toBe(false);
  expect(document.querySelector(".os-phone-page")?.hasAttribute("inert")).toBe(true);
  expect(screen.queryByRole("button", {name:/Maximize Ask|Restore Ask/})).toBeNull();
  expect(document.querySelector(".os-ask-wait")).toBeNull();
  fireEvent.change(within(sheet).getByRole("textbox", {name:"Ask"}), {target:{value:"Keep my unsent message"}});
  fireEvent.click(within(sheet).getByRole("button", {name:"Create the dog image Needs your input"}));
  expect(screen.queryByRole("dialog", {name:"Ask"})).toBeNull();
  expect(screen.getByText("Selected run: paused-run")).toBeTruthy();
  expect(nexus.mock.calls.at(-1)?.[0]).toMatchObject({sectionId:"runs",intent:{payload:{runId:"paused-run",conversationId:"chat"}}});
  fireEvent.click(screen.getByRole("button", {name:"Ask"}));
  expect(screen.getByRole("textbox", {name:"Ask"})).toHaveProperty("value","Keep my unsent message");
  fireEvent.click(screen.getByRole("button", {name:"Close Ask"}));
  expect(document.querySelector(".os-phone-page")?.hasAttribute("inert")).toBe(false);
  expect(transport.ask).not.toHaveBeenCalled(); expect(signOut).not.toHaveBeenCalled();
});
