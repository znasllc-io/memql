// The New email page: choosing how an email starts, and the states around that.
// The grid is the engine's built-in layouts, grouped by category, each drawn as a
// wireframe. The server draws the real ones; the gallery has no server, so it draws
// a plain stand-in from each layout's blocks, in the same style (a white page of
// grey bars). The wording and the grouping are the page's own.

import { GALLERY_NONCE, type Scenario } from "../../../vscode/gallery/harness.js";
import { newEmailPage } from "../../src/newEmailPage.js";
import { email, withHostMessages, type EmailFixture, type Fixtures, type Then } from "../fixtures.js";

const GROUP = "New email";
const DOMAIN = "memql.localhost";
const ACME = { id: "acme", name: "Acme Co" };
const LABS = { id: "acme-labs", name: "Acme Labs" };

/** The shape of a block as a bar: its height and how much of the width it takes. */
const BARS: Record<string, [number, number]> = { image: [120, 1], heading: [28, 0.7], text: [64, 1], highlight: [56, 0.8], button: [36, 0.4], divider: [2, 1], spacer: [16, 0], section: [64, 1], columns: [80, 1], social: [24, 0.4], footer: [40, 0.6], html: [64, 1] };

/** A stand-in for the server's wireframe: grey bars on a white page, one per block. */
function wireframe(fixture: EmailFixture): string {
  let y = 24;
  const bars: string[] = [];
  for (const block of fixture.document.blocks ?? []) {
    const [height, width] = BARS[String((block as { type?: string }).type)] ?? [40, 1];
    if (width > 0) bars.push(`<rect x="${(600 - 520 * width) / 2}" y="${y}" width="${520 * width}" height="${height}" rx="2" fill="#e3e5e8"/>`);
    y += height + 16;
  }
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 ${y + 24}" width="600" height="${y + 24}"><rect width="600" height="${y + 24}" fill="#ffffff"/>${bars.join("")}</svg>`;
}

/** The built-in layouts the grid shows: the engine's four, and a few more of the same shapes in other categories. */
const LAYOUTS: { from: string; id: string; name: string; category: string; description: string }[] = [
  { from: "confirmation", id: "builtin:confirmation", name: "Confirm your address", category: "Welcome", description: "Asks a new subscriber to confirm their address." },
  { from: "letter", id: "builtin:letter", name: "Personal letter", category: "Letters", description: "A personal note with a signature." },
  { from: "article", id: "builtin:article", name: "Article", category: "Articles", description: "A banner, a headline, the story and a button." },
  { from: "article", id: "builtin:weekly", name: "Weekly digest", category: "Articles", description: "Several stories under one banner." },
  { from: "spotlight", id: "builtin:spotlight", name: "Spotlight", category: "Promotions", description: "One offer, front and centre." },
  { from: "spotlight", id: "builtin:sale", name: "Sale", category: "Promotions", description: "A deadline and a discount." },
  { from: "letter", id: "builtin:news", name: "Company news", category: "News", description: "What changed this month." },
  { from: "article", id: "builtin:guide", name: "How-to guide", category: "Guides", description: "Steps for getting started." },
  { from: "confirmation", id: "builtin:invite", name: "Event invitation", category: "Events", description: "The date, the place and a button to reply." },
];

/** The draft a person left: Article, a name and a description. */
const DRAFT = {
  organizationId: ACME.id,
  layoutId: "builtin:article",
  name: "Calmer mornings",
  description: "Three tips for calmer mornings for our newsletter readers. Keep it warm and short, and end with the button to ask our team a question.",
};

export function scenarios(fixtures: Fixtures): Scenario[] {
  const layouts = LAYOUTS.map(layout => ({ id: layout.id, name: layout.name, category: layout.category, description: layout.description, wireframe: wireframe(email(fixtures, layout.from)) }));
  const setup = (extra: Record<string, unknown> = {}) => ({
    type: "setup", connected: true, domain: DOMAIN, organizations: [ACME], organizationId: ACME.id, layouts, dictation: true, ...extra,
  });
  const page = (messages: readonly unknown[], then?: Then) => withHostMessages(newEmailPage({ script: "new-email-view.js", nonce: GALLERY_NONCE }), messages, then);

  return [
    {
      id: "new-email-loading",
      group: GROUP,
      title: "Loading: skeleton cards keep the grid’s shape",
      render: () => page([]),
    },
    {
      id: "new-email-choose",
      group: GROUP,
      title: "The grid by category, nothing chosen: no acts yet",
      render: () => page([setup()]),
    },
    {
      id: "new-email-ready",
      group: GROUP,
      title: "Article chosen and the form filled: both acts, a file and the picture of the finished email",
      render: () => page([
        setup({ organizations: [ACME, LABS] }),
        { type: "restore", draft: DRAFT },
        { type: "references", items: [{ label: "Morning routine notes.md", kind: "file" }, { label: "Breakfast table.png", kind: "picture" }] },
        { type: "reference", item: { label: "Finished newsletter.png" } },
      ], { show: "#name", align: "start" }),
    },
    {
      id: "new-email-named",
      group: GROUP,
      title: "Article chosen and named, no description: only Start from the layout",
      render: () => page([setup(), { type: "restore", draft: { ...DRAFT, description: undefined } }], { show: '[role="radio"][aria-checked="true"]', align: "start" }),
    },
    {
      id: "new-email-refused",
      group: GROUP,
      title: "A refused write: the server’s sentence, the form kept",
      render: () => page([
        setup(), { type: "restore", draft: DRAFT },
        { type: "failed", problem: { message: "Add a brand kit before writing an email.", reference: "tools-3b1f6c2e-9a4d-4e8b-b7f2-5d0c8a1e6f42" } },
      ], { show: '[role="radio"][aria-checked="true"]', align: "start" }),
    },
    {
      id: "new-email-disconnected",
      group: GROUP,
      title: "Not connected: Connect",
      render: () => page([{ type: "setup", connected: false, domain: DOMAIN, organizations: [], layouts: [], dictation: false }]),
    },
  ];
}
