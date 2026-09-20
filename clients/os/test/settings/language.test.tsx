import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";

// Settings -> Language (memql#5390): the MemQL line the cluster speaks, the
// forms it deprecates and where its DSL still spells them, and the grammar and
// vocabulary copied for a model.
//
// EVERY READ SITS UNDER `executeNamed` (the Deployables harness's rule), so
// `query.languageStatus()` runs the generated builder and the test sees the
// call string that reaches the wire -- and so do `memqlGrammar()` and
// `memqlVocabulary()`, which this tree serves untyped and the section calls by
// name.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../src/live/connection")>();
  return { ...actual, useOsConnection: () => h.connection };
});

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  editionCaption,
  languageDocsText,
  languageFactsFromRow,
  usesSummary,
  windowSentence,
  type LanguageForm,
} from "../../src/apps/settings/language";
import { SettingsApp } from "../../src/apps/settings/SettingsApp";
import { resetIdsForTest } from "../../src/system/desks";
import { builtinReply, withSession } from "../deployables/harness";
import { rolesOpening } from "../seededAccess";
import { appTileName } from "../appTile";
import { OWNER, READER, openFromLauncher, renderShell } from "./shellHarness";

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const GRAMMAR = 'file = { construct } ;\nconstruct = "concept" identifier block ;\n';
const ENTRIES = [
  { kind: "construct", name: "query", signature: "query <Concept> <name> { ... }", description: "A read." },
  { kind: "annotation", name: "@level", signature: '@level("<level>")', description: "How much intelligence a call needs." },
];
const VOCABULARY: Row = {
  edition: "2026",
  grammarVersion: "g-1",
  kinds: ["construct", "annotation"],
  entries: ENTRIES,
  count: ENTRIES.length,
};

function arrayForm(over: Partial<LanguageForm> = {}): LanguageForm {
  return {
    rule: "deprecated_array_type",
    spelling: "array(T)",
    replacement: "[]T",
    migrator: "memqlmigrate --rewrite=slice-syntax",
    deprecatedIn: "0.23.0",
    refusedFrom: "0.25",
    state: "deprecated",
    uses: [],
    ...over,
  };
}

const TWO_USES = [
  { file: "shop/concepts.memql", line: 4, column: 8, text: "array(string)" },
  { file: "shop/concepts.memql", line: 12, column: 8, text: "array(object)" },
];

function statusRow(over: Record<string, unknown> = {}): Row {
  return {
    language: "1.0",
    edition: "2026",
    status: "frozen",
    grammarVersion: "2026.09-dsl-v1-foundations-77cda60c",
    editorRelease: "0.5.1",
    deprecationWindowMinors: 2,
    forms: [arrayForm({ uses: TWO_USES })],
    ...over,
  };
}

interface Stub {
  executeNamed: ReturnType<typeof vi.fn>;
  /** The names of the document reads, in order. */
  docsAsked: () => string[];
}

function connect({
  status = statusRow(),
  statusError = "",
  docs = (name: string): Row => (name === "memqlGrammar" ? { format: "ebnf", content: GRAMMAR } : VOCABULARY),
  docsError = "",
  hold,
  holdDocs,
}: {
  status?: Row;
  statusError?: string;
  docs?: (name: string) => Row;
  docsError?: string;
  /** Keeps the languageStatus read open until it settles, to look at the loading state. */
  hold?: Promise<unknown>;
  /** Keeps a document read open, to look at what the click started before it landed. */
  holdDocs?: Promise<unknown>;
} = {}): Stub {
  const receipts: Row[] = [];
  const executeNamed = vi.fn(async (name: string, call: string): Promise<Result> => {
    if (name === "languageStatus") {
      if (hold !== undefined) await hold;
      if (statusError !== "") throw new Error(statusError);
      return builtinReply("languageStatus", [status]);
    }
    if (name === "memqlGrammar" || name === "memqlVocabulary") {
      if (holdDocs !== undefined) await holdDocs;
      if (docsError !== "") throw new Error(docsError);
      return builtinReply(name, [docs(name)]);
    }
    if (name === "myAttentionReceipts") return builtinReply("myAttentionReceipts", receipts);
    if (name === "acknowledgeAttention") {
      const changeId = /changeId: "([^"]+)"/.exec(call)?.[1] ?? "";
      const revision = /revision: "([^"]+)"/.exec(call)?.[1] ?? "";
      receipts.push({ id: changeId + revision, changeId, revision });
    }
    return builtinReply(name, []);
  });
  const query = Object.assign(Object.create(QueryClient.prototype), { executeNamed });
  h.connection = { query, subscriptions: null, onStatusChange: () => () => {} };
  return {
    executeNamed,
    docsAsked: () =>
      executeNamed.mock.calls
        .map(([name]) => name as string)
        .filter((name) => name === "memqlGrammar" || name === "memqlVocabulary"),
  };
}

function renderLanguage(role = "owner") {
  return render(withSession(<SettingsApp sectionId="language" navigate={vi.fn()} askContext={vi.fn()} />, { role }));
}

async function landed() {
  await screen.findByText("This cluster speaks");
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: { writeText } });
}

beforeEach(() => {
  h.connection = null;
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// ---------------------------------------------------------------------------
// The view model
// ---------------------------------------------------------------------------

describe("languageFactsFromRow", () => {
  it("reads a well-formed row", () => {
    expect(languageFactsFromRow(statusRow())).toEqual({
      language: "1.0",
      edition: "2026",
      status: "frozen",
      grammarVersion: "2026.09-dsl-v1-foundations-77cda60c",
      editorRelease: "0.5.1",
      deprecationWindowMinors: 2,
      forms: [arrayForm({ uses: TWO_USES })],
    });
  });

  it("reads the same row through the envelope a builtin reply carries", () => {
    const { forms, ...rest } = statusRow();
    expect(languageFactsFromRow({ id: "memql:language", payload: { ...rest, forms } })).toEqual(
      languageFactsFromRow(statusRow()),
    );
  });

  it("answers null for a malformed row, and never throws", () => {
    const { grammarVersion: _dropped, ...noGrammar } = statusRow();
    for (const bad of [
      null,
      undefined,
      "languageStatus",
      42,
      [],
      noGrammar,
      statusRow({ status: "retired" }),
      statusRow({ deprecationWindowMinors: "2" }),
      statusRow({ forms: "array(T)" }),
      statusRow({ forms: [{ ...arrayForm(), state: "gone" }] }),
      statusRow({ forms: [{ ...arrayForm(), migrator: undefined }] }),
      statusRow({ forms: [{ ...arrayForm(), refusedFrom: undefined }] }),
      statusRow({ forms: [arrayForm({ uses: [{ file: "a.memql", line: "4", column: 1, text: "array(x)" }] as never })] }),
      statusRow({ forms: [arrayForm({ uses: [{ file: "a.memql", line: 4, column: 1 }] as never })] }),
      statusRow({ forms: [{ ...arrayForm(), uses: undefined }] }),
    ]) {
      expect(languageFactsFromRow(bad)).toBeNull();
    }
  });
});

describe("usesSummary", () => {
  it("says nothing uses a form when no loaded file does", () => {
    expect(usesSummary([])).toBe("No loaded file uses a deprecated form.");
    expect(usesSummary([arrayForm()])).toBe("No loaded file uses a deprecated form.");
  });

  it("counts uses and the files they are in", () => {
    expect(usesSummary([arrayForm({ uses: [TWO_USES[0]!] })])).toBe("1 use in 1 file");
    expect(usesSummary([arrayForm({ uses: TWO_USES })])).toBe("2 uses in 1 file");
    expect(
      usesSummary([
        arrayForm({ uses: TWO_USES }),
        arrayForm({
          rule: "deprecated_other",
          uses: [{ file: "billing/concepts.memql", line: 7, column: 3, text: "array(int)" }],
        }),
      ]),
    ).toBe("3 uses in 2 files");
  });
});

describe("windowSentence", () => {
  it("states a deprecated form's refusal as a floor, and a refused form's as a fact", () => {
    // refusedFrom is the first release ALLOWED to refuse; a form may warn longer.
    expect(windowSentence(arrayForm())).toBe("Loads with a warning. It may be refused from 0.25.");
    expect(windowSentence(arrayForm({ state: "refused" }))).toBe("Refused since 0.25.");
  });

  it("invents no floor when the engine could not read one", () => {
    expect(windowSentence(arrayForm({ refusedFrom: "" }))).toBe("Loads with a warning.");
    expect(windowSentence(arrayForm({ refusedFrom: "", state: "refused" }))).toBe(
      "Refused: this cluster no longer loads it.",
    );
  });
});

describe("editionCaption", () => {
  it("states a frozen edition's guarantee and its deprecation window", () => {
    expect(editionCaption({ edition: "2026", status: "frozen", deprecationWindowMinors: 2 })).toBe(
      "A frozen edition keeps every form it accepts, with the same meaning, until a new edition. Deprecated forms keep loading for at least 2 minor releases.",
    );
    expect(editionCaption({ edition: "2026", status: "frozen", deprecationWindowMinors: 1 })).toBe(
      "A frozen edition keeps every form it accepts, with the same meaning, until a new edition. Deprecated forms keep loading for at least 1 minor release.",
    );
  });

  it("promises nothing a draft edition does not keep", () => {
    expect(editionCaption({ edition: "2027", status: "draft", deprecationWindowMinors: 2 })).toBe(
      "Edition 2027 is still a draft: the forms it accepts can change before it freezes. Once frozen, it keeps every form it accepts, with the same meaning, until a new edition.",
    );
  });
});

describe("languageDocsText", () => {
  it("copies the grammar verbatim and the vocabulary as the engine's own structure", () => {
    expect(languageDocsText("grammar", { format: "ebnf", content: GRAMMAR })).toBe(GRAMMAR);
    const copied = JSON.parse(languageDocsText("vocabulary", VOCABULARY)) as Record<string, unknown>;
    expect(copied).toEqual(VOCABULARY);
  });

  it("leaves the reply envelope out of the vocabulary document", () => {
    const copied = JSON.parse(
      languageDocsText("vocabulary", {
        id: "memql:vocabulary",
        concept: "memql:vocabulary",
        type: "object",
        createdAt: "2026-01-01T00:00:00Z",
        payload: VOCABULARY,
      }),
    ) as Record<string, unknown>;
    expect(copied).toEqual(VOCABULARY);
  });

  it("throws rather than copy the wrong document under the right label", () => {
    expect(() => languageDocsText("grammar", VOCABULARY)).toThrow("The answer carries no grammar document.");
    expect(() => languageDocsText("vocabulary", { format: "ebnf", content: GRAMMAR })).toThrow(
      "The answer carries no vocabulary document.",
    );
    expect(() => languageDocsText("grammar", null)).toThrow("The answer carries no grammar document.");
  });
});

// ---------------------------------------------------------------------------
// The section
// ---------------------------------------------------------------------------

describe("Settings -> Language", () => {
  it("opens on the line this cluster speaks, its one primary action on the Head", async () => {
    const stub = connect();
    renderLanguage();
    await landed();

    expect(screen.getByRole("heading", { name: "Language" })).toBeTruthy();
    // No meta beside the title (DESIGN.md rule 7): the line and the edition are
    // stated once, in the facts below, with the status that goes with them.
    const head = document.querySelector(".os-settings > .os-head")!;
    expect(head.querySelector(".os-head-meta")).toBeNull();
    // The call string the generated method sends, on mount -- and the documents
    // are NOT read until somebody asks for a copy.
    expect(stub.executeNamed).toHaveBeenCalledWith("languageStatus", "builtin languageStatus()", expect.anything());
    expect(stub.docsAsked()).toEqual([]);

    const speaks = screen.getByRole("region", { name: "This cluster speaks" });
    expect(within(speaks).getByText("MemQL 1.0")).toBeTruthy();
    expect(within(speaks).getByText("2026, frozen")).toBeTruthy();
    expect(within(speaks).getByText("2026.09-dsl-v1-foundations-77cda60c").className).toContain("os-mono");
    // The extension's own name, in full -- test/editorProduct.test.ts holds
    // every "MemQL for ..." in the shell to the one the extension uses.
    expect(within(speaks).getByText("MemQL for Visual Studio Code and Cursor 0.5.1 or later")).toBeTruthy();
    expect(
      within(speaks).getByText(
        "A frozen edition keeps every form it accepts, with the same meaning, until a new edition. Deprecated forms keep loading for at least 2 minor releases.",
      ),
    ).toBeTruthy();
    expect(within(speaks).queryByText(/still a draft/)).toBeNull();

    // The Head carries exactly one primary action (rule 1).
    const primaries = document.querySelectorAll('.os-head .os-button[data-tone="primary"]');
    expect(primaries).toHaveLength(1);
    expect(primaries[0]!.textContent).toContain("Copy grammar");
  });

  it("promises nothing frozen when the cluster's edition is a draft", async () => {
    // THE CAPTION READS THE STATUS. It is the one sentence on this page a
    // person would plan around, and a draft does not keep it.
    connect({ status: statusRow({ edition: "2027", status: "draft" }) });
    renderLanguage();
    await landed();
    const speaks = screen.getByRole("region", { name: "This cluster speaks" });
    expect(within(speaks).getByText("2027, draft")).toBeTruthy();
    expect(
      within(speaks).getByText(
        "Edition 2027 is still a draft: the forms it accepts can change before it freezes. Once frozen, it keeps every form it accepts, with the same meaning, until a new edition.",
      ),
    ).toBeTruthy();
    expect(within(speaks).queryByText(/^A frozen edition keeps/)).toBeNull();
  });

  it("names each deprecated form, its window, its migrator and where it is used", async () => {
    connect();
    renderLanguage();
    await landed();

    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    expect(within(forms).getByText("2 uses in 1 file")).toBeTruthy();
    expect(within(forms).getByText("array(T)").tagName).toBe("CODE");
    expect(within(forms).getByText("[]T").tagName).toBe("CODE");
    // The arrow is drawn for the eye and hidden from assistive tech; the words
    // stand in for it, so the pair reads as a sentence rather than two tokens.
    const pair = within(forms).getByText("array(T)").closest("p")!;
    expect(pair.querySelector("svg")!.getAttribute("aria-hidden")).toBe("true");
    expect(within(pair).getByText("is now written").className).toContain("os-sr-only");
    expect(pair.textContent!.replace(/\s+/g, " ").trim()).toBe("array(T) is now written []T");
    expect(within(forms).getByText("Loads with a warning. It may be refused from 0.25.")).toBeTruthy();

    const command = within(forms).getByRole("textbox", { name: "command" }) as HTMLInputElement;
    expect(command.value).toBe("memqlmigrate --rewrite=slice-syntax");
    expect(command.readOnly).toBe(true);
    expect(within(forms).getByRole("button", { name: "Copy command" })).toBeTruthy();

    // Each use is the place an editor jumps to, plus the spelling AS WRITTEN --
    // the part `array(T)` cannot show.
    const uses = within(forms).getByRole("list", { name: "Uses of array(T)" });
    expect(within(uses).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "shop/concepts.memql:4array(string)",
      "shop/concepts.memql:12array(object)",
    ]);
    // Where the cluster has it comes BEFORE the fix: under the command field the
    // list would read as the command's output.
    expect(uses.compareDocumentPosition(command) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(forms).queryByText("Nothing this cluster loads uses a deprecated form.")).toBeNull();

    const models = screen.getByRole("region", { name: "For models" });
    expect(within(models).getByRole("button", { name: "Copy vocabulary" })).toBeTruthy();
    // The grammar's copy is the Head's action and is not repeated here (rule 7).
    expect(within(models).queryByRole("button", { name: "Copy grammar" })).toBeNull();
  });

  it("says so when the edition deprecates nothing", async () => {
    connect({ status: statusRow({ forms: [] }) });
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    expect(within(forms).getByText("Edition 2026 deprecates nothing.")).toBeTruthy();
    expect(within(forms).queryByRole("list")).toBeNull();
  });

  it("says so when forms are deprecated and nothing this cluster loads uses one", async () => {
    connect({ status: statusRow({ forms: [arrayForm()] }) });
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    const nothing = within(forms).getByText("Nothing this cluster loads uses a deprecated form.");
    // The form is still listed: a window is worth knowing BEFORE anybody uses it.
    const spelling = within(forms).getByText("array(T)");
    expect(within(forms).queryByRole("list", { name: "Uses of array(T)" })).toBeNull();
    // Said ONCE, where the count would be -- under the Subhead, not as a footnote
    // below the list, and not again as a meta beside the Subhead (rule 7).
    expect(nothing.compareDocumentPosition(spelling) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(forms.querySelector(".os-head-meta")).toBeNull();
    expect(within(forms).queryByText("No loaded file uses a deprecated form.")).toBeNull();
  });

  it("offers no ready command for a form nothing uses", async () => {
    // A copy field under a sentence saying nothing needs it read as an
    // instruction: it was the only act-shaped affordance on the panel.
    connect({ status: statusRow({ forms: [arrayForm()] }) });
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });

    // The form and its window still list -- that is the useful half.
    expect(within(forms).getByText("array(T)")).toBeTruthy();
    expect(within(forms).getByText("Loads with a warning. It may be refused from 0.25.")).toBeTruthy();

    // The rewrite is named as a FACT about the form, not offered as an act.
    expect(within(forms).queryByRole("textbox", { name: "command" })).toBeNull();
    const rewrite = within(forms).getByText("memqlmigrate --rewrite=slice-syntax");
    expect(rewrite.tagName).toBe("CODE");
    expect(rewrite.closest("p")!.textContent).toBe("The rewrite is memqlmigrate --rewrite=slice-syntax.");

    // And the panel then carries no control at all -- not the copy, and (with
    // no uses) not the toggle either.
    expect(within(forms).queryAllByRole("button")).toEqual([]);
  });

  it("still offers the command where there is something to run it on", async () => {
    // The reachable positive for the case above: suppressing the act on every
    // form would pass it.
    connect();
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    expect((within(forms).getByRole("textbox", { name: "command" }) as HTMLInputElement).value).toBe(
      "memqlmigrate --rewrite=slice-syntax",
    );
    expect(within(forms).queryByText(/^The rewrite is/)).toBeNull();
  });

  it("keeps a long list of uses behind one control", async () => {
    const many = Array.from({ length: 11 }, (_, i) => ({
      file: "shop/concepts.memql",
      line: i + 1,
      column: 3,
      text: "array(string)",
    }));
    connect({ status: statusRow({ forms: [arrayForm({ uses: many })] }) });
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    expect(within(forms).getByText("11 uses in 1 file")).toBeTruthy();
    const uses = () => within(within(forms).getByRole("list", { name: "Uses of array(T)" })).getAllByRole("listitem");
    expect(uses()).toHaveLength(8);

    const toggle = within(forms).getByRole("button", { name: "Show 3 more" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // A keyboard press: focus is on the control when it acts.
    toggle.focus();
    fireEvent.click(toggle);
    expect(uses()).toHaveLength(11);
    // THE SAME CONTROL, still focused, now saying what a second press does. A
    // control that removed itself would drop focus to the document body.
    expect(within(forms).getByRole("button", { name: "Show fewer" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(toggle);

    fireEvent.click(toggle);
    expect(uses()).toHaveLength(8);
    expect(within(forms).getByRole("button", { name: "Show 3 more" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(toggle);
  });

  it("draws nothing to toggle when every use already fits", async () => {
    connect();
    renderLanguage();
    await landed();
    const forms = screen.getByRole("region", { name: "Deprecated forms" });
    expect(within(forms).queryByRole("button", { name: /^Show / })).toBeNull();
  });

  it("renders a refused read in the engine's own words", async () => {
    const refusal =
      'capability_not_held: "languageStatus" requires the read on app:settings/language capability; the caller (role "user") does not hold it';
    connect({ statusError: refusal });
    renderLanguage();
    expect(await screen.findByText(refusal)).toBeTruthy();
    expect(screen.getByText("The cluster declined this read for owner.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "This cluster speaks" })).toBeNull();
    // The documents are a different read, so both copies stay offered.
    expect(
      within(screen.getByRole("region", { name: "For models" })).getByRole("button", { name: "Copy vocabulary" }),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy grammar" })).toBeTruthy();
  });

  it("says it is loading until the read lands", async () => {
    connect();
    renderLanguage();
    expect(screen.getByText("Loading from the cluster")).toBeTruthy();
    await landed();
    expect(screen.queryByText("Loading from the cluster")).toBeNull();
  });

  it("adds what the read brings only below the controls already on screen", async () => {
    // A control drawn while the read is out must not move when the answer
    // lands: "Copy vocabulary" under the loading line would jump the height of
    // both fact panels, out from under the pointer reaching for it.
    let release!: () => void;
    connect({ hold: new Promise<void>((resolve) => (release = resolve)) });
    const { container } = renderLanguage();
    expect(screen.getByText("Loading from the cluster")).toBeTruthy();
    const controls = () => [
      ...container.querySelectorAll(".os-settings button, .os-settings input, .os-settings textarea"),
    ];
    const whileLoading = controls();
    expect(whileLoading.length).toBeGreaterThan(0);

    await act(async () => release());
    await landed();

    // Every control on screen while loading is the same node, in the same order,
    // ahead of anything new...
    const loaded = controls();
    whileLoading.forEach((node, i) => expect(loaded[i]).toBe(node));
    // ...and every panel the read brought lands after the last of them.
    const last = whileLoading[whileLoading.length - 1]!;
    for (const name of ["This cluster speaks", "Deprecated forms", "For models"]) {
      const region = screen.getByRole("region", { name });
      expect(last.compareDocumentPosition(region) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("does not invent facts from a row it cannot read", async () => {
    connect({ status: statusRow({ status: "retired" }) });
    renderLanguage();
    expect(await screen.findByText("The cluster answered in a form this window does not read.")).toBeTruthy();
    expect(screen.queryByText("MemQL 1.0")).toBeNull();
    expect(screen.queryByRole("region", { name: "Deprecated forms" })).toBeNull();
  });
});

describe("copying for a model", () => {
  it("copies the grammar the cluster serves, and says Copied for two seconds", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const stub = connect();
    const writeText = vi.fn(async (_text: string) => {});
    stubClipboard(writeText);
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
    expect(stub.executeNamed).toHaveBeenCalledWith("memqlGrammar", "builtin memqlGrammar()", expect.anything());
    expect(writeText).toHaveBeenCalledWith(GRAMMAR);

    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(screen.getByRole("button", { name: "Copy grammar" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Copied" })).toBeNull();
  });

  it("copies the vocabulary as the engine's own structure", async () => {
    const stub = connect();
    const writeText = vi.fn(async (_text: string) => {});
    stubClipboard(writeText);
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy vocabulary" }));
    await screen.findByRole("button", { name: "Copied" });
    expect(stub.executeNamed).toHaveBeenCalledWith("memqlVocabulary", "builtin memqlVocabulary()", expect.anything());
    const copied = JSON.parse(writeText.mock.calls[0]![0]) as { entries: unknown };
    expect(copied.entries).toEqual(ENTRIES);
  });

  it("hands over the text to select when the clipboard refuses", async () => {
    connect();
    stubClipboard(async () => Promise.reject(new Error("denied")));
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByText("Copy did not reach the clipboard. Select the text and copy it.")).toBeTruthy();
    const text = screen.getByRole("textbox", { name: "Grammar" }) as HTMLTextAreaElement;
    expect(text.value).toBe(GRAMMAR);
    expect(text.readOnly).toBe(true);
    // Not a confirmation: the button offers the copy again.
    expect(screen.getByRole("button", { name: "Copy grammar" })).toBeTruthy();
  });

  it.each([
    [
      "a clipboard that will not take it",
      { clipboard: async () => Promise.reject(new Error("denied")) },
      "Copy did not reach the clipboard. Select the text and copy it.",
    ],
    [
      "a cluster that will not send it",
      { docsError: "memqlGrammar: refused" },
      "The cluster did not send the grammar.",
    ],
  ])("leaves every control where it was when the grammar copy fails on %s", async (_what, how, says) => {
    // THE HEAD'S COPY IS THE ONE CONTROL AT THE TOP OF THE PAGE, so its answer
    // is the one thing that could land above every other control. Drawn under
    // the Head it pushed "Copy vocabulary" and every per-form control down by
    // the height of a ten-row textarea, out from under a pointer already
    // reaching for one. It goes at the foot instead.
    connect("docsError" in how ? { docsError: how.docsError } : {});
    if ("clipboard" in how) stubClipboard(how.clipboard!);
    else stubClipboard(async () => {});
    const { container } = renderLanguage();
    await landed();

    const controls = () => [
      ...container.querySelectorAll(".os-settings button, .os-settings input, .os-settings textarea"),
    ];
    const before = controls();
    // The Head's copy, the vocabulary's, and the migrator's field and button.
    expect(before.length).toBeGreaterThan(2);

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    const landedAt = await screen.findByText(says);

    // Every control that was on screen is the SAME node at the SAME index --
    // nothing moved, nothing was replaced...
    const after = controls();
    before.forEach((node, i) => expect(after[i]).toBe(node));
    // ...and whatever the copy had to say came after the last of them.
    const last = before[before.length - 1]!;
    expect(last.compareDocumentPosition(landedAt) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("starts the clipboard write inside the click where the browser takes a promised item", async () => {
    // Safari keeps the clipboard only for a write STARTED by the click, and the
    // grammar is still on its way from the cluster at that moment.
    let release!: () => void;
    connect({ holdDocs: new Promise<void>((resolve) => (release = resolve)) });
    const written: Blob[] = [];
    const write = vi.fn(async (items: { data: Record<string, Promise<Blob>> }[]) => {
      written.push(await items[0]!.data["text/plain"]!);
    });
    class ClipboardItemStub {
      constructor(readonly data: Record<string, Promise<Blob>>) {}
    }
    vi.stubGlobal("ClipboardItem", ClipboardItemStub);
    vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: { write, writeText: vi.fn() } });
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(write).toHaveBeenCalledTimes(1);
    await act(async () => release());
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
    expect(await written[0]!.text()).toBe(GRAMMAR);
  });

  it.each([
    ["the clipboard's write", "write"],
    ["ClipboardItem", "item"],
  ])("falls back to writeText, and settles, when %s throws synchronously", async (_what, which) => {
    connect();
    const writeText = vi.fn(async (_text: string) => {});
    const write = vi.fn((_items: unknown) => {
      if (which === "write") throw new Error("write is not allowed here");
      return Promise.resolve();
    });
    class ClipboardItemStub {
      constructor(readonly data: unknown) {
        if (which === "item") throw new TypeError("ClipboardItem cannot take a promise here");
      }
    }
    vi.stubGlobal("ClipboardItem", ClipboardItemStub);
    vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: { write, writeText } });
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
    expect(writeText).toHaveBeenCalledWith(GRAMMAR);
    expect(screen.queryByRole("button", { name: "Copying" })).toBeNull();
  });

  it("settles on the text to select when a synchronous throw leaves no clipboard to fall back to", async () => {
    connect();
    vi.stubGlobal(
      "ClipboardItem",
      class {
        constructor(readonly data: unknown) {}
      },
    );
    vi.stubGlobal("navigator", {
      ...globalThis.navigator,
      clipboard: {
        write: () => {
          throw new Error("write is not allowed here");
        },
      },
    });
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByText("Copy did not reach the clipboard. Select the text and copy it.")).toBeTruthy();
    expect((screen.getByRole("textbox", { name: "Grammar" }) as HTMLTextAreaElement).value).toBe(GRAMMAR);
    // Not stuck at Copying: the control offers the copy again.
    expect(screen.getByRole("button", { name: "Copy grammar" })).toBeTruthy();
  });

  it("tries writeText with the text in hand when a promised write is refused", async () => {
    connect();
    const writeText = vi.fn(async (_text: string) => {});
    const write = vi.fn(async (_items: unknown) => Promise.reject(new Error("promised items are not supported")));
    vi.stubGlobal(
      "ClipboardItem",
      class {
        constructor(readonly data: unknown) {}
      },
    );
    vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: { write, writeText } });
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
    expect(write).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith(GRAMMAR);
  });

  it("renders a refused document read in the engine's own words", async () => {
    connect({ docsError: "memqlVocabulary: unknown kind \"terms\"" });
    stubClipboard(vi.fn(async () => {}));
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy vocabulary" }));
    expect(await screen.findByText("The cluster did not send the vocabulary.")).toBeTruthy();
    expect(screen.getByText('memqlVocabulary: unknown kind "terms"')).toBeTruthy();
  });

  it("refuses to copy an answer that is not the document it asked for", async () => {
    const writeText = vi.fn(async (_text: string) => {});
    connect({ docs: () => ({ format: "markdown", content: "# MemQL" }) });
    stubClipboard(writeText);
    renderLanguage();
    await landed();

    fireEvent.click(screen.getByRole("button", { name: "Copy grammar" }));
    expect(await screen.findByText("The cluster did not send the grammar.")).toBeTruthy();
    expect(writeText).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// The registry, and the attention marker
// ---------------------------------------------------------------------------

describe("the Language section in the registry", () => {
  it("sits after Cluster and opens for the roles the seeds grant", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings")!;
    const ids = settings.sections!.map((s) => s.id);
    expect(ids.indexOf("language")).toBe(ids.indexOf("cluster") + 1);
    expect(settings.sections!.find((s) => s.id === "language")).toEqual({
      id: "language",
      name: "Language",
      requires: "app:settings/language",
    });
    expect(rolesOpening("app:settings/language")).toEqual(["owner", "developer", "admin"]);
  });

  it("declares the change for people who can reach it", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings")!;
    expect(settings.attentionChanges).toContainEqual({
      id: "settings:language",
      revision: "language-1.0",
      sectionId: "language",
      label: "MemQL 1.0 language and deprecations",
    });
  });
});

describe("the unseen-change marker on Language", () => {
  beforeEach(() => {
    resetIdsForTest();
    document.documentElement.removeAttribute("data-theme");
  });

  function languageNavButton() {
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    return within(nav).getByRole("button", { name: /^Language/ });
  }

  it("marks the section, survives the window opening elsewhere, and clears where Language is read", async () => {
    const stub = connect();
    renderShell({ access: OWNER });
    openFromLauncher("Settings");

    // The destination is reachable from the window's own nav, and marked there.
    const button = languageNavButton();
    await waitFor(() => expect(within(button).getByRole("img", { name: "Unseen change" })).toBeTruthy());
    // Opening Settings on another section is an ANCESTOR view: it acknowledges
    // nothing, which is the rule an ancestor marker must never break.
    expect(stub.executeNamed.mock.calls.some(([name]) => name === "acknowledgeAttention")).toBe(false);

    fireEvent.click(button);
    await screen.findByText("This cluster speaks");
    await waitFor(() =>
      expect(stub.executeNamed.mock.calls.filter(([name]) => name === "acknowledgeAttention")).toHaveLength(1),
    );
    const [, call] = stub.executeNamed.mock.calls.find(([name]) => name === "acknowledgeAttention")!;
    expect(call).toContain('changeId: "settings:language"');
    expect(call).toContain('revision: "language-1.0"');
    await waitFor(() => expect(within(languageNavButton()).queryByRole("img", { name: "Unseen change" })).toBeNull());
  });

  it("is not offered to somebody the section is not open to", async () => {
    connect();
    renderShell({ access: READER });
    openFromLauncher("Settings");
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    expect(within(nav).queryByRole("button", { name: /^Language/ })).toBeNull();
  });

  function settingsTile() {
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    return within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", {
      name: appTileName("Settings"),
    });
  }

  it.each([
    ["a viewer", READER],
    ["a member", { ...READER, userId: "u-5", primaryEmail: "member@example.com", role: "writer" }],
  ])("leaves %s's Settings tile unmarked", async (_who, access) => {
    connect();
    renderShell({ access });
    const tile = settingsTile();
    // Their Settings holds no section this change is about, so it marks nothing.
    await act(async () => {
      await Promise.resolve();
    });
    expect(within(tile).queryByRole("img", { name: "Unseen change" })).toBeNull();
    expect(tile.textContent).toBe("Settings");
  });

  it("marks the Settings tile for a role the section opens for", async () => {
    connect();
    renderShell({ access: OWNER });
    // The reachable positive: without it, the two cases above pass on a shell
    // that draws no markers at all.
    await waitFor(() => expect(within(settingsTile()).getByRole("img", { name: "Unseen change" })).toBeTruthy());
  });
});
