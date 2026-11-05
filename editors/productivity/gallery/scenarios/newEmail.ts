// The New email page: choosing how an email starts, and the states around that.
// The grid is the engine's 24 built-in layouts, grouped by category, each drawn as
// the server's own wireframe (the golden files of component/compose/emaildoc). The
// wording and the grouping are the page's own.

import * as fs from "node:fs";
import * as path from "node:path";

import { GALLERY_NONCE, type Scenario } from "../../../vscode/gallery/harness.js";
import { newEmailPage } from "../../src/newEmailPage.js";
import { withHostMessages, type Fixtures, type Then } from "../fixtures.js";

const GROUP = "New email";
const DOMAIN = "memql.localhost";
const ACME = { id: "acme", name: "Acme Co" };
const LABS = { id: "acme-labs", name: "Acme Labs" };

/** Where the real wireframes are: the engine's golden files, drawn by the server's own code (emaildoc.Wireframe). */
const GOLDEN = path.resolve(process.cwd(), "../../component/compose/emaildoc/testdata/golden");

/**
 * The 24 built-in layouts as the server lists them: id (without "builtin:"), name, category and description, copied from the
 * catalogue in component/compose/emaildoc/layouts.go (builtinLayouts). The wireframe of each is its golden file, wireframe-<id>.svg.
 */
const CATALOGUE: [string, string, string, string][] = [
  ["confirmation", "Confirmation", "Welcome", "A welcome on your brand colour with one button that confirms the subscription."],
  ["letter", "Letter", "Letters", "A personal note with a signature band, a promotion and social links."],
  ["article", "Article", "Articles", "A banner, a tinted article body with a highlighted statement, and a signature."],
  ["lesson", "Lesson", "Articles", "A banner, a tinted lesson that ends on its takeaway, a promotion beside a picture, and social links."],
  ["three-reasons", "Three reasons", "Articles", "A banner, a row of icons, a tinted body of titled reasons with a closing line, and a signature."],
  ["side-by-side", "Side by side", "Products", "A banner, a tinted introduction, two cards side by side and closing lines, with a signature."],
  ["spotlight", "Spotlight", "Announcements", "One large picture, a short message and one button."],
  ["welcome-tour", "Welcome tour", "Welcome", "A banner, a short introduction, one to three benefits with icons, a button and social links."],
  ["plain-note", "Plain note", "Letters", "A greeting and a few paragraphs of plain text, with a signature."],
  ["greeting", "Greeting", "Letters", "A full-width picture, a short message and a signature."],
  ["announcement", "Announcement", "Announcements", "A banner, a headline, a short text, one button and social links."],
  ["video", "Video", "Announcements", "A headline, a video picture that links to the video, a short text, one button and social links."],
  ["product-launch", "Product launch", "Products", "A product picture, a headline with a short line above it, a list of features, one button and social links."],
  ["product-grid", "Product grid", "Products", "A headline, a short introduction and rows of two products, each with a picture, name, line and button, with social links."],
  ["product-focus", "Product focus", "Products", "One product with its picture beside its name, text and button, and a quote below."],
  ["offer", "Offer", "Promotions", "A headline, a short text, a highlighted code, one button, small print and social links."],
  ["newsletter", "Newsletter", "News", "A short introduction and one to three stories, each with a picture, title, excerpt and link, with social links."],
  ["steps", "Steps", "Guides", "A headline, a short introduction and two to six numbered steps, each with a title and text, with a button and a reply."],
  ["picture-steps", "Picture steps", "Guides", "A headline and two to four rows, each with a picture beside its text."],
  ["questions", "Questions", "Guides", "A headline, a short introduction and two to six questions, each with its answer, with a reply."],
  ["event", "Event", "Events", "A picture, a headline, a tinted box with when and where, a short text and one button."],
  ["testimonial", "Testimonial", "Feedback", "A highlighted quote, the person who said it with a photo, one button and social links."],
  ["review-request", "Review request", "Feedback", "A headline, a product picture, a short text and one button."],
  ["thank-you", "Thank you", "Feedback", "A headline, a short text, a tinted box of tips, a reply and a signature."],
];

/** The draft a person left: Article, a name and a description. */
const DRAFT = {
  organizationId: ACME.id,
  layoutId: "builtin:article",
  name: "Calmer mornings",
  description: "Three tips for calmer mornings for our newsletter readers. Keep it warm and short, and end with the button to ask our team a question.",
};

export function scenarios(fixtures: Fixtures): Scenario[] {
  void fixtures;
  const layouts = CATALOGUE.map(([id, name, category, description]) => ({ id: `builtin:${id}`, name, category, description, wireframe: fs.readFileSync(path.join(GOLDEN, `wireframe-${id}.svg`), "utf8") }));
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
