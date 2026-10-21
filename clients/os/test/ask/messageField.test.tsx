import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks, AskOptions, AskTransport } from "../../src/ask/askController";
import { routeIdFromSource, routeOptionsFrom, routeSource, type AskRouteOption } from "../../src/ask/askRoute";
import { ConversationSession, MAX_ASK_ATTACHMENTS, type AskTurn } from "../../src/ask/conversationSession";
import { SdkAskTransport } from "../../src/ask/sdkTransport";
import { sanitizeAskSettings } from "../../src/apps/settings/askSettings";
import type { UploadHandle, UploadProvider, UploadResult } from "../../src/items/upload";

afterEach(cleanup);

const READY = { state: "ready" as const, message: "", refresh: vi.fn() };

const ROUTES: AskRouteOption[] = routeOptionsFrom({
  routes: [
    { id: "auto", name: "Auto", kind: "auto", scope: "cluster", ready: true, reason: "" },
    { id: "local", name: "Local", kind: "local", scope: "cluster", ready: true, reason: "" },
    { id: "apps", name: "Apps", kind: "apps", scope: "cluster", ready: false, reason: "No signed-in app on your machines" },
    { id: "r1", name: "Emergency", kind: "custom", scope: "personal", ready: true, reason: "" },
  ],
});

function transport(routes: AskRouteOption[] = ROUTES) {
  const sent: { prompt: string; options?: AskOptions; on: AskCallbacks }[] = [];
  const t: AskTransport = {
    ask: vi.fn((prompt: string, _context: string | null, on: AskCallbacks, options?: AskOptions) => { sent.push({ prompt, options, on }); return { cancel: vi.fn() }; }),
    routes: vi.fn(async () => routes),
  };
  return { t, sent };
}

/** An upload provider the test resolves by hand, one deferred per file. */
function uploads() {
  const pending: { file: File; resolve: (result: UploadResult) => void; reject: (error: Error) => void; progress?: (p: { sentBytes: number; totalBytes: number }) => void; aborted: boolean }[] = [];
  const provider: UploadProvider = {
    upload(file: File): UploadHandle {
      let resolve!: (result: UploadResult) => void, reject!: (error: Error) => void;
      const done = new Promise<UploadResult>((a, b) => { resolve = a; reject = b; });
      const entry: (typeof pending)[number] = { file, resolve, reject, aborted: false };
      pending.push(entry);
      return { done, abort: () => { entry.aborted = true; reject(new Error("upload aborted")); }, onProgress: listener => { entry.progress = listener; return () => {}; } };
    },
  };
  return { provider, pending };
}

function field() { return screen.getByRole("textbox", { name: "Ask" }) as HTMLTextAreaElement; }
function choose(label: string, option: string | RegExp) {
  fireEvent.click(screen.getByRole("combobox", { name: label }));
  const row = within(screen.getByRole("listbox", { name: label })).getAllByRole("option").find(row => {
    const name = row.querySelector(".os-select-option-label")?.textContent ?? "";
    return typeof option === "string" ? name === option : option.test(name);
  });
  expect(row).toBeDefined();
  fireEvent.click(row!);
}

describe("the message field", () => {
  it("starts two rows tall, with the controls in a row inside the same frame", async () => {
    const { t } = transport();
    const { provider } = uploads();
    render(<AskSurface transport={t} variant="sheet" availability={READY} uploads={provider} />);
    await act(async () => {});
    const form = field().closest("form")!;
    expect(field().getAttribute("rows")).toBe("2");
    const controls = form.querySelector(".os-ask-controls")!;
    expect(controls).not.toBeNull();
    // Below the text, never beside or above it.
    expect(field().compareDocumentPosition(controls) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const start = within(controls.querySelector(".os-ask-controls-start") as HTMLElement);
    expect(start.getByRole("button", { name: "Attach files" })).toBeTruthy();
    expect(start.getByRole("combobox", { name: "Where" })).toBeTruthy();
    expect(start.getByRole("combobox", { name: "Route" })).toBeTruthy();
    expect(form.querySelector(".os-ask-destination")).toBeNull();
  });

  it("offers no Attach where the window has no upload path", () => {
    render(<AskSurface transport={transport().t} variant="widget" availability={READY} />);
    expect(screen.queryByRole("button", { name: "Attach files" })).toBeNull();
  });

  it("grows with its text up to 40% of the surface, then scrolls inside", () => {
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} />);
    const surface = field().closest(".os-ask") as HTMLElement;
    Object.defineProperty(surface, "clientHeight", { configurable: true, value: 500 });
    let wanted = 120;
    Object.defineProperty(field(), "scrollHeight", { configurable: true, get: () => wanted });
    fireEvent.change(field(), { target: { value: "one\ntwo\nthree" } });
    expect(field().style.height).toBe("120px");
    expect(field().style.overflowY).toBe("hidden");
    wanted = 900;
    fireEvent.change(field(), { target: { value: "a very long message\n".repeat(40) } });
    expect(field().style.height).toBe("200px");
    expect(field().style.overflowY).toBe("auto");
  });

  it("keeps Send absent while there is nothing to send, holding its place", () => {
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} />);
    expect(screen.queryByRole("button", { name: "Send" })).toBeNull();
    expect(document.querySelector(".os-ask-send-slot")).not.toBeNull();
    fireEvent.change(field(), { target: { value: "hello" } });
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy();
  });

  it("uploads attached files, shows their progress, waits for them, and sends their artifact ids", async () => {
    const { t, sent } = transport();
    const { provider, pending } = uploads();
    render(<AskSurface transport={t} variant="sheet" availability={READY} uploads={provider} onOpenFile={vi.fn()} />);
    const input = field().closest("form")!.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(["# notes"], "notes.md", { type: "text/markdown" });
    fireEvent.change(input, { target: { files: [file] } });
    expect(pending).toHaveLength(1);
    expect(screen.getByRole("progressbar", { name: "Uploading notes.md" })).toBeTruthy();
    act(() => pending[0]!.progress?.({ sentBytes: 5, totalBytes: 10 }));
    expect(screen.getByRole("progressbar", { name: "Uploading notes.md" }).getAttribute("aria-valuenow")).toBe("50");

    fireEvent.change(field(), { target: { value: "Summarize this" } });
    expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { pending[0]!.resolve({ artifactId: "artifact-1", fileId: "file-1", title: "notes.md", fileKind: "file", source: "uploaded" }); });
    expect(screen.queryByRole("progressbar")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.attachments).toEqual(["artifact-1"]);
    // The field empties and the sent message names the file.
    expect(screen.queryByRole("list", { name: "Attached files" })?.closest("form")).toBeFalsy();
    expect(screen.getByRole("button", { name: "notes.md" })).toBeTruthy();
  });

  it("says a failed upload in a few words and lets it be removed", async () => {
    const { provider, pending } = uploads();
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} uploads={provider} />);
    const input = field().closest("form")!.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["x"], "big.bin")] } });
    await act(async () => { pending[0]!.reject(new Error("upload refused: 170 bytes would exceed this account's quota of 150 (MEMQL_LIBRARY_USER_QUOTA_BYTES)")); });
    const chip = screen.getByText("big.bin").closest("li")!;
    expect(chip.getAttribute("data-state")).toBe("failed");
    expect(within(chip).getByText("Over your storage quota")).toBeTruthy();
    expect(chip.getAttribute("title")).toMatch(/^big\.bin: .*quota/);
    fireEvent.click(within(chip).getByRole("button", { name: "Remove big.bin" }));
    expect(screen.queryByText("big.bin")).toBeNull();
  });

  it("stops an upload still running when its file is removed", () => {
    const { provider, pending } = uploads();
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} uploads={provider} />);
    const input = field().closest("form")!.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["x"], "draft.txt")] } });
    fireEvent.click(screen.getByRole("button", { name: "Remove draft.txt" }));
    expect(pending[0]!.aborted).toBe(true);
    expect(screen.queryByText("draft.txt")).toBeNull();
  });

  it("holds at most the files a message can carry", () => {
    const { provider, pending } = uploads();
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} uploads={provider} />);
    const input = field().closest("form")!.querySelector('input[type="file"]') as HTMLInputElement;
    const files = Array.from({ length: MAX_ASK_ATTACHMENTS + 2 }, (_, i) => new File(["x"], `f${i}.txt`));
    fireEvent.change(input, { target: { files } });
    expect(pending).toHaveLength(MAX_ASK_ATTACHMENTS);
    expect((screen.getByRole("button", { name: "Attach files" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("the route and Where menus", () => {
  it("lists Auto, Local and Apps, the person's own under Yours, and shows why a route is not ready", async () => {
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} onManageRoutes={vi.fn()} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("combobox", { name: "Route" }));
    const list = screen.getByRole("listbox", { name: "Route" });
    expect(within(list).getAllByRole("option").map(option => option.querySelector(".os-select-option-label")?.textContent)).toEqual(["Auto", "Local", "Apps", "Emergency", "Manage routes"]);
    expect(list.querySelector(".os-select-group")?.textContent).toBe("Yours");
    const apps = within(list).getByRole("option", { name: /Apps/ });
    expect(apps.getAttribute("aria-disabled")).toBe("true");
    expect(apps.textContent).toContain("No signed-in app on your machines");
  });

  it("keeps Auto choosable when the cluster says nothing can answer, and says why", async () => {
    const routes = routeOptionsFrom({ routes: [
      { id: "auto", name: "Auto", kind: "auto", scope: "cluster", ready: false, reason: "Needs an embedding model" },
      { id: "local", name: "Local", kind: "local", scope: "cluster", ready: false, reason: "Needs an embedding model" },
    ] });
    const { t, sent } = transport(routes);
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("combobox", { name: "Route" }));
    const auto = within(screen.getByRole("listbox", { name: "Route" })).getByRole("option", { name: /Auto/ });
    expect(auto.getAttribute("aria-disabled")).not.toBe("true");
    expect(auto.textContent).toContain("Needs an embedding model");
    fireEvent.click(auto);
    fireEvent.change(field(), { target: { value: "go" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "", level: "" });
  });

  it("sends the chosen route and Where with the message", async () => {
    const { t, sent } = transport();
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    choose("Route", "Emergency");
    choose("Where", "Workbench");
    fireEvent.change(field(), { target: { value: "go" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "route:r1", level: "", destination: "workbench" });
  });

  it("sends nothing extra on Auto, as before", async () => {
    const { t, sent } = transport();
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    fireEvent.change(field(), { target: { value: "go" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "", level: "" });
  });

  it.each(["sheet", "widget"] as const)("keeps Auto as the default when Apps is chosen in the %s", async variant => {
    const ready = ROUTES.map(route => route.id === "apps" ? { ...route, ready: true, reason: "" } : route);
    const { t, sent } = transport(ready);
    render(<AskSurface transport={t} variant={variant} availability={READY} />);
    await act(async () => {});
    choose("Route", "Apps");
    expect(screen.getByRole("combobox", { name: "Where" }).textContent).toBe("Auto");
    fireEvent.change(field(), { target: { value: "go" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "route:apps", level: "" });
  });

  it("preserves an explicit Computer choice when changing routes", async () => {
    const { t, sent } = transport(ROUTES.map(route => ({ ...route, ready: true, reason: "" })));
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    choose("Where", "Computer");
    for (const route of ["Apps", "Local", "Auto", "Apps"]) {
      choose("Route", route);
      expect(screen.getByRole("combobox", { name: "Where" }).textContent).toBe("Computer");
    }
    fireEvent.change(field(), { target: { value: "go" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "route:apps", level: "", destination: "computer" });
  });

  it("disables Apps with Workbench, preserves the draft, and keeps its description stable", async () => {
    const { t, sent } = transport(ROUTES.map(route => ({ ...route, ready: true, reason: "" })));
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    const routeMenu = screen.getByRole("combobox", { name: "Route" });
    fireEvent.click(routeMenu);
    const description = screen.getByRole("option", { name: /^Apps / }).textContent;
    fireEvent.keyDown(routeMenu, { key: "Escape" });
    choose("Where", "Workbench");
    fireEvent.change(field(), { target: { value: "keep this draft" } });
    fireEvent.click(routeMenu);
    const apps = screen.getByRole("option", { name: /^Apps / });
    expect(apps.textContent).toBe(description);
    expect(apps.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(apps);
    expect(routeMenu.textContent).toBe("Auto");
    expect(screen.getByRole("combobox", { name: "Where" }).textContent).toBe("Workbench");
    expect(field().value).toBe("keep this draft");
    expect(screen.queryByRole("alert")).toBeNull();
    // Keyboard navigation skips the disabled Apps row, too.
    fireEvent.keyDown(routeMenu, { key: "ArrowDown" });
    fireEvent.keyDown(routeMenu, { key: "ArrowDown" });
    fireEvent.keyDown(routeMenu, { key: "Enter" });
    expect(routeMenu.textContent).toBe("Emergency");
    expect(document.activeElement).toBe(routeMenu);
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "route:r1", level: "", destination: "workbench" });
  });

  it("disables Workbench with Apps while leaving Auto and Computer selectable", async () => {
    render(<AskSurface transport={transport(ROUTES.map(route => ({ ...route, ready: true, reason: "" }))).t} variant="widget" availability={READY} />);
    await act(async () => {});
    const where = screen.getByRole("combobox", { name: "Where" });
    fireEvent.click(where);
    const description = screen.getByRole("option", { name: /^Workbench / }).textContent;
    fireEvent.keyDown(where, { key: "Escape" });
    choose("Route", "Apps");
    fireEvent.click(where);
    const workbench = screen.getByRole("option", { name: /^Workbench / });
    expect(workbench.textContent).toBe(description);
    expect(workbench.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(workbench);
    expect(where.textContent).toBe("Auto");
    fireEvent.keyDown(where, { key: "ArrowDown" });
    fireEvent.keyDown(where, { key: "Enter" });
    expect(where.textContent).toBe("Computer");
    choose("Where", "Auto");
    expect(screen.getByRole("combobox", { name: "Route" }).textContent).toBe("Apps");
    choose("Route", "Local");
    choose("Where", "Workbench");
    expect(where.textContent).toBe("Workbench");
  });

  it("keeps an older incompatible choice visible and blocks submission until a control changes", async () => {
    const { t, sent } = transport(ROUTES.map(route => ({ ...route, ready: true, reason: "" })));
    const session = new ConversationSession(t);
    session.setRoutingDefault({ route: "apps", destination: "workbench" });
    session.setDraft("keep this draft");
    render(<AskSurface conversation={session} transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    expect(screen.getByRole("combobox", { name: "Where" }).textContent).toBe("Workbench");
    expect(screen.getByRole("combobox", { name: "Route" }).textContent).toBe("Apps");
    expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.keyDown(field(), { key: "Enter" });
    expect(session.send("also blocked", null)).toBe(false);
    expect(sent).toHaveLength(0);
    expect(field().value).toBe("keep this draft");
    expect(session.getSnapshot().error).toBe("");
    choose("Route", "Auto");
    expect(screen.getByRole("combobox", { name: "Where" }).textContent).toBe("Workbench");
    fireEvent.keyDown(field(), { key: "Enter" });
    await act(async () => {});
    expect(sent[0]?.options?.routing).toEqual({ source: "", level: "", destination: "workbench" });
  });

  it("opens Fleet's routes from Manage routes without changing the choice", async () => {
    const manage = vi.fn();
    render(<AskSurface transport={transport().t} variant="sheet" availability={READY} onManageRoutes={manage} />);
    await act(async () => {});
    choose("Route", "Manage routes");
    expect(manage).toHaveBeenCalledOnce();
    expect(screen.getByRole("combobox", { name: "Route" }).textContent).toBe("Auto");
  });

  it("offers Auto alone when the cluster has no routes", async () => {
    const t: AskTransport = { ask: vi.fn(() => ({ cancel: vi.fn() })), routes: vi.fn(async () => { throw new Error("unknown builtin routesForActor"); }) };
    render(<AskSurface transport={t} variant="sheet" availability={READY} />);
    await act(async () => {});
    fireEvent.click(screen.getByRole("combobox", { name: "Route" }));
    expect(within(screen.getByRole("listbox", { name: "Route" })).getAllByRole("option").map(option => option.querySelector(".os-select-option-label")?.textContent)).toEqual(["Auto"]);
  });
});

describe("keeping the choice", () => {
  const turn = (route?: AskTurn["route"]): AskTurn => ({ id: "t", prompt: "p", answer: "a", state: "done", startedAt: new Date().toISOString(), activity: [], ...(route ? { route } : {}) });
  function sessionWith(turns: AskTurn[]) {
    return new ConversationSession({
      ask: vi.fn(() => ({ cancel: vi.fn() })),
      conversations: { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "new", title: "New conversation" })), read: vi.fn(async () => turns) },
    });
  }

  it("rejects incompatible edits at the shared session boundary without replacing either choice", () => {
    const session = sessionWith([]);
    session.setDestination("workbench");
    session.setRoute("apps");
    expect(session.getSnapshot().destination).toBe("workbench");
    expect(session.getSnapshot().route ?? "").toBe("");
    session.setDestination("auto");
    session.setRoute("apps");
    session.setDestination("workbench");
    expect(session.getSnapshot().destination).toBe("auto");
    expect(session.getSnapshot().route).toBe("apps");
  });

  it("opens a conversation on what its last message ran with", async () => {
    const session = sessionWith([turn(), turn({ source: "route:local", destination: "workbench" })]);
    session.setRoutingDefault({ route: "r1", destination: "computer" });
    await session.select("c1");
    expect(session.getSnapshot().route).toBe("local");
    expect(session.getSnapshot().destination).toBe("workbench");
  });

  it("reads a conversation whose last message ran on Auto as Auto", async () => {
    const session = sessionWith([turn({ source: "route:local" }), turn()]);
    session.setRoutingDefault({ route: "r1", destination: "computer" });
    await session.select("c1");
    expect(session.getSnapshot().route).toBe("");
    expect(session.getSnapshot().destination).toBe("auto");
  });

  it("starts a new conversation from the person's last choice, and reports every change", () => {
    const session = sessionWith([]);
    const heard = vi.fn();
    session.setRoutingDefault({ route: "r1", destination: "computer" });
    expect(session.getSnapshot().route).toBe("r1");
    session.onRoutingChoice(heard);
    session.setRoute("local");
    expect(heard).toHaveBeenLastCalledWith({ route: "local", destination: "computer" });
    session.setDestination("workbench");
    expect(heard).toHaveBeenLastCalledWith({ route: "local", destination: "workbench" });
    session.newConversation();
    expect(session.getSnapshot().route).toBe("r1");
  });

  it("remembers the choice in Ask's own settings, dropping anything the cluster would refuse", () => {
    expect(sanitizeAskSettings({ version: 1, commit: "review", spaceToTalk: true, route: "r1", destination: "computer" })).toMatchObject({ route: "r1", destination: "computer" });
    const dropped = sanitizeAskSettings({ version: 1, commit: "review", spaceToTalk: true, route: "route:has spaces", destination: "moon" });
    expect(dropped.route).toBeUndefined();
    expect(dropped.destination).toBeUndefined();
  });
});

describe("the route words", () => {
  it("travels as route:<id>, with Auto as nothing", () => {
    expect(routeSource("")).toBe("");
    expect(routeSource("auto")).toBe("");
    expect(routeSource("apps")).toBe("route:apps");
    expect(routeIdFromSource("route:apps")).toBe("apps");
    expect(routeIdFromSource("route:auto")).toBe("");
    expect(routeIdFromSource("app:claude-code")).toBe("");
    expect(routeIdFromSource(undefined)).toBe("");
  });

  it("keeps Auto first and always present, the cluster's routes next and the person's after", () => {
    const options = routeOptionsFrom({ routes: [
      { id: "r1", name: "Mine", kind: "custom", scope: "personal", ready: true },
      { id: "local", name: "Local", kind: "local", scope: "cluster", ready: false },
      { id: "bad", name: "Bad", kind: "made-up", scope: "cluster", ready: true },
    ] });
    expect(options.map(option => option.id)).toEqual(["auto", "local", "r1"]);
    expect(options.find(option => option.id === "local")?.reason).toBe("Not ready");
  });
});

describe("the SDK transport", () => {
  it("sends attached artifact ids beside the route, and only when there are any", async () => {
    const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
    const transportFor = () => new SdkAskTransport(() => ({} as Dispatcher), stream);
    await new Promise<void>((done, error) => transportFor().ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t1", routing: { source: "route:apps", level: "" }, attachments: ["artifact-1"] }));
    expect(stream).toHaveBeenLastCalledWith(expect.anything(), expect.anything(), expect.objectContaining({ provider: "route:apps", attachmentArtifactIds: ["artifact-1"] }));
    await new Promise<void>((done, error) => transportFor().ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t2" }));
    const opts = (stream.mock.calls[1] as unknown as [unknown, unknown, Record<string, unknown>])[2];
    expect(Object.hasOwn(opts, "attachmentArtifactIds")).toBe(false);
  });

  it("reads the routes through the generated routesForActor builtin", async () => {
    // The cluster answers one virtual row, {routes, revision}, in the builtin node envelope.
    const payload = { revision: 4, routes: [
      { id: "auto", name: "Auto", kind: "auto", scope: "cluster", description: "", ready: true, reason: "", customized: false, jobs: [] },
      { id: "apps", name: "Apps", kind: "apps", scope: "cluster", description: "Claude Code or Codex", ready: false, reason: "No signed-in app on your machines", customized: false, jobs: [] },
      { id: "r9", name: "Night", kind: "custom", scope: "personal", description: "", ready: true, reason: "", customized: false, jobs: [] },
    ] };
    const sendAndWait = vi.fn(async (_request: unknown) => ({ queryResult: { result: { data: [{ routes: { id: "routes", concept: "v1:router:routeCatalog", type: "object", createdAt: "2026-10-18T12:00:00Z", payload } }] } } }));
    const routes = await new SdkAskTransport(() => ({ sendAndWait } as unknown as Dispatcher)).routes!();
    expect(sendAndWait).toHaveBeenCalledWith(expect.objectContaining({ executeQuery: expect.objectContaining({ query: "builtin routesForActor()" }) }), undefined);
    expect(routes.map(route => [route.id, route.ready, route.reason])).toEqual([["auto", true, ""], ["apps", false, "No signed-in app on your machines"], ["r9", true, ""]]);
  });
});
