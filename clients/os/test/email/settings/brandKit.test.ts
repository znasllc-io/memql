import { describe, expect, it } from "vitest";

import {
  BRAND_COLOURS,
  FIXED_FOOTER_LINKS,
  draftFromKit,
  draftProblem,
  isHexColour,
  kitFromDraft,
} from "../../../src/apps/email/settings/brand/kit";

// A BRAND KIT AND THE PAGE'S DRAFT OF IT (Task 24), as pure functions: the kit
// as the engine validates it (emaildoc.ParseBrandKit) read into what the page
// edits, and written back as the kit `campaignSaveBrandKit` and
// `campaignBrandPreview` take.

const PNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==";

const FULL = {
  theme: {
    page: "#f6f3ed", canvas: "#ffffff", primary: "#1a625c", secondary: "#2f4f4f", accent: "#e9b949",
    tint: "#eef5f3", text: "#263638", muted: "#6b7a7c", headingFont: "Georgia", bodyFont: "Arial", webFonts: true,
  },
  logo: { src: PNG, onColour: PNG, wide: PNG, alt: "Acme", href: "https://acme.example" },
  signature: { photo: PNG, name: { text: "Dana Reyes" }, title: "Head of letters", picture: PNG },
  follow: "Follow along for more",
  social: [{ network: "Instagram", href: "https://instagram.com/acme", icon: PNG }, { network: "YouTube", href: "https://youtube.com/@acme" }],
  footer: {
    lines: ["Acme, 1 Main Street", "", "You signed up at acme.example"],
    links: [
      { label: "View online", href: "{{viewUrl}}" },
      { label: "Contact", href: "mailto:hello@acme.example" },
      { label: "Preferences", href: "{{preferencesUrl}}" },
      { label: "Leave", href: "{{unsubscribeUrl}}" },
    ],
    separator: " · ",
  },
  reply: { heading: "Questions?", button: { label: "Write to us", href: "mailto:hello@acme.example" }, note: "We answer within a day." },
};

describe("a brand kit as the page edits it", () => {
  it("reads a whole kit and writes it back as it was", () => {
    expect(kitFromDraft(draftFromKit(FULL))).toEqual(FULL);
  });

  it("starts a new kit on the layouts' neutral look, with the three fixed footer links and no logo", () => {
    const draft = draftFromKit(null);
    expect(BRAND_COLOURS.map(({ key }) => draft.colours[key])).toEqual([
      "#f1f1f1", "#ffffff", "#2f2f2f", "#595959", "#3d3d3d", "#f5f5f5", "#1c1c1c", "#666666",
    ]);
    expect([draft.headingFont, draft.bodyFont, draft.webFonts]).toEqual(["Georgia", "Arial", false]);
    expect(draft.logo.src).toBe("");
    expect(kitFromDraft(draft)["footer"]).toEqual({
      links: [
        { label: "Preferences", href: "{{preferencesUrl}}" },
        { label: "Unsubscribe", href: "{{unsubscribeUrl}}" },
        { label: "View in browser", href: "{{viewUrl}}" },
      ],
    });
    expect(draftProblem(draft)).toBe("Add a logo");
  });

  it("keeps the three fixed links whatever a kit holds: their own labels, and the missing ones added", () => {
    const draft = draftFromKit({ ...FULL, footer: { links: [{ label: "Leave", href: "{{unsubscribeUrl}}" }, { label: "Shop", href: "https://acme.example/shop" }] } });
    expect(draft.footer.links.map((link) => [link.label, link.href, link.fixed])).toEqual([
      ["Leave", "{{unsubscribeUrl}}", true],
      ["Shop", "https://acme.example/shop", false],
      ["Preferences", "{{preferencesUrl}}", true],
      ["View in browser", "{{viewUrl}}", true],
    ]);
    // Each fixed link is the one its address says, in the order the page names them.
    expect(FIXED_FOOTER_LINKS.map((link) => link.href)).toEqual(["{{preferencesUrl}}", "{{unsubscribeUrl}}", "{{viewUrl}}"]);
  });

  it("leaves out what was left empty: an added entry, a reply, a signature and a separator", () => {
    const draft = draftFromKit({ theme: FULL.theme, logo: { src: PNG, alt: "Acme" }, footer: { links: FULL.footer.links } });
    draft.social.push({ key: "blank", network: " ", href: "", icon: "" });
    draft.footer.links.push({ key: "blank-link", label: "", href: "", fixed: false });
    draft.footer.lines = "\n";
    draft.signature.nameAs = "picture";
    draft.signature.nameText = "Dana";
    const kit = kitFromDraft(draft);
    expect(Object.keys(kit)).toEqual(["theme", "logo", "footer"]);
    expect(kit["footer"]).toEqual({ links: FULL.footer.links });
  });

  it("sends the name the signature is chosen to show, and keeps the other for a change of mind", () => {
    const draft = draftFromKit(FULL);
    draft.signature.nameAs = "picture";
    expect((kitFromDraft(draft)["signature"] as Record<string, unknown>)["name"]).toBeUndefined();
    draft.signature.nameSrc = PNG;
    expect((kitFromDraft(draft)["signature"] as Record<string, unknown>)["name"]).toEqual({ src: PNG });
    draft.signature.nameAs = "text";
    expect((kitFromDraft(draft)["signature"] as Record<string, unknown>)["name"]).toEqual({ text: "Dana Reyes" });
  });

  it("keeps a font the list does not offer, and writes a colour in small letters", () => {
    const draft = draftFromKit({ ...FULL, theme: { ...FULL.theme, headingFont: "Lato", primary: "#1A625C" } });
    const theme = kitFromDraft(draft)["theme"] as Record<string, unknown>;
    expect(theme["headingFont"]).toBe("Lato");
    expect(theme["primary"]).toBe("#1a625c");
  });

  it("says what keeps a draft from being saved, one thing at a time", () => {
    const draft = draftFromKit(FULL);
    expect(draftProblem(draft)).toBe("");
    draft.colours.primary = "#12g";
    expect(draftProblem(draft)).toBe("Fix the primary colour");
    draft.colours.tint = "blue";
    expect(draftProblem(draft)).toBe("Fix the colours");
    draft.colours.primary = "#1a625c";
    draft.colours.tint = "#EEF5F3";
    draft.logo.alt = "  ";
    expect(draftProblem(draft)).toBe("Describe the logo");
    draft.logo.src = "";
    expect(draftProblem(draft)).toBe("Add a logo");
  });

  it("knows a colour by the engine's own rule", () => {
    for (const good of ["#1a625c", "#1A625C", "#000000"]) expect(isHexColour(good), good).toBe(true);
    for (const bad of ["", "#12", "#1a625", "1a625c", "#1a625cc", "#12g45z", " #1a625c"]) expect(isHexColour(bad), bad).toBe(false);
  });
});
