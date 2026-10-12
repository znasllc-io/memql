import { useState, type ComponentType, type ReactNode } from "react";
import { EmailApp } from "../src/apps/email/EmailApp";
import { LocalEmailSettingsStore } from "../src/apps/email/settings";
import { fakeConnection, rowsResult, withSession } from "../test/email/harness";
import { installSeededAccess } from "../test/seededAccess";
import { EMAIL_SEED, EMAIL_MESSAGES } from "./emailSeeds";
export const EMAIL_CLICKS: Record<string, string> = {
  "email-campaign-add": "Add a campaign", "email-campaign-add-waiting": "Add a campaign|Newsletter|Create new template", "email-campaign-add-review": "Add a campaign|Newsletter|Welcome|This cluster's sender|Send now|Send now", "email-campaign-edit": "Edit",
  "email-automation-draft": "Send Autumn preview and wait for confirmation, up to 7 days, sender This cluster's sender, 16 waiting", "email-automation-add": "Add an automation|Someone joins an audience|Newsletter", "email-automation-describe": "Add an automation|Describe it instead|fill:Describe the automation=Welcome customers and send the next letter three days later.|Draft it", "email-rule": "A new account",
  "email-audience-add": "Add an audience", "email-people-report": "Add people|Import a CSV file|file:people", "email-signup-form": "Acme Journal",
  "email-template-plain": "Plain text", "email-template-create": "Create new template|Open the editor", "email-layouts": "Layouts", "email-layouts-none": "Layouts", "email-layout": "Layouts|Acme letter", "email-layout-builtin": "Layouts|Letter", "email-message": "Good things take a little time.", "email-sender": "news@example.com",
};
type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
function EmailQaWindow({ frame: Frame, section, intent }: { frame: Frame; section: string; intent?: Record<string, unknown> }) {
  const [current, setCurrent] = useState(section);
  const [store] = useState(() => new LocalEmailSettingsStore({ getItem: () => null, setItem: () => {} }));
  return <Frame fallback="Email"><EmailApp sectionId={current} navigate={setCurrent} askContext={() => {}} store={store} intent={intent ? { id: `qa-${section}`, payload: intent } : undefined} consumeIntent={() => {}} uploads={{ upload: () => ({ done: Promise.resolve({ artifactId: "qa-csv", fileId: "qa-file" }), abort: () => {} }) }} /></Frame>;
}
export function emailScenes(frame: Frame) {
  function view(section: string, { empty = false, intent, role, mode, single = false, issues = false, noLayouts = false }: { empty?: boolean; intent?: Record<string, unknown>; role?: string; mode?: string; single?: boolean; issues?: boolean; noLayouts?: boolean } = {}) {
    return { role, connect: () => {
      installSeededAccess(role ?? "owner");
      const conn = fakeConnection(empty ? { accounts: EMAIL_SEED.accounts } : { ...EMAIL_SEED, ...(single ? { accounts: EMAIL_SEED.accounts?.slice(0, 1) } : {}), ...(noLayouts ? { layouts: {} } : {}), ...(issues ? { render: { ...(EMAIL_SEED.render as object), issues: [{ code: "unresolved_tag", message: "No value for company.", block: "blocks.0" }] } } : {}) });
      Object.assign(conn.query, { emailInbox: async () => rowsResult([{ mode: mode ?? "capture", messages: empty ? [] : EMAIL_MESSAGES }]) });
      Object.assign(conn.query, { campaignImportRecipients: async () => rowsResult([{ added: 38, duplicates: 2, invalid: 3, total: 43, samples: [{ line: 7, text: "no-address", reason: "Email address is missing." }, { line: 19, text: "lee at example", reason: "Email address is invalid." }] }]) });
      return conn;
    }, wrap: (el: JSX.Element, selectedRole: string) => withSession(el, { role: selectedRole }), render: () => <EmailQaWindow frame={frame} section={section} intent={intent} /> };
  }
  return {
    "email-campaigns": view("campaigns"), "email-campaigns-empty": view("campaigns", { empty: true }),
    ...Object.fromEntries(["add", "add-waiting", "add-review"].map(name => [`email-campaign-${name}`, view("campaigns")])),
    ...Object.fromEntries(["draft", "scheduled", "sending", "sent", "repeating", "edit"].map(name => [`email-campaign-${name}`, view("campaigns", { intent: { campaignId: `${name === "repeating" ? "repeat" : name === "edit" ? "draft" : name}-c` } })])),
    "email-automations": view("automations"), "email-automations-empty": view("automations", { empty: true }), "email-automation-draft": view("automations", { intent: { journeyId: "draft-j" } }), "email-automation-on": view("automations", { intent: { journeyId: "on-j" } }), "email-automation-add": view("automations"), "email-automation-describe": view("automations"), "email-rule": view("automations"),
    "email-audiences": view("audiences"), "email-audiences-empty": view("audiences", { empty: true }), "email-audience-add": view("audiences"),
    ...Object.fromEntries(["audience", "people-report", "signup-form"].map(name => [`email-${name}`, view("audiences", { intent: { audienceId: "newsletter" } })])),
    "email-templates": view("templates"), "email-templates-empty": view("templates", { empty: true }),
    ...Object.fromEntries(["template", "template-plain", "template-issues"].map(name => [`email-${name}`, view("templates", { intent: { templateId: "welcome" }, issues: name === "template-issues" })])),
    "email-template-create": view("templates"), "email-layouts": view("templates"), "email-layouts-none": view("templates", { noLayouts: true }), "email-layout": view("templates"), "email-layout-builtin": view("templates"),
    "email-inbox": view("inbox"), "email-inbox-empty": view("inbox", { empty: true }), "email-inbox-external": view("inbox", { mode: "external", empty: true }), "email-message": view("inbox"),
    "email-settings": view("settings"), "email-settings-member": view("settings", { role: "writer" }), "email-settings-single": view("settings", { single: true }), "email-sender": view("settings"),
  };
}
