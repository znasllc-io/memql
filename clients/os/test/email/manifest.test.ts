import { Mail } from "lucide-react";
import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_EMAIL_SETTINGS,
  EMAIL_OPENING_TABS,
  EMAIL_SECTION_IDS,
  EMAIL_SETTINGS_KEY,
  sanitizeEmailSettings,
} from "../../src/apps/email/settings";

// ONE EMAIL APP (memql#6033). Campaigns moved into Email: one manifest, one
// resource family (`app:email`, `app:email/inbox`, `app:email/logs`) and one
// preferences key. These pin the shape the shell, the seeds and the stored
// preference all agree on.

const email = () => {
  const found = OS_REGISTRY.apps.find((a) => a.id === "email");
  if (!found) throw new Error("no email app");
  return found;
};

describe("one Email app", () => {
  it("is one Email app whose tabs are its nouns, with the inbox and logs gated on their own resources", () => {
    expect(OS_REGISTRY.apps.some((a) => a.id === "campaigns")).toBe(false);

    const app = email();
    expect(app.name).toBe("Email");
    expect(app.requires).toBe("app:email");
    expect(app.icon).toBe(Mail);
    expect(app.settingsSection).toBe("settings");
    expect(app.logsSection).toBe("logs");

    // Senders and Rules are not tabs: a sender is set up in Settings, and a
    // rule is one kind of automation.
    const sections = app.sections ?? [];
    expect(sections.map((s) => s.id)).toEqual([
      "overview",
      "campaigns",
      "automations",
      "audiences",
      "templates",
      "inbox",
      "logs",
      "settings",
    ]);
    expect(sections.map((s) => s.name)).toEqual([
      "Overview",
      "Campaigns",
      "Automations",
      "Audiences",
      "Templates",
      "Inbox",
      "Logs",
      "Settings",
    ]);
    expect(sections.find((s) => s.id === "automations")?.needs).toEqual(["email", "campaigns"]);
    expect(sections.find((s) => s.id === "inbox")?.requires).toBe("app:email/inbox");
    expect(sections.find((s) => s.id === "logs")?.requires).toBe("app:email/logs");
  });

  it("names the records search and Ask can open, one per noun", () => {
    expect(email().records).toEqual([
      { section: "campaigns", idField: "campaignId", query: "campaigns.campaigns", labels: ["name"] },
      { section: "audiences", idField: "audienceId", query: "campaigns.audiences", labels: ["name", "description"] },
      { section: "templates", idField: "templateId", query: "campaigns.templates", labels: ["name", "subject"] },
    ]);
  });

  it("keeps its preferences under its own key and repairs a section it does not declare", () => {
    expect(EMAIL_SETTINGS_KEY).toBe("memql-os-email-v1");
    expect(DEFAULT_EMAIL_SETTINGS.defaultSection).toBe("overview");
    expect(EMAIL_SECTION_IDS).toEqual((email().sections ?? []).map((s) => s.id));

    const repaired = sanitizeEmailSettings({
      version: 1,
      defaultSection: "nowhere",
      includeArchived: true,
      trackByDefault: false,
    });
    expect(repaired.defaultSection).toBe("overview");
    expect(repaired.includeArchived).toBe(true);
    expect(repaired.trackByDefault).toBe(false);

    // A tab this app used to have is a section it no longer declares.
    for (const retired of ["senders", "rules"]) {
      expect(sanitizeEmailSettings({ version: 1, defaultSection: retired, includeArchived: false, trackByDefault: true }).defaultSection).toBe("overview");
    }
  });

  it("opens a window on a tab, which Logs and Settings are not: they are the header's icons", () => {
    expect(EMAIL_OPENING_TABS.map((s) => s.id)).toEqual(["overview", "campaigns", "automations", "audiences", "templates", "inbox"]);
    for (const icon of ["logs", "settings"]) {
      expect(EMAIL_SECTION_IDS).toContain(icon);
      expect(sanitizeEmailSettings({ version: 1, defaultSection: icon, includeArchived: false, trackByDefault: true }).defaultSection).toBe("overview");
    }
    expect(sanitizeEmailSettings({ version: 1, defaultSection: "inbox", includeArchived: false, trackByDefault: true }).defaultSection).toBe("inbox");
  });

  it("does not read a preference saved under a name it no longer has", () => {
    // Pre-release: nothing is migrated, so the old name is simply not there.
    const read = sanitizeEmailSettings({ version: 1, defaultSection: "overview", showFiled: true, trackByDefault: true });
    expect(read.includeArchived).toBe(false);
    expect(read).not.toHaveProperty("showFiled");
  });
});
