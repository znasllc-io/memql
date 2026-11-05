// AN ORGANIZATION'S BRAND KIT, AND THE DRAFT THE BRAND PAGE EDITS (Task 24).
//
// The kit is the look and the standing parts every email an organization makes
// takes: eight colours and two fonts, a logo in up to three versions, a
// signature, a follow line, social links, the footer and an invitation to
// reply. The engine validates it (emaildoc.ParseBrandKit) and refuses one that
// is wrong with a sentence that names the field; this file only reads a kit
// into what the page edits and writes the draft back as the kit
// `campaignSaveBrandKit` and `campaignBrandPreview` take, in the format's own
// key order.
//
// WHAT THE PAGE DECIDES ITSELF is only what it can know before asking: a
// colour that is not `#rrggbb`, and a logo the kit cannot be without. Every
// other refusal is the engine's, said in its words.
//
// PICTURES pass through untouched: a saved kit's are `data:` pictures and go
// back as they came; one the person uploads is `artifact:<id>`, which the
// engine reads as them and stores as `data:`.

export type BrandColour = "page" | "canvas" | "primary" | "secondary" | "accent" | "tint" | "text" | "muted";

/** The eight colours, in the format's order, with the names the page shows. */
export const BRAND_COLOURS: readonly { key: BrandColour; name: string }[] = [
  { key: "page", name: "Page" },
  { key: "canvas", name: "Canvas" },
  { key: "primary", name: "Primary" },
  { key: "secondary", name: "Secondary" },
  { key: "accent", name: "Accent" },
  { key: "tint", name: "Tint" },
  { key: "text", name: "Text" },
  { key: "muted", name: "Muted" },
];

/** The fonts the page offers. Each renders with an email-safe fallback; a kit may hold another, which is kept. */
export const BRAND_FONTS: readonly string[] = ["Arial", "Georgia", "Helvetica", "Inter", "Sansita", "Times New Roman", "Trebuchet MS", "Verdana"];

/**
 * The three footer links every email carries, by their fixed addresses: the
 * engine signs each one per recipient. Their labels are the organization's to
 * word; their addresses are nobody's to change, and they cannot be removed.
 */
export const FIXED_FOOTER_LINKS: readonly { href: string; label: string; name: string; does: string }[] = [
  { href: "{{preferencesUrl}}", label: "Preferences", name: "preferences", does: "Opens their preferences" },
  { href: "{{unsubscribeUrl}}", label: "Unsubscribe", name: "unsubscribe", does: "Unsubscribes them" },
  { href: "{{viewUrl}}", label: "View in browser", name: "view in browser", does: "Opens this email online" },
];

/** The neutral look the built-in layouts are drawn in, which a new kit starts from. */
const NEUTRAL: Readonly<Record<BrandColour, string>> = {
  page: "#f1f1f1",
  canvas: "#ffffff",
  primary: "#2f2f2f",
  secondary: "#595959",
  accent: "#3d3d3d",
  tint: "#f5f5f5",
  text: "#1c1c1c",
  muted: "#666666",
};

export interface SocialDraft {
  /** The page's own handle for the entry; never sent. */
  key: string;
  network: string;
  href: string;
  icon: string;
}

export interface FooterLinkDraft {
  key: string;
  label: string;
  href: string;
  /** One of the three fixed links: its address is not edited and it is not removed. */
  fixed: boolean;
}

export interface BrandDraft {
  /** As typed: a value that is not a colour stays here, at its field, and is never sent. */
  colours: Record<BrandColour, string>;
  headingFont: string;
  bodyFont: string;
  webFonts: boolean;
  logo: { src: string; onColour: string; wide: string; alt: string; href: string };
  /** The name is shown as text or as a picture; the other is kept for a change of mind and not sent. */
  signature: { photo: string; nameAs: "text" | "picture"; nameText: string; nameSrc: string; title: string; picture: string };
  follow: string;
  social: SocialDraft[];
  /** `lines` is the text box, one line each; `separator` null is the layouts' own. */
  footer: { lines: string; links: FooterLinkDraft[]; separator: string | null };
  reply: { heading: string; label: string; href: string; note: string };
}

/** An edit of the draft: what it becomes, from what it is. */
export type ChangeDraft = (edit: (draft: BrandDraft) => BrandDraft) => void;

let handles = 0;
/** A handle for an entry of a collection, unique for the life of the page. */
export function entryKey(kind: "social" | "link"): string {
  handles += 1;
  return `${kind}-${handles}`;
}

const HEX = /^#[0-9a-fA-F]{6}$/;

/** A colour as the engine takes one: `#rrggbb`, in either case. */
export function isHexColour(value: string): boolean {
  return HEX.test(value);
}

type Bag = Record<string, unknown>;
const bag = (value: unknown): Bag => (value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Bag) : {});
const text = (from: Bag, key: string): string => (typeof from[key] === "string" ? (from[key] as string) : "");
const list = (value: unknown): Bag[] => (Array.isArray(value) ? value.map(bag) : []);

/** A kit, or none yet, as the page edits it. A new kit starts from the neutral look. */
export function draftFromKit(kit: Bag | null): BrandDraft {
  const source = bag(kit);
  const theme = bag(source["theme"]);
  const logo = bag(source["logo"]);
  const signature = bag(source["signature"]);
  const name = bag(signature["name"]);
  const footer = bag(source["footer"]);
  const reply = bag(source["reply"]);
  const button = bag(reply["button"]);
  const known = kit !== null;

  const colours = { ...NEUTRAL };
  if (known) for (const { key } of BRAND_COLOURS) colours[key] = text(theme, key);

  // The fixed links stay where the kit put them, under its labels; one the
  // kit lacks is added at the end, in the page's order. A second link to the
  // same fixed address is an ordinary one.
  const links: FooterLinkDraft[] = [];
  const seen = new Set<string>();
  for (const link of list(footer["links"])) {
    const href = text(link, "href");
    const fixed = FIXED_FOOTER_LINKS.some((one) => one.href === href) && !seen.has(href);
    if (fixed) seen.add(href);
    links.push({ key: entryKey("link"), label: text(link, "label"), href, fixed });
  }
  for (const one of FIXED_FOOTER_LINKS) {
    if (!seen.has(one.href)) links.push({ key: entryKey("link"), label: one.label, href: one.href, fixed: true });
  }

  const lines = Array.isArray(footer["lines"]) ? (footer["lines"] as unknown[]).filter((line): line is string => typeof line === "string") : [];

  return {
    colours,
    headingFont: known ? text(theme, "headingFont") : "Georgia",
    bodyFont: known ? text(theme, "bodyFont") : "Arial",
    webFonts: theme["webFonts"] === true,
    logo: { src: text(logo, "src"), onColour: text(logo, "onColour"), wide: text(logo, "wide"), alt: text(logo, "alt"), href: text(logo, "href") },
    signature: {
      photo: text(signature, "photo"),
      nameAs: text(name, "src") !== "" ? "picture" : "text",
      nameText: text(name, "text"),
      nameSrc: text(name, "src"),
      title: text(signature, "title"),
      picture: text(signature, "picture"),
    },
    follow: text(source, "follow"),
    social: list(source["social"]).map((entry) => ({ key: entryKey("social"), network: text(entry, "network"), href: text(entry, "href"), icon: text(entry, "icon") })),
    footer: { lines: lines.join("\n"), links, separator: typeof footer["separator"] === "string" ? (footer["separator"] as string) : null },
    reply: { heading: text(reply, "heading"), label: text(button, "label"), href: text(button, "href"), note: text(reply, "note") },
  };
}

/** Only what is there: an empty optional part is left out, as the format leaves it out. */
function some(entries: [string, unknown][]): Bag {
  return Object.fromEntries(entries.filter(([, value]) => value !== "" && value !== undefined));
}

/**
 * The draft as the kit the engine takes, in the format's key order. Words are
 * trimmed; a footer line and the separator are kept as typed, spaces and all.
 * An entry with nothing in it is not sent, and neither is an optional part
 * left empty. A colour that is a colour is written in small letters; one that
 * is not is written as typed (`draftProblem` keeps it from being sent).
 */
export function kitFromDraft(draft: BrandDraft): Bag {
  const colour = (value: string) => (isHexColour(value) ? value.toLowerCase() : value);
  const theme: Bag = {};
  for (const { key } of BRAND_COLOURS) theme[key] = colour(draft.colours[key]);
  theme["headingFont"] = draft.headingFont;
  theme["bodyFont"] = draft.bodyFont;
  if (draft.webFonts) theme["webFonts"] = true;

  const { logo, signature, reply } = draft;
  const kit: Bag = {
    theme,
    logo: { src: logo.src, ...some([["onColour", logo.onColour], ["wide", logo.wide]]), alt: logo.alt.trim(), ...some([["href", logo.href.trim()]]) },
  };

  const name =
    signature.nameAs === "picture"
      ? signature.nameSrc === "" ? undefined : { src: signature.nameSrc }
      : signature.nameText.trim() === "" ? undefined : { text: signature.nameText.trim() };
  const signed = some([["photo", signature.photo], ["name", name], ["title", signature.title.trim()], ["picture", signature.picture]]);
  if (Object.keys(signed).length > 0) kit["signature"] = signed;

  if (draft.follow.trim() !== "") kit["follow"] = draft.follow.trim();

  const social = draft.social
    .map((entry) => ({ network: entry.network.trim(), href: entry.href.trim(), icon: entry.icon }))
    .filter((entry) => entry.network !== "" || entry.href !== "" || entry.icon !== "")
    .map((entry) => ({ network: entry.network, href: entry.href, ...some([["icon", entry.icon]]) }));
  if (social.length > 0) kit["social"] = social;

  const lines = draft.footer.lines.split("\n").map((line) => line.replace(/\r$/, ""));
  while (lines.length > 0 && lines[lines.length - 1]!.trim() === "") lines.pop();
  const links = draft.footer.links
    .filter((link) => link.fixed || link.label.trim() !== "" || link.href.trim() !== "")
    .map((link) => ({ label: link.label.trim(), href: link.fixed ? link.href : link.href.trim() }));
  kit["footer"] = {
    ...(lines.length > 0 ? { lines } : {}),
    links,
    ...(draft.footer.separator === null ? {} : { separator: draft.footer.separator }),
  };

  if ([reply.heading, reply.label, reply.href, reply.note].some((value) => value.trim() !== "")) {
    kit["reply"] = { heading: reply.heading.trim(), button: { label: reply.label.trim(), href: reply.href.trim() }, ...some([["note", reply.note.trim()]]) };
  }
  return kit;
}

/**
 * The brand colour a picture is drawn on in an email, while it is a colour:
 * the canvas, or the primary for the reversed logo. The page shows each
 * picture on it, so a dark logo is not lost on a dark page, nor a white one on
 * a light page.
 */
export function groundOf(draft: BrandDraft, colour: "canvas" | "primary"): string | undefined {
  const value = draft.colours[colour];
  return isHexColour(value) ? value : undefined;
}

/** Two drafts that would send the same kit are the same draft. */
export function sameKit(a: BrandDraft, b: BrandDraft): boolean {
  return JSON.stringify(kitFromDraft(a)) === JSON.stringify(kitFromDraft(b));
}

/**
 * What keeps the draft from being sent, in the words the bar says it, or ""
 * when nothing does. Only what the page knows before it asks: a colour that is
 * not one, and the logo and its description, which a kit cannot be without.
 */
export function draftProblem(draft: BrandDraft): string {
  const wrong = BRAND_COLOURS.filter(({ key }) => !isHexColour(draft.colours[key]));
  if (wrong.length > 1) return "Fix the colours";
  if (wrong.length === 1) return `Fix the ${wrong[0]!.name.toLowerCase()} colour`;
  if (draft.logo.src === "") return "Add a logo";
  if (draft.logo.alt.trim() === "") return "Describe the logo";
  return "";
}
