import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBar } from "../../readTheBar";
import { chooseOption } from "../../selectControl";

// EMAIL -> SETTINGS -> BRAND (Task 24): one row in Settings for the chosen
// organization's brand kit, and the page it opens -- the kit's editor, a live
// preview of an email in it, and one bar whose Save is the only act that
// writes.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { fakeConnection, rowsResult, withSession } = await import("../harness");

type Conn = ReturnType<typeof fakeConnection>;

const PNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==";
const REVISION = "2026-11-01T10:00:00Z";
const THEME = {
  page: "#f6f3ed", canvas: "#ffffff", primary: "#1a625c", secondary: "#2f4f4f", accent: "#e9b949",
  tint: "#eef5f3", text: "#263638", muted: "#6b7a7c", headingFont: "Georgia", bodyFont: "Arial",
};
const FIXED = [
  { label: "Preferences", href: "{{preferencesUrl}}" },
  { label: "Unsubscribe", href: "{{unsubscribeUrl}}" },
  { label: "View in browser", href: "{{viewUrl}}" },
];
const KIT = {
  theme: THEME,
  logo: { src: PNG, alt: "Acme", href: "https://acme.example" },
  footer: { lines: ["Acme, 1 Main Street"], links: FIXED },
};

function kitRow(kit: Record<string, unknown> = KIT, over: Partial<Row> = {}): Row {
  return { id: "kit-self", ownerUserId: "v1:identity:user:me", accountId: "self", kit, createdAt: REVISION, ...over };
}

/** An upload port that lands every file as the one artifact. */
function uploader(artifactId = "picture-1") {
  return {
    upload: vi.fn((file: File) => ({
      done: Promise.resolve({ artifactId, title: file.name, fileKind: "file", source: "uploaded" }),
      abort: () => {},
    })),
  };
}

function mount(connection: Conn, uploads = uploader()) {
  h.connection = connection;
  const store = new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} });
  render(withSession(<EmailApp sectionId="settings" navigate={() => {}} askContext={() => {}} store={store} uploads={uploads} />));
  return uploads;
}

const brandSection = () => screen.findByRole("region", { name: "Brand" });

async function openBrand(connection: Conn, uploads = uploader()) {
  mount(connection, uploads);
  const section = await brandSection();
  fireEvent.click(await within(section).findByText("Brand", { selector: ".os-row-name" }));
  expect(await screen.findByRole("heading", { level: 3, name: "Brand" })).toBeTruthy();
  // The page opens on what it read, and draws its first preview at once.
  await waitFor(() => expect(connection.query.campaignBrandPreview).toHaveBeenCalled());
  return uploads;
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barButtons = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const region = (name: string) => screen.getByRole("region", { name });

/** The kit the preview was last asked to draw. */
function previewKit(connection: Conn, call = -1): Record<string, any> {
  const calls = connection.query.campaignBrandPreview.mock.calls;
  return calls.at(call)![0].kit as Record<string, any>;
}

async function save(connection: Conn) {
  await readTheBar();
  fireEvent.click(within(bar()).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(connection.query.campaignSaveBrandKit).toHaveBeenCalled());
}

// ---------------------------------------------------------------------------
// The row in Settings
// ---------------------------------------------------------------------------

describe("Brand in Settings", () => {
  it("is one row after Senders, set up, with no act in it, and it opens the Brand page", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    mount(conn);

    const section = await brandSection();
    expect(within(section).getByRole("heading", { level: 4, name: "Brand" })).toBeTruthy();
    // After Senders, before Testing.
    const brand = within(section).getByRole("heading", { level: 4, name: "Brand" });
    const follows = (a: HTMLElement, b: HTMLElement) => (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    expect(follows(screen.getByRole("heading", { level: 4, name: "Senders" }), brand)).toBe(true);
    expect(follows(brand, await screen.findByRole("heading", { level: 4, name: "Testing" }))).toBe(true);
    expect(await within(section).findByText("Set up")).toBeTruthy();
    // One row, and nothing in it to press but the row.
    expect(within(section).getAllByRole("button")).toHaveLength(1);
    expect(conn.query.brandKitForAccount).toHaveBeenCalledWith({ accountId: "self" }, expect.anything());

    fireEvent.click(within(section).getByText("Brand", { selector: ".os-row-name" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Brand" })).toBeTruthy();
    // The page takes the pane: Settings is parked behind it.
    expect(screen.queryByRole("heading", { level: 3, name: "Settings" })).toBeNull();
    expect(screen.getAllByRole("group", { name: "What you can do with this" })).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "Back to Settings" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Settings" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 3, name: "Brand" })).toBeNull();
  });

  it("says Not set up for an organization with no kit, and its page says what a kit needs first", async () => {
    const conn = fakeConnection();
    mount(conn);
    const section = await brandSection();
    expect(await within(section).findByText("Not set up")).toBeTruthy();

    fireEvent.click(within(section).getByText("Brand", { selector: ".os-row-name" }));
    await screen.findByRole("heading", { level: 3, name: "Brand" });
    expect(within(bar()).getByText("Not set up")).toBeTruthy();
    expect(within(bar()).getByText("Add a logo")).toBeTruthy();
    // Nothing a kit cannot be saved without is offered as Save.
    expect(barButtons()).toEqual([]);
    // With no logo there is no email to show, so nothing is asked for one.
    expect(conn.query.campaignBrandPreview).not.toHaveBeenCalled();
  });

  it("draws shapes, not words, while the brand is read", async () => {
    const conn = fakeConnection();
    conn.query.brandKitForAccount.mockImplementation(() => new Promise(() => {}));
    mount(conn);
    const section = await brandSection();
    const shapes = within(section).getByRole("status");
    expect(shapes.getAttribute("aria-busy")).toBe("true");
    expect(within(section).queryByText(/set up/i)).toBeNull();
  });

  it("does not call a brand it could not read Not set up, and reads it again when asked", async () => {
    const conn = fakeConnection({ brandKits: new Error("brandKitForAccount: You can't read this organization's brand.") });
    mount(conn);
    const section = await brandSection();
    expect(await within(section).findByText("This cluster did not return the brand.")).toBeTruthy();
    expect(within(section).getByText("You can't read this organization's brand.")).toBeTruthy();
    expect(within(section).queryByText("Not set up")).toBeNull();

    conn.query.brandKitForAccount.mockResolvedValueOnce(rowsResult([kitRow()]));
    fireEvent.click(within(section).getByRole("button", { name: "Try again" }));
    expect(await within(section).findByText("Set up")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The page and its one act
// ---------------------------------------------------------------------------

describe("The Brand page", () => {
  it("offers no Save when nothing changed, and takes it back when a change is undone", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    expect(within(bar()).getByText("Saved")).toBeTruthy();
    expect(barButtons()).toEqual([]);

    const primary = screen.getByLabelText("Primary");
    fireEvent.change(primary, { target: { value: "#123456" } });
    expect(within(bar()).getByText("Not saved yet")).toBeTruthy();
    expect(barButtons()).toEqual(["Discard changes", "Save"]);

    fireEvent.change(primary, { target: { value: "#1a625c" } });
    expect(within(bar()).getByText("Saved")).toBeTruthy();
    expect(barButtons()).toEqual([]);
    expect(conn.query.campaignSaveBrandKit).not.toHaveBeenCalled();
  });

  it("saves the kit with the revision it was read at, then stands saved", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    fireEvent.change(screen.getByLabelText("Primary"), { target: { value: "#123456" } });
    await save(conn);

    expect(conn.query.campaignSaveBrandKit).toHaveBeenCalledTimes(1);
    expect(conn.query.campaignSaveBrandKit).toHaveBeenCalledWith({
      accountId: "self",
      kit: { ...KIT, theme: { ...THEME, primary: "#123456" } },
      expectedRevision: REVISION,
    });
    await waitFor(() => expect(within(bar()).getByText("Saved")).toBeTruthy());
    expect(barButtons()).toEqual([]);

    // The next save carries the revision the last one answered.
    fireEvent.change(screen.getByLabelText("Text"), { target: { value: "#111111" } });
    await save(conn);
    await waitFor(() => expect(conn.query.campaignSaveBrandKit).toHaveBeenCalledTimes(2));
    expect(conn.query.campaignSaveBrandKit.mock.calls[1]![0].expectedRevision).toBe("2026-11-05T09:00:01Z");
  });

  it("creates a kit with an empty revision, from an uploaded logo, and Settings then says Set up", async () => {
    const conn = fakeConnection();
    const uploads = uploader("logo-1");
    mount(conn, uploads);
    const section = await brandSection();
    fireEvent.click(await within(section).findByText("Brand", { selector: ".os-row-name" }));
    await screen.findByRole("heading", { level: 3, name: "Brand" });

    const file = new File([new Uint8Array([137, 80, 78, 71])], "logo.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Logo file"), { target: { files: [file] } });
    await waitFor(() => expect(uploads.upload).toHaveBeenCalledWith(file));
    await waitFor(() => expect(within(bar()).getByText("Describe the logo")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Logo description"), { target: { value: "Acme" } });
    await waitFor(() => expect(barButtons()).toEqual(["Discard changes", "Save"]));
    // The preview is asked for once the kit can be drawn, with the upload in it.
    await waitFor(() => expect(previewKit(conn).logo.src).toBe("artifact:logo-1"));

    await save(conn);
    const sent = conn.query.campaignSaveBrandKit.mock.calls[0]![0];
    expect(sent.expectedRevision).toBe("");
    expect(sent.accountId).toBe("self");
    expect((sent.kit as Record<string, any>).logo).toEqual({ src: "artifact:logo-1", alt: "Acme" });
    await waitFor(() => expect(within(bar()).getByText("Saved")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Back to Settings" }));
    expect(await within(await brandSection()).findByText("Set up")).toBeTruthy();
  });

  it("shows contrast warnings beside the preview, and they do not keep Save away", async () => {
    const warning = "Text in #ffffff on #e9b949 has a contrast of 1.9 to 1; the minimum here is 4.5 to 1.";
    const conn = fakeConnection({ brandKits: [kitRow()], brandPreview: { html: "<p>Brand</p>", warnings: [{ code: "contrast", message: warning }] } });
    await openBrand(conn);
    expect(await screen.findByText(warning)).toBeTruthy();
    // They stand under the frame, so their coming and going never moves it.
    const frame = await screen.findByTitle("Email preview");
    const notice = screen.getByText(warning).closest(".os-notice")!;
    expect(frame.compareDocumentPosition(notice) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(frame.parentElement).toBe(notice.parentElement);

    fireEvent.change(screen.getByLabelText("Accent"), { target: { value: "#f2c94c" } });
    expect(barButtons()).toEqual(["Discard changes", "Save"]);
    await save(conn);
    await waitFor(() => expect(within(bar()).getByText("Saved")).toBeTruthy());
    // Still said: the kit was saved as it is.
    expect(screen.getByText(warning)).toBeTruthy();
  });

  it("keeps the draft and shows the engine's sentence when a save is refused", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    conn.query.campaignSaveBrandKit.mockRejectedValueOnce(new Error("campaignSaveBrandKit: this brand changed; reload it before saving"));
    await openBrand(conn);
    fireEvent.change(screen.getByLabelText("Primary"), { target: { value: "#123456" } });
    fireEvent.change(screen.getByLabelText("Logo description"), { target: { value: "Acme, a leaf" } });
    await save(conn);

    expect(await screen.findByText("this brand changed; reload it before saving")).toBeTruthy();
    expect(screen.getByText("The brand wasn't saved.")).toBeTruthy();
    expect((screen.getByLabelText("Primary") as HTMLInputElement).value).toBe("#123456");
    expect((screen.getByLabelText("Logo description") as HTMLInputElement).value).toBe("Acme, a leaf");
    expect(within(bar()).getByText("Not saved yet")).toBeTruthy();
    expect(barButtons()).toEqual(["Discard changes", "Save"]);
  });

  it("says a save whose answer was lost may have landed, and stands saved once the kit read back is the draft", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    const land = conn.query.campaignSaveBrandKit.getMockImplementation()!;
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    // The save lands, and the answer never comes back. The kit is read back only once released.
    conn.query.campaignSaveBrandKit.mockImplementationOnce(async (args) => {
      await land(args);
      throw Object.assign(new Error("the connection closed"), { reason: "closed" });
    });
    const read = conn.query.brandKitForAccount.getMockImplementation()!;
    await openBrand(conn);
    conn.query.brandKitForAccount.mockImplementation(async (args, opts) => { await held; return read(args, opts); });
    fireEvent.change(screen.getByLabelText("Primary"), { target: { value: "#123456" } });
    await save(conn);

    expect(await screen.findByText("No answer came back, so the brand may have been saved.")).toBeTruthy();
    expect(screen.getByText("Saving again is safe.")).toBeTruthy();
    expect(barButtons()).toEqual(["Discard changes", "Save"]);

    await act(async () => release());
    await waitFor(() => expect(within(bar()).getByText("Saved")).toBeTruthy());
    expect(screen.queryByText("No answer came back, so the brand may have been saved.")).toBeNull();
    expect((screen.getByLabelText("Primary") as HTMLInputElement).value).toBe("#123456");
  });

  it("puts the draft back to what is saved on Discard changes", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    fireEvent.change(screen.getByLabelText("Primary"), { target: { value: "#123456" } });
    await readTheBar();
    fireEvent.click(within(bar()).getByRole("button", { name: "Discard changes" }));
    expect((screen.getByLabelText("Primary") as HTMLInputElement).value).toBe("#1a625c");
    expect(barButtons()).toEqual([]);
    expect(conn.query.campaignSaveBrandKit).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// The preview beside the editor
// ---------------------------------------------------------------------------

describe("The Brand page's preview", () => {
  it("fills the Brand page's pane by a rule of the Brand page's own, and leaves the shared frame as it was", async () => {
    // The frame stands directly in the Brand page's preview column, which the fill rule names.
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    expect((await screen.findByTitle("Email preview")).parentElement?.classList.contains("email-brand-preview")).toBe(true);

    const css = readFileSync(join(__dirname, "../../../src/apps/email/email.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map(([, selector, body]) => ({ selector: selector!.trim(), body: body! }));
    // The template page and the inbox keep their frame's own height.
    const shared = rules.filter((rule) => rule.selector === ".email-preview-frame");
    expect(shared).toHaveLength(1);
    expect(shared[0]!.body).toMatch(/height:\s*min\(72vh,\s*900px\)/);
    // Every other rule that sizes the frame is the Brand page's.
    const others = rules.filter((rule) => rule.selector !== ".email-preview-frame" && rule.selector.includes(".email-preview-frame"));
    expect(others.length).toBeGreaterThan(0);
    for (const rule of others) {
      for (const selector of rule.selector.split(",")) expect(selector.trim(), rule.selector).toMatch(/^\.email-brand/);
    }
  });

  it("asks the preview for the part being edited: the end for the footer, the start for the colours, and stops when the person reaches in", async () => {
    // A browser shows a sandboxed srcdoc frame as an opaque window: its address
    // cannot be read, and the page may still ask it to move to a fragment.
    const replace = vi.fn();
    const opaque = { location: { get href(): string { throw new DOMException("Blocked a frame from accessing a cross-origin frame.", "SecurityError"); }, replace } };
    const opaqueWindow = vi.spyOn(HTMLIFrameElement.prototype, "contentWindow", "get").mockReturnValue(opaque as unknown as Window);
    try {
      const conn = fakeConnection({ brandKits: [kitRow()] });
      await openBrand(conn);
      const frame = await screen.findByTitle("Email preview");
      fireEvent.load(frame);
      replace.mockClear();

      const footer = region("Footer");
      const lines = within(footer).getByLabelText("Footer lines");
      fireEvent.focus(lines);
      expect(replace).toHaveBeenLastCalledWith("about:srcdoc#email-preview-end");
      // Moving within the footer is not entering it again.
      const calls = replace.mock.calls.length;
      fireEvent.focus(within(footer).getByLabelText("Label of the unsubscribe link"), { relatedTarget: lines });
      expect(replace.mock.calls.length).toBe(calls);

      // A new email drawn while the footer is edited is kept at its end.
      fireEvent.change(lines, { target: { value: "Acme, 2 Main Street" } });
      await waitFor(() => expect(previewKit(conn).footer.lines).toEqual(["Acme, 2 Main Street"]));
      replace.mockClear();
      fireEvent.load(screen.getByTitle("Email preview"));
      expect(replace).toHaveBeenLastCalledWith("about:srcdoc#email-preview-end");

      // Once the person reaches into the frame, a new email does not move it.
      fireEvent.pointerEnter(screen.getByTitle("Email preview"));
      replace.mockClear();
      fireEvent.change(lines, { target: { value: "Acme, 3 Main Street" } });
      await waitFor(() => expect(previewKit(conn).footer.lines).toEqual(["Acme, 3 Main Street"]));
      fireEvent.load(screen.getByTitle("Email preview"));
      expect(replace).not.toHaveBeenCalled();

      // Entering another group asks again: the colours are at the start.
      fireEvent.focus(screen.getByLabelText("Primary"), { relatedTarget: lines });
      expect(replace).toHaveBeenLastCalledWith("about:srcdoc#email-preview-start");
    } finally {
      opaqueWindow.mockRestore();
    }
  });

  it("draws the part being edited on a layout that has it", async () => {
    // Article has no follow line, no social links and no coloured panel, so
    // one layout cannot show every part: Letter carries the logo as it is, the
    // signature, the follow line and the social links; Confirmation carries the
    // logo on a coloured panel; Article the full-width header, the reply and
    // the footer.
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const drawnOn = () => conn.query.campaignBrandPreview.mock.calls.at(-1)![0].layoutId;
    expect(drawnOn()).toBe("builtin:article");
    const entering = async (element: HTMLElement, layout: string) => {
      fireEvent.focus(element);
      await waitFor(() => expect(drawnOn()).toBe(layout));
    };
    await entering(screen.getByLabelText("Logo description"), "builtin:letter");
    await entering(screen.getByRole("button", { name: "Upload the logo for coloured panels" }), "builtin:confirmation");
    await entering(screen.getByRole("button", { name: "Upload the full-width header" }), "builtin:article");
    await entering(screen.getByLabelText("Logo link"), "builtin:letter");
    await entering(screen.getByRole("button", { name: "Upload the logo" }), "builtin:letter");
    await entering(screen.getByLabelText("Signature title"), "builtin:letter");
    await entering(screen.getByLabelText("Footer lines"), "builtin:article");
    await entering(screen.getByLabelText("Follow line"), "builtin:letter");
    await entering(screen.getByLabelText("Reply heading"), "builtin:article");
    await entering(screen.getByLabelText("Heading font"), "builtin:article");
    // The same kit each time: only the layout moved.
    const kits = conn.query.campaignBrandPreview.mock.calls.map(([args]) => JSON.stringify(args.kit));
    expect(new Set(kits).size).toBe(1);
  });

  it("says under the last email, not over it, when the next cannot be drawn", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    conn.query.campaignBrandPreview.mockRejectedValueOnce(new Error("campaignBrandPreview: reply needs a heading, a button label and a button link"));
    fireEvent.change(screen.getByLabelText("Reply heading"), { target: { value: "Questions?" } });
    const said = await screen.findByText("reply needs a heading, a button label and a button link");
    const frame = await screen.findByTitle("Email preview");
    expect(frame.compareDocumentPosition(said) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The fields
// ---------------------------------------------------------------------------

describe("The Brand page's fields", () => {
  it("refuses a colour that is not a hex at its field, and sends nothing with it", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const primary = screen.getByLabelText("Primary");
    fireEvent.focus(primary);
    fireEvent.change(primary, { target: { value: "#12g" } });
    fireEvent.blur(primary);

    expect(screen.getByText("Use a hex colour like #1a625c.")).toBeTruthy();
    expect(primary.getAttribute("aria-invalid")).toBe("true");
    expect(within(bar()).getByText("Fix the primary colour")).toBeTruthy();
    expect(barButtons()).toEqual(["Discard changes"]);

    // Past the moment a preview would have been asked for, none carried it.
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 600)); });
    const sent = conn.query.campaignBrandPreview.mock.calls.map(([args]) => (args.kit?.["theme"] as Record<string, unknown>)["primary"]);
    expect(sent).not.toContain("#12g");
    expect(conn.query.campaignSaveBrandKit).not.toHaveBeenCalled();

    // Mended, it is taken, and the picker beside it shows it.
    fireEvent.change(primary, { target: { value: "#12AB34" } });
    expect(screen.queryByText("Use a hex colour like #1a625c.")).toBeNull();
    expect((screen.getByLabelText("Choose the primary colour") as HTMLInputElement).value).toBe("#12ab34");
    await waitFor(() => expect(previewKit(conn).theme.primary).toBe("#12ab34"));
    expect(barButtons()).toEqual(["Discard changes", "Save"]);
  });

  it("asks for a preview 400 ms after the last change, not on every one", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    await waitFor(() => expect(conn.query.campaignBrandPreview).toHaveBeenCalledTimes(1));

    vi.useFakeTimers();
    try {
      const primary = screen.getByLabelText("Primary");
      for (const value of ["#111111", "#222222", "#333333"]) {
        fireEvent.change(primary, { target: { value } });
        await act(async () => { await vi.advanceTimersByTimeAsync(300); });
      }
      expect(conn.query.campaignBrandPreview).toHaveBeenCalledTimes(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(99); });
      expect(conn.query.campaignBrandPreview).toHaveBeenCalledTimes(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(1); });
      expect(conn.query.campaignBrandPreview).toHaveBeenCalledTimes(2);
      expect(previewKit(conn).theme.primary).toBe("#333333");
      expect(conn.query.campaignBrandPreview.mock.calls.at(-1)![0]).toMatchObject({ accountId: "self", layoutId: "builtin:article" });
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the footer's unsubscribe link: no way to remove it, and no field for its address", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const footer = region("Footer");
    expect(within(footer).getByLabelText("Label of the unsubscribe link")).toBeTruthy();
    expect(within(footer).queryByDisplayValue("{{unsubscribeUrl}}")).toBeNull();
    expect(within(footer).queryAllByRole("button", { name: /^Remove/ })).toHaveLength(0);

    // A link the person adds is theirs to remove.
    fireEvent.click(within(footer).getByRole("button", { name: "Add a footer link" }));
    fireEvent.change(within(footer).getByLabelText("Label of footer link 4"), { target: { value: "Shop" } });
    fireEvent.change(within(footer).getByLabelText("Address of footer link 4"), { target: { value: "https://acme.example/shop" } });
    await waitFor(() => expect(previewKit(conn).footer.links).toHaveLength(4));
    expect(within(footer).getAllByRole("button", { name: /^Remove/ }).map((button) => button.getAttribute("aria-label"))).toEqual(["Remove footer link 4"]);
    fireEvent.click(within(footer).getByRole("button", { name: "Remove footer link 4" }));
    expect(within(footer).queryByLabelText("Label of footer link 4")).toBeNull();
    expect(within(footer).getByLabelText("Label of the unsubscribe link")).toBeTruthy();
  });

  it("sends the three fixed footer links with their own addresses, whatever they are called", async () => {
    // A kit saved without two of them still shows all three, and opening it is not a change.
    const conn = fakeConnection({ brandKits: [kitRow({ ...KIT, footer: { links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }] } })] });
    await openBrand(conn);
    const footer = region("Footer");
    for (const name of ["preferences", "unsubscribe", "view in browser"]) {
      expect(within(footer).getByLabelText(`Label of the ${name} link`)).toBeTruthy();
    }
    expect(barButtons()).toEqual([]);

    fireEvent.change(within(footer).getByLabelText("Label of the unsubscribe link"), { target: { value: "Leave this list" } });
    fireEvent.change(within(footer).getByLabelText("Label of the view in browser link"), { target: { value: "Read it online" } });
    await save(conn);
    expect((conn.query.campaignSaveBrandKit.mock.calls[0]![0].kit as Record<string, any>).footer.links).toEqual([
      { label: "Leave this list", href: "{{unsubscribeUrl}}" },
      { label: "Preferences", href: "{{preferencesUrl}}" },
      { label: "Read it online", href: "{{viewUrl}}" },
    ]);
  });

  it("refuses a picture over 1 MiB at its field before uploading it, as the engine would", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    const uploads = await openBrand(conn);
    const big = new File([new Uint8Array(1024 * 1024 + 1)], "header.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Full-width header file"), { target: { files: [big] } });
    expect(await within(region("Logo")).findByText("Choose a picture of 1 MiB or less.")).toBeTruthy();
    expect(uploads.upload).not.toHaveBeenCalled();
    // At 1 MiB it goes.
    const fits = new File([new Uint8Array(1024 * 1024)], "header.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Full-width header file"), { target: { files: [fits] } });
    await waitFor(() => expect(uploads.upload).toHaveBeenCalledTimes(1));
  });

  it("sends an uploaded picture as the upload, and a saved one back as it came", async () => {
    const conn = fakeConnection({ brandKits: [kitRow({ ...KIT, logo: { ...KIT.logo, onColour: PNG } })] });
    const uploads = uploader("header-1");
    await openBrand(conn, uploads);

    const file = new File([new Uint8Array([137, 80, 78, 71])], "header.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Full-width header file"), { target: { files: [file] } });
    await waitFor(() => expect(previewKit(conn).logo.wide).toBe("artifact:header-1"));
    // The page shows the picture it was handed, not the name of the upload.
    const shown = Array.from(region("Logo").querySelectorAll("img")).map((img) => img.getAttribute("src") ?? "");
    expect(shown.some((src) => src.startsWith("data:"))).toBe(true);
    expect(shown.some((src) => src.startsWith("artifact:"))).toBe(false);

    await save(conn);
    expect((conn.query.campaignSaveBrandKit.mock.calls[0]![0].kit as Record<string, any>).logo).toEqual({
      src: PNG, onColour: PNG, wide: "artifact:header-1", alt: "Acme", href: "https://acme.example",
    });
  });

  it("takes a removed logo out of what can be saved until another is added", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    fireEvent.click(screen.getByRole("button", { name: "Remove the logo" }));
    expect(within(bar()).getByText("Add a logo")).toBeTruthy();
    expect(barButtons()).toEqual(["Discard changes"]);
    // The logo's own field offers the way back.
    expect(screen.getByRole("button", { name: "Upload the logo" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Remove the logo" })).toBeNull();
  });

  it("adds a social link, sends it once it says something, and removes it", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const social = region("Social");
    fireEvent.click(within(social).getByRole("button", { name: "Add a social link" }));
    fireEvent.change(within(social).getByLabelText("Follow line"), { target: { value: "Follow along" } });
    await waitFor(() => expect(previewKit(conn).follow).toBe("Follow along"));
    // An entry with nothing in it is not a link yet.
    expect(previewKit(conn).social).toBeUndefined();

    fireEvent.change(within(social).getByLabelText("Network of social link 1"), { target: { value: "Instagram" } });
    fireEvent.change(within(social).getByLabelText("Address of social link 1"), { target: { value: "https://instagram.com/acme" } });
    await waitFor(() => expect(previewKit(conn).social).toEqual([{ network: "Instagram", href: "https://instagram.com/acme" }]));

    fireEvent.click(within(social).getByRole("button", { name: "Remove social link 1" }));
    expect(within(social).queryByLabelText("Network of social link 1")).toBeNull();
    await waitFor(() => expect(previewKit(conn).social).toBeUndefined());
  });

  it("keeps the keyboard where the person is: in an entry they add, and on what stays when one goes", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const social = region("Social");
    fireEvent.click(within(social).getByRole("button", { name: "Add a social link" }));
    await waitFor(() => expect(document.activeElement).toBe(within(social).getByLabelText("Network of social link 1")));
    fireEvent.click(within(social).getByRole("button", { name: "Remove social link 1" }));
    await waitFor(() => expect(document.activeElement).toBe(within(social).getByRole("button", { name: "Add a social link" })));

    const footer = region("Footer");
    fireEvent.click(within(footer).getByRole("button", { name: "Add a footer link" }));
    await waitFor(() => expect(document.activeElement).toBe(within(footer).getByLabelText("Label of footer link 4")));
    fireEvent.click(within(footer).getByRole("button", { name: "Remove footer link 4" }));
    await waitFor(() => expect(document.activeElement).toBe(within(footer).getByRole("button", { name: "Add a footer link" })));

    fireEvent.click(screen.getByRole("button", { name: "Remove the logo" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Upload the logo" })));
  });

  it("writes the signature's name as text or as a picture, as chosen", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const signature = region("Signature");
    fireEvent.change(within(signature).getByLabelText("Signature name"), { target: { value: "Dana Reyes" } });
    fireEvent.change(within(signature).getByLabelText("Signature title"), { target: { value: "Head of letters" } });
    await waitFor(() => expect(previewKit(conn).signature).toEqual({ name: { text: "Dana Reyes" }, title: "Head of letters" }));

    chooseOption(within(signature).getByRole("combobox", { name: "Show the name as" }), "A picture");
    expect(within(signature).queryByLabelText("Signature name")).toBeNull();
    expect(within(signature).getByRole("button", { name: "Upload the name picture" })).toBeTruthy();
    await waitFor(() => expect(previewKit(conn).signature).toEqual({ title: "Head of letters" }));
  });

  it("says quietly when the preview cannot be drawn, and Save stays", async () => {
    const conn = fakeConnection({
      brandKits: [kitRow()],
      brandPreview: new Error("campaignBrandPreview: reply needs a heading, a button label and a button link"),
    });
    await openBrand(conn);
    expect(await screen.findByText("reply needs a heading, a button label and a button link")).toBeTruthy();
    expect(within(region("Preview")).getByText("This can't be previewed yet.")).toBeTruthy();
    // It can be asked again, and editing goes on meanwhile.
    const asked = conn.query.campaignBrandPreview.mock.calls.length;
    fireEvent.click(within(region("Preview")).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(conn.query.campaignBrandPreview.mock.calls.length).toBe(asked + 1));
    fireEvent.change(screen.getByLabelText("Primary"), { target: { value: "#123456" } });
    expect(barButtons()).toEqual(["Discard changes", "Save"]);
  });

  it("has no disabled button anywhere on the page", async () => {
    const conn = fakeConnection({ brandKits: [kitRow()] });
    await openBrand(conn);
    const dead = screen
      .queryAllByRole("button")
      .filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true");
    expect(dead.map((button) => button.textContent)).toEqual([]);
  });
});
