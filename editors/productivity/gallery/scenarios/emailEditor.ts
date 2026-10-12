// The email editor's Design page: the email as the engine renders it, a block
// selected, the inspector, and the action bar in the states a person meets.
// Every preview is a real render of a built-in layout made into a finished
// email (or the email written in HTML), as TestWriteEditorGalleryFixtures wrote it.

import { GALLERY_NONCE, type Scenario } from "../../../vscode/gallery/harness.js";
import { EMAIL_COPY, fill } from "../../src/emailCopy.js";
import { emailPage } from "../../src/emailPage.js";
import { email, withHostMessages, type EmailFixture, type Fixtures, type Then } from "../fixtures.js";

const GROUP = "Email editor";
const C = EMAIL_COPY.editor;
const ACME = { id: "acme", name: "Acme Co" };
const CONNECTED = { type: "context", connected: true, organization: ACME, choices: [] };

type Act = "connect" | "openSource" | "saveToMemQL" | "saveLayout" | "publish";
const bar = (state: string, acts: Act[], layoutName?: string) => ({ type: "bar", state, acts, ...(layoutName ? { layoutName } : {}) });
const restore = (state: Record<string, unknown>) => ({ type: "restore", state });
const shown = (fixture: EmailFixture) => ({ type: "document", version: 1, email: fixture.document });
const rendered = (fixture: EmailFixture) => ({ type: "render", seq: 1, html: fixture.html, text: fixture.text, subject: fixture.subject, issues: fixture.issues });

export function scenarios(fixtures: Fixtures): Scenario[] {
  const article = email(fixtures, "article-finished");
  const heroMissing = email(fixtures, "confirmation-hero-missing");
  const letter = email(fixtures, "letter-finished");
  const spotlight = email(fixtures, "spotlight-finished");
  const custom = email(fixtures, "custom");
  const page = (title: string, messages: readonly unknown[], then?: Then) => withHostMessages(emailPage({ title, script: "email-view.js", nonce: GALLERY_NONCE }), messages, then);
  const draft = bar(C.draft, ["saveLayout", "publish"], "Calmer mornings");

  return [
    {
      id: "email-design",
      group: GROUP,
      title: "Design: the Article rendered, the intro selected, a draft",
      render: () => page("Calmer mornings.email.json", [restore({ width: "desktop", selected: "intro" }), CONNECTED, draft, shown(article), rendered(article)], { reveal: "intro" }),
    },
    {
      id: "email-design-mobile",
      group: GROUP,
      title: "Mobile width, the button selected",
      render: () => page("Calmer mornings.email.json", [restore({ width: "mobile", selected: "ask" }), CONNECTED, draft, shown(article), rendered(article)], { reveal: "ask" }),
    },
    {
      id: "email-issues",
      group: GROUP,
      title: "An issue: the Confirmation’s hero picture still to choose",
      render: () => page("Please confirm.email.json", [restore({ width: "desktop", selected: "hero" }), CONNECTED, bar(C.issuesOne, ["saveLayout"], "Please confirm"), shown(heroMissing), rendered(heroMissing)], { reveal: "hero" }),
    },
    {
      id: "email-published",
      group: GROUP,
      title: "Published, with the theme open",
      render: () => page("A note from our founder.email.json", [restore({ width: "desktop", themeOpen: true }), CONNECTED, bar(fill(C.published, { organization: ACME.name }), ["saveLayout"], "A note from our founder"), shown(letter), rendered(letter)], { show: "theme" }),
    },
    {
      id: "email-custom",
      group: GROUP,
      title: "An email written in HTML, with Preview with open",
      render: () => page("Workshop.email.json", [restore({ width: "desktop", samplesOpen: true }), CONNECTED, bar(C.draft, ["publish"]), shown(custom), rendered(custom)]),
    },
    {
      id: "email-loading",
      group: GROUP,
      title: "Before the first render: the Letter’s shape, sweeping",
      render: () => page("A note from our founder.email.json", [CONNECTED, draft, shown(letter)]),
    },
    {
      id: "email-disconnected",
      group: GROUP,
      title: "Not connected: the Spotlight’s shape, still",
      render: () => page("Meet Ace.email.json", [{ type: "context", connected: false, choices: [] }, bar(fill(C.notConnectedTo, { domain: "memql.localhost" }), ["connect"]), shown(spotlight)]),
    },
  ];
}
