// The New email page: choosing how an email starts, writing it, and the states
// around that. The layouts are the engine's built-ins as they ship, each card
// showing its real render; the organization's own layout is a finished Letter.

import { GALLERY_NONCE, type Scenario } from "../../../vscode/gallery/harness.js";
import { newEmailPage } from "../../src/newEmailPage.js";
import { email, withHostMessages, type EmailFixture, type Fixtures, type Then } from "../fixtures.js";

const GROUP = "New email";
const DOMAIN = "memql.localhost";
const ACME = { id: "acme", name: "Acme Co" };
const LABS = { id: "acme-labs", name: "Acme Labs" };
const BUILT_IN = ["confirmation", "letter", "article", "spotlight"];

/** A layout as the host lists it, from the email the engine wrote for it. */
const listed = (fixture: EmailFixture, kind: "builtin" | "organization" = "builtin", id = fixture.id, name = fixture.name, description = fixture.description) =>
  ({ id, name, description, kind, document: fixture.document });

/** The draft a person left: Article, a name and a description. */
const DRAFT = {
  organizationId: ACME.id,
  layoutId: "builtin:article",
  name: "Calmer mornings",
  description: "Three tips for calmer mornings for our newsletter readers. Keep it warm and short, and end with the button to ask our team a question.",
};

export function scenarios(fixtures: Fixtures): Scenario[] {
  const builtIn = BUILT_IN.map(name => email(fixtures, name));
  const founderNote = listed(email(fixtures, "letter-finished"), "organization", "org:founder-note", "Founder’s note", "The monthly letter from our founder, with the signature band and our social links.");
  const setup = (extra: Record<string, unknown> = {}) => ({
    type: "setup", connected: true, domain: DOMAIN, organizations: [ACME], organizationId: ACME.id,
    layouts: builtIn.map(fixture => listed(fixture)), dictation: true, ...extra,
  });
  const thumbnails = (list: readonly { id: string; html: string }[]) => list.map(item => ({ type: "thumbnail", id: item.id, html: item.html }));
  const page = (messages: readonly unknown[], then?: Then) => withHostMessages(newEmailPage({ script: "new-email-view.js", nonce: GALLERY_NONCE }), messages, then);

  return [
    {
      id: "new-email-choose",
      group: GROUP,
      title: "One organization, the four built-in layouts, nothing chosen",
      render: () => page([setup(), ...thumbnails(builtIn)]),
    },
    {
      id: "new-email-ready",
      group: GROUP,
      title: "Ready to write: an organization layout and the built-ins, Article chosen, two references",
      render: () => page([
        setup({ organizations: [ACME, LABS], layouts: [founderNote, ...builtIn.map(fixture => listed(fixture))] }),
        { type: "restore", draft: DRAFT },
        { type: "references", items: [{ label: "Morning routine notes.md", kind: "file" }, { label: "Breakfast table.png", kind: "picture", include: true }] },
        ...thumbnails([{ id: founderNote.id, html: email(fixtures, "letter-finished").html }, ...builtIn]),
      ], { show: '[role="radio"][aria-checked="true"]', align: "start" }),
    },
    {
      id: "new-email-writing",
      group: GROUP,
      title: "Writing: the Article’s shape while the email is written",
      render: () => page([setup(), { type: "restore", draft: DRAFT }, ...thumbnails(builtIn), { type: "writing", layoutId: DRAFT.layoutId }]),
    },
    {
      id: "new-email-failed",
      group: GROUP,
      title: "Couldn’t write the email: the problem and its details, the form kept",
      render: () => page([
        setup(), { type: "restore", draft: DRAFT }, ...thumbnails(builtIn),
        { type: "failed", problem: { message: "The AI couldn’t finish within the time allowed. Try a smaller request, or choose another model in Fleet.", reference: "tools-3b1f6c2e-9a4d-4e8b-b7f2-5d0c8a1e6f42" } },
      ]),
    },
    {
      id: "new-email-disconnected",
      group: GROUP,
      title: "Not connected: Connect",
      render: () => page([{ type: "setup", connected: false, domain: DOMAIN, organizations: [], layouts: [], dictation: false }]),
    },
  ];
}
