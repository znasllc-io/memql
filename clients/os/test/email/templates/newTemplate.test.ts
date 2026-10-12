import { afterEach, describe, expect, it, vi } from "vitest";

import { arrivedTemplate, beginTemplateWait, openNewEmail, waitWords } from "../../../src/apps/email/templates/newTemplate";
import type { TemplateRow } from "../../../src/apps/email/rows";
import { saveEditorPreference } from "../../../src/items/editorPreference";
import { VSCODE_HANDOFF_TIMEOUT_MS, type HandoffPorts } from "../../../src/items/vscode";

// THE WAIT FOR A NEW TEMPLATE (memql#6033, Ruling 9; Review Focus 1).
//
// "Create new template" opens the editor's New email page, and the template
// exists only once the person publishes the email there -- on another node,
// delivered by the live templates feed as a draft first and then as the
// published row. The wait is a pure model over that feed, so everything it
// promises is asserted here without a DOM: what counts as new, what does not
// (a template republished meanwhile, another organization's, an archived one),
// and what the bar says while it waits.

function template(over: Partial<TemplateRow> & { id: string }): TemplateRow {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "August copy",
    subject: "What we shipped",
    textBody: "",
    htmlBody: "",
    status: "ready",
    accountId: "self",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}

const THERE = [
  template({ id: "t1" }),
  template({ id: "t2", name: "Autumn draft", status: "draft" }),
  template({ id: "t3", name: "Old news", status: "archived" }),
  template({ id: "t4", name: "Globex welcome", accountId: "globex" }),
];

describe("beginning to wait", () => {
  it("remembers the organization, what was already there and when it began", () => {
    const wait = beginTemplateWait(THERE, "self", true, 1_000);
    expect(wait.accountId).toBe("self");
    expect(wait.startedAt).toBe(1_000);
    expect(wait.requirePublished).toBe(true);
    // The organization's templates in every state, archived and draft included:
    // none of them is new, whatever happens to it next.
    expect([...wait.known]).toEqual(expect.arrayContaining(["t1", "t2", "t3"]));
  });

  it("keeps what it saw, not the list it was handed", () => {
    const list = [template({ id: "t1" })];
    const wait = beginTemplateWait(list, "self", false, 0);
    list.push(template({ id: "t9" }));
    expect(wait.known.has("t9")).toBe(false);
  });
});

describe("the template that arrives", () => {
  it("ignores templates that existed when the wait began", () => {
    const wait = beginTemplateWait(THERE, "self", false, 0);
    expect(arrivedTemplate(wait, THERE)).toBeNull();
    expect(arrivedTemplate(wait, [])).toBeNull();
  });

  it("ignores other organizations and archived templates", () => {
    // Published ones, so that a wait which needs publication is not what excludes them.
    for (const requirePublished of [false, true]) {
      const wait = beginTemplateWait(THERE, "self", requirePublished, 0);
      const later = [
        ...THERE,
        template({ id: "t5", name: "Globex autumn", accountId: "globex" }),
        template({ id: "t6", name: "Put away at once", status: "archived" }),
      ];
      expect(arrivedTemplate(wait, later)).toBeNull();
      // The one that is new, in the right organization, is still found among them.
      const welcome = template({ id: "t7", name: "Welcome 3" });
      expect(arrivedTemplate(wait, [...later, welcome])).toEqual({ template: welcome, published: true });
    }
  });

  it("reports a draft arrival and waits for publication when asked to", () => {
    const wait = beginTemplateWait(THERE, "self", true, 0);
    const draft = template({ id: "t7", name: "Welcome 3", status: "draft" });
    // A draft does not end a wait that needs the template published...
    expect(arrivedTemplate(wait, [...THERE, draft])).toBeNull();
    // ...but it has arrived, and the same question asked of a wait that takes
    // whatever arrives says so. That is what the bar names.
    expect(arrivedTemplate({ ...wait, requirePublished: false }, [...THERE, draft])).toEqual({ template: draft, published: false });

    // The same id, a moment later: the published row replaces the draft.
    const published = { ...draft, status: "ready", createdAt: "2026-10-11T09:00:00Z" };
    expect(arrivedTemplate(wait, [...THERE, published])).toEqual({ template: published, published: true });
  });

  it("takes a draft when the wait does not need the template published", () => {
    const wait = beginTemplateWait(THERE, "self", false, 0);
    const draft = template({ id: "t7", name: "Welcome 3", status: "draft" });
    expect(arrivedTemplate(wait, [...THERE, draft])).toEqual({ template: draft, published: false });
  });

  // THE FINDER IS THE WHOLE ANSWER: it returns the first new template that ENDS
  // the wait, so a caller asks one question -- did something arrive -- and a
  // draft that comes first, one the person never publishes, does not hide a
  // published template that comes later.
  it("a draft that comes first does not hide a published template that comes later", () => {
    const wait = beginTemplateWait(THERE, "self", true, 0);
    const stuck = template({ id: "t7", name: "Stuck draft", status: "draft" });
    const later = template({ id: "t8", name: "Welcome 4" });

    // Only the draft has arrived: the wait goes on.
    expect(arrivedTemplate(wait, [...THERE, stuck])).toBeNull();
    // The published one arrives behind it: it ends the wait.
    expect(arrivedTemplate(wait, [...THERE, stuck, later])).toEqual({ template: later, published: true });
    // A wait that takes whatever arrives takes the first, the draft.
    expect(arrivedTemplate({ ...wait, requirePublished: false }, [...THERE, stuck, later])).toEqual({ template: stuck, published: false });
  });

  it("a republished existing template does not end the wait", () => {
    const wait = beginTemplateWait(THERE, "self", true, 0);
    const republished = THERE.map((row) => (row.id === "t2" ? { ...row, status: "ready", createdAt: "2026-10-11T09:00:00Z" } : row));
    expect(arrivedTemplate(wait, republished)).toBeNull();
    // Nor does one that was put away and brought back.
    const restored = THERE.map((row) => (row.id === "t3" ? { ...row, status: "draft" } : row));
    expect(arrivedTemplate(wait, restored)).toBeNull();
  });

  it("reports the first new template when several arrive", () => {
    const wait = beginTemplateWait(THERE, "self", false, 0);
    const first = template({ id: "t7", name: "Welcome 3" });
    const second = template({ id: "t8", name: "Welcome 4" });
    expect(arrivedTemplate(wait, [...THERE, first, second])?.template.id).toBe("t7");
    expect(arrivedTemplate(wait, [...THERE, second, first])?.template.id).toBe("t8");
  });
});

describe("what the wait says", () => {
  const WORD = "Waiting for the new template";

  it("says what it is waiting for", () => {
    const wait = beginTemplateWait(THERE, "self", true, 0);
    expect(waitWords(wait, null)).toEqual({ word: WORD, detail: "Publish it in the editor to continue." });

    // The draft the editor has saved is what the finder reports for a wait that
    // takes whatever arrives; the wait that needs it published is still waiting.
    const draft = template({ id: "t7", name: "Welcome 3", status: "draft" });
    const saved = arrivedTemplate({ ...wait, requirePublished: false }, [...THERE, draft]);
    expect(arrivedTemplate(wait, [...THERE, draft])).toBeNull();
    expect(waitWords(wait, saved)).toEqual({
      word: WORD,
      detail: "“Welcome 3” is a draft. Publish it in the editor to continue.",
    });
  });

  it("never quotes an empty name", () => {
    const wait = beginTemplateWait(THERE, "self", true, 0);
    const unnamed = template({ id: "t7", name: "  ", status: "draft" });
    const saved = arrivedTemplate({ ...wait, requirePublished: false }, [...THERE, unnamed]);
    expect(waitWords(wait, saved).detail).toBe("“Untitled template (t7)” is a draft. Publish it in the editor to continue.");
  });

  it("talks about a draft only when the wait needs the template published", () => {
    const wait = beginTemplateWait(THERE, "self", false, 0);
    const draft = template({ id: "t7", name: "Welcome 3", status: "draft" });
    expect(waitWords(wait, arrivedTemplate(wait, [...THERE, draft]))).toEqual({
      word: WORD,
      detail: "Publish it in the editor to continue.",
    });
  });
});

describe("opening the New email page", () => {
  afterEach(() => localStorage.clear());

  function ports() {
    const cancel = vi.fn();
    const navigate = vi.fn<HandoffPorts["navigate"]>();
    const schedule = vi.fn<HandoffPorts["schedule"]>(() => cancel);
    return { cancel, navigate, schedule, ports: { navigate, schedule } satisfies HandoffPorts };
  }

  it("opens the browser editor on the page for the organization, and waits for no answer", () => {
    const held = ports();
    openNewEmail("memql.example.com", "self", vi.fn(), held.ports);
    expect(held.navigate).toHaveBeenCalledOnce();
    const url = new URL(held.navigate.mock.calls[0]![0]);
    expect(`${url.origin}${url.pathname}`).toBe("https://vscode.memql.example.com/editor/");
    expect(url.searchParams.get("resource")).toBe("memql-file://memql.example.com/new-email/self/New%20email");
    // A browser tab needs no answer; only an installed editor can fail to reply.
    expect(held.schedule).not.toHaveBeenCalled();
  });

  it("opens a browser tab when it is given no ports", () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    try {
      openNewEmail("memql.example.com", "self", vi.fn());
      expect(open).toHaveBeenCalledOnce();
      const [address, target, features] = open.mock.calls[0]!;
      expect(new URL(String(address)).searchParams.get("resource")).toBe("memql-file://memql.example.com/new-email/self/New%20email");
      expect([target, features]).toEqual(["_blank", "noopener,noreferrer"]);
    } finally {
      open.mockRestore();
    }
  });

  it("opens the installed editor the person chose, and reports one that does not answer", () => {
    saveEditorPreference("vscode");
    const held = ports();
    const onNoAnswer = vi.fn();
    const cancel = openNewEmail("memql.example.com", "acme", onNoAnswer, held.ports);

    const url = new URL(held.navigate.mock.calls[0]![0]);
    expect(url.protocol).toBe("vscode:");
    expect(url.searchParams.get("resource")).toBe("memql-file://memql.example.com/new-email/acme/New%20email");

    expect(held.schedule).toHaveBeenCalledWith(expect.any(Function), VSCODE_HANDOFF_TIMEOUT_MS);
    expect(onNoAnswer).not.toHaveBeenCalled();
    held.schedule.mock.calls[0]![0]();
    expect(onNoAnswer).toHaveBeenCalledOnce();

    // What it returns is the way to stop listening for the answer.
    cancel();
    expect(held.cancel).toHaveBeenCalledOnce();
  });
});
