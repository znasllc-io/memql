import { afterAll, beforeAll, describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  campaignActs,
  campaignReading,
  confirmSentence,
  type CampaignFacts,
} from "../../../src/apps/email/campaigns/campaignActs";
import type { ActSpec } from "../../../src/apps/email/acts";
import { campaignFromRow, seriesFromRow, templateFromRow, type SeriesRow } from "../../../src/apps/email/rows";
import { absent, figureOf } from "../../../src/kit/measure";
import { campaignRow, templateRow } from "../harness";

// ONE CAMPAIGN'S BAR, AS A PURE TABLE (memql#6033, Ruling 7).
//
// What a campaign's page offers, and what its bar says, are functions of the
// campaign, its series and what is known about sending -- so "an act that is not
// legal is ABSENT, never disabled" is a property of a list that does not hold
// it, asserted without a DOM. Pinned in one zone with no daylight saving, so a
// sentence that names a time reads the same on every machine.

const ZONE = "America/Phoenix";
let previousZone: string | undefined;

beforeAll(() => {
  previousZone = process.env.TZ;
  process.env.TZ = ZONE;
});

afterAll(() => {
  if (previousZone === undefined) delete process.env.TZ;
  else process.env.TZ = previousZone;
});

/** Before the times the campaigns name (October 14, 2031, a Tuesday). */
const NOW = new Date("2031-10-01T12:00:00Z");
/** Tue, Oct 14 2031 at 9:00 in Phoenix. */
const OCT_14 = "2031-10-14T16:00:00Z";

function series(over: Partial<Row> = {}): SeriesRow {
  return seriesFromRow({
    id: "series-1",
    sourceCampaignId: "c1",
    accountId: "acme",
    name: "August update",
    intervalWeeks: 2,
    timeZone: ZONE,
    anchorAt: OCT_14,
    nextAt: OCT_14,
    status: "active",
    lastError: "",
    createdAt: "2031-09-01T12:00:00.000001Z",
    ...over,
  });
}

function facts(campaign: Partial<Row> = {}, over: Partial<CampaignFacts> = {}): CampaignFacts {
  return {
    campaign: campaignFromRow(campaignRow({ id: "c1", accountId: "acme", audienceId: "a1", templateId: "t1", ...campaign })),
    series: null,
    template: templateFromRow(templateRow({ id: "t1", name: "Welcome 2", accountId: "acme" })),
    sendingReady: true,
    sendingReason: "",
    canTest: true,
    organizationName: "Acme",
    now: NOW,
    zone: ZONE,
    subscribers: figureOf(1204),
    audienceName: "Newsletter",
    ...over,
  };
}

const labels = (specs: readonly ActSpec[]) => specs.map((spec) => spec.label);
const dress = (specs: readonly ActSpec[]) => specs.map((spec) => spec.tone ?? (spec.text ? "text" : "quiet"));

/** The bar's grammar, for any table: at most three, the primary last, nothing disabled. */
function wellFormed(specs: readonly ActSpec[]) {
  expect(specs.length).toBeLessThanOrEqual(3);
  const primary = specs.findIndex((spec) => spec.tone === "primary");
  if (primary >= 0) expect(primary).toBe(specs.length - 1);
  for (const spec of specs) expect(spec).not.toHaveProperty("disabled");
  expect(new Set(specs.map((spec) => spec.id)).size).toBe(specs.length);
}

describe("what a campaign's bar offers", () => {
  it("a draft with no time, ready: Edit, Send a test, Send now", () => {
    const specs = campaignActs(facts());
    expect(labels(specs)).toEqual(["Edit", "Send a test", "Send now"]);
    expect(dress(specs)).toEqual(["text", "text", "primary"]);
    expect(specs.map((spec) => spec.id)).toEqual(["edit", "test", "send"]);
    wellFormed(specs);
  });

  it("a draft with a time still to come, ready: Edit, Send a test, Schedule", () => {
    const specs = campaignActs(facts({ scheduledAt: OCT_14 }));
    expect(labels(specs)).toEqual(["Edit", "Send a test", "Schedule"]);
    expect(specs.at(-1)).toMatchObject({ id: "schedule", tone: "primary" });
    wellFormed(specs);
  });

  it("a draft that is not ready offers no way forward, and the reading names the blocker", () => {
    const draftTemplate = facts({}, { template: templateFromRow(templateRow({ id: "t1", name: "Welcome 2", accountId: "acme", status: "draft" })) });
    expect(labels(campaignActs(draftTemplate))).toEqual(["Edit", "Send a test"]);
    expect(campaignReading(draftTemplate)).toEqual({ word: "Draft", detail: "Publish “Welcome 2” before sending.", tone: "none" });

    // Sending that is not set up is said once, by the page, beside the way to
    // set it up (Ruling 6): the bar offers no way forward and says nothing more.
    const unset = facts({}, { sendingReady: false, sendingReason: "Verify the sending domain first." });
    expect(labels(campaignActs(unset))).toEqual(["Edit", "Send a test"]);
    expect(campaignReading(unset).detail).toBe("");

    const nobody = facts({}, { subscribers: figureOf(0) });
    expect(labels(campaignActs(nobody))).toEqual(["Edit", "Send a test"]);
    expect(campaignReading(nobody).detail).toBe("Newsletter has no subscribers.");

    for (const specs of [campaignActs(draftTemplate), campaignActs(unset), campaignActs(nobody)]) wellFormed(specs);
  });

  it("names the first blocker, in the order a person fixes them: the template, sending, subscribers", () => {
    const everything = facts({}, {
      template: templateFromRow(templateRow({ id: "t1", name: "Welcome 2", accountId: "acme", status: "draft" })),
      sendingReady: false,
      subscribers: figureOf(0),
    });
    expect(campaignReading(everything).detail).toBe("Publish “Welcome 2” before sending.");
    // Sending is the page's to say; the bar does not jump past it to the next one.
    expect(campaignReading({ ...everything, template: facts().template }).detail).toBe("");
    expect(campaignReading({ ...everything, template: facts().template, sendingReady: true }).detail).toBe("Newsletter has no subscribers.");
  });

  it("a template that is gone, archived or another organization's is not one to send", () => {
    for (const template of [undefined, templateFromRow(templateRow({ id: "t1", accountId: "acme", status: "archived" })), templateFromRow(templateRow({ id: "t1", accountId: "other" }))]) {
      const f = facts({}, { template });
      expect(labels(campaignActs(f))).toEqual(["Edit", "Send a test"]);
      expect(campaignReading(f).detail).toBe("Choose a published template.");
    }
  });

  it("says nothing, and offers nothing forward, while what decides it is still being read", () => {
    for (const f of [
      facts({}, { sendingKnown: false, sendingReady: false, sendingReason: "Sending readiness is not confirmed." }),
      facts({}, { templateKnown: false, template: undefined }),
    ]) {
      expect(labels(campaignActs(f))).toEqual(["Edit", "Send a test"]);
      expect(campaignReading(f)).toEqual({ word: "Draft", detail: "", tone: "none" });
    }
  });

  it("offers no Edit while its series is still being read: which page Edit opens is not known yet", () => {
    // A draft's Edit asks when it goes; a repeat's asks how often and from when.
    const f = facts({}, { seriesKnown: false });
    expect(labels(campaignActs(f))).toEqual(["Send a test"]);
    expect(campaignReading(f)).toEqual({ word: "Draft", detail: "", tone: "none" });
  });

  it("a sending check that failed is not sending that was never set up -- and is the page's to say", () => {
    const f = facts({}, { sendingReady: false, sendingFailed: true, sendingReason: "the connection was reset" });
    expect(labels(campaignActs(f))).toEqual(["Edit", "Send a test"]);
    expect(campaignReading(f).detail).toBe("");
  });

  it("a count it could not read does not block a send", () => {
    for (const subscribers of [absent("unread"), absent("failed", "Counting is not yours to do."), absent("unmeasured")]) {
      expect(labels(campaignActs(facts({}, { subscribers })))).toEqual(["Edit", "Send a test", "Send now"]);
    }
  });

  it("a draft whose saved time has passed sends now, and says why", () => {
    const f = facts({ scheduledAt: "2031-09-01T16:00:00Z" });
    expect(labels(campaignActs(f))).toEqual(["Edit", "Send a test", "Send now"]);
    expect(campaignReading(f).detail).toBe("Its time has passed.");
  });

  it("a draft with no organization says what to do", () => {
    const f = facts({ accountId: "" }, { canTest: false, organizationName: "" });
    expect(labels(campaignActs(f))).toEqual(["Edit"]);
    // The page's notice says it, and how: the bar does not say it again.
    expect(campaignReading(f).detail).toBe("");
  });

  it("a draft a series repeats from: Pause while it repeats, Resume when it is paused", () => {
    const active = campaignActs(facts({}, { series: series() }));
    expect(labels(active)).toEqual(["Edit", "Send a test", "Pause"]);
    expect(active.at(-1)).toMatchObject({ id: "pause", tone: "primary" });
    const paused = campaignActs(facts({}, { series: series({ status: "paused" }) }));
    expect(labels(paused)).toEqual(["Edit", "Send a test", "Resume"]);
    expect(paused.at(-1)).toMatchObject({ id: "resume", tone: "primary" });
    wellFormed(active);
    wellFormed(paused);
  });

  it("a draft whose series stopped: Edit, Resume, and the series' own sentence", () => {
    const f = facts({}, { series: series({ status: "blocked", lastError: "Publish the template before resuming this schedule." }) });
    expect(labels(campaignActs(f))).toEqual(["Edit", "Resume"]);
    expect(campaignReading(f)).toMatchObject({ word: "Repeating stopped", detail: "Publish the template before resuming this schedule." });
  });

  it("a scheduled campaign: Cancel (it asks), Send a test, Send now", () => {
    const specs = campaignActs(facts({ status: "scheduled", scheduledAt: OCT_14 }));
    expect(labels(specs)).toEqual(["Cancel", "Send a test", "Send now"]);
    expect(dress(specs)).toEqual(["text", "text", "primary"]);
    expect(specs.map((spec) => spec.id)).toEqual(["cancel", "test", "send"]);
    wellFormed(specs);
    // Nothing to send with: no Send now, and the bar says why.
    const blocked = facts({ status: "scheduled", scheduledAt: OCT_14 }, { sendingReady: false });
    expect(labels(campaignActs(blocked))).toEqual(["Cancel", "Send a test"]);
    // Sending is the page's to say; the bar keeps saying when it was meant to go.
    expect(campaignReading(blocked).detail).toBe("Tue, Oct 14 at 9:00");
  });

  it("a send in progress: Send a test, Pause", () => {
    const specs = campaignActs(facts({ status: "sending", recipientCount: 1250, sentCount: 1204 }));
    expect(labels(specs)).toEqual(["Send a test", "Pause"]);
    expect(specs.at(-1)).toMatchObject({ id: "pause", tone: "primary" });
    wellFormed(specs);
  });

  it("a paused send: Cancel, Send a test, Resume -- Resume only while sending is ready", () => {
    const specs = campaignActs(facts({ status: "paused" }));
    expect(labels(specs)).toEqual(["Cancel", "Send a test", "Resume"]);
    wellFormed(specs);
    expect(labels(campaignActs(facts({ status: "paused" }, { sendingReady: false })))).toEqual(["Cancel", "Send a test"]);
    expect(campaignReading(facts({ status: "paused" }, { sendingReady: false })).detail).toBe("");
    // Its template was pinned when it started: a later draft of it does not stop a resume.
    expect(labels(campaignActs(facts({ status: "paused" }, { template: templateFromRow(templateRow({ id: "t1", accountId: "acme", status: "draft" })) })))).toEqual(["Cancel", "Send a test", "Resume"]);
  });

  it("a finished campaign: Send a test, Duplicate", () => {
    for (const status of ["sent", "cancelled", "failed"]) {
      const specs = campaignActs(facts({ status, lastError: status === "failed" ? "the sending service refused the message" : "" }));
      expect(labels(specs)).toEqual(["Send a test", "Duplicate"]);
      expect(dress(specs)).toEqual(["text", "text"]);
      wellFormed(specs);
    }
  });

  it("a finished campaign that a series still repeats from keeps the series' Pause and Resume", () => {
    expect(labels(campaignActs(facts({ status: "sent" }, { series: series() })))).toEqual(["Send a test", "Duplicate", "Pause"]);
    expect(labels(campaignActs(facts({ status: "sent" }, { series: series({ status: "paused" }) })))).toEqual(["Send a test", "Duplicate", "Resume"]);
  });

  it("offers a test only when one can go somewhere", () => {
    const cases: Partial<Row>[] = [{}, { scheduledAt: OCT_14 }, { status: "scheduled", scheduledAt: OCT_14 }, { status: "sending" }, { status: "paused" }, { status: "sent" }];
    for (const campaign of cases) {
      const specs = campaignActs(facts(campaign, { canTest: false }));
      expect(labels(specs)).not.toContain("Send a test");
      wellFormed(specs);
    }
  });

  it("offers nothing for a status it does not know", () => {
    expect(campaignActs(facts({ status: "" }))).toEqual([]);
  });
});

describe("what a campaign's bar says", () => {
  it("in each state", () => {
    expect(campaignReading(facts())).toEqual({ word: "Draft", detail: "", tone: "none" });
    expect(campaignReading(facts({ status: "scheduled", scheduledAt: OCT_14 }))).toEqual({ word: "Scheduled", detail: "Tue, Oct 14 at 9:00", tone: "none" });
    expect(campaignReading(facts({ status: "sending", recipientCount: 1250, sentCount: 1204 }))).toEqual({ word: "Sending", detail: "1,204 of 1,250 sent", tone: "live" });
    expect(campaignReading(facts({ status: "paused" }))).toMatchObject({ word: "Paused", tone: "paused" });
    expect(campaignReading(facts({ status: "sent", completedAt: "2031-10-15T17:30:00Z" }))).toEqual({ word: "Sent", detail: "Wed, Oct 15 at 10:30", tone: "none" });
    expect(campaignReading(facts({ status: "cancelled" }))).toMatchObject({ word: "Cancelled", detail: "" });
    expect(campaignReading(facts({ status: "failed", lastError: "the sending service refused the message" }))).toEqual({
      word: "Failed",
      detail: "the sending service refused the message",
      tone: "none",
    });
  });

  it("a draft a series repeats from reads as the series does", () => {
    expect(campaignReading(facts({}, { series: series() }))).toEqual({
      word: "Repeating",
      detail: "Every 2 weeks · next Tue, Oct 14 at 9:00 (America/Phoenix)",
      tone: "live",
    });
    expect(campaignReading(facts({}, { series: series({ intervalWeeks: 1 }) })).detail).toBe("Every week · next Tue, Oct 14 at 9:00 (America/Phoenix)");
    expect(campaignReading(facts({}, { series: series({ status: "paused" }) }))).toMatchObject({ word: "Repeating paused", tone: "paused" });
    expect(campaignReading(facts({}, { series: series({ status: "blocked", lastError: "The next occurrence has no valid date." }) }))).toMatchObject({
      word: "Repeating stopped",
      detail: "The next occurrence has no valid date.",
    });
  });
});

describe("the question an act asks first", () => {
  it("names how many people a send reaches, and leaves out a count it could not read", () => {
    expect(confirmSentence("send", facts(), figureOf(1204))).toBe("Send “August update” to 1,204 subscribers now?");
    expect(confirmSentence("send", facts(), figureOf(1))).toBe("Send “August update” to 1 subscriber now?");
    expect(confirmSentence("send", facts(), absent("failed", "Counting is not yours to do."))).toBe("Send “August update” now?");
  });

  it("names the time a schedule sends at, on this browser's clock", () => {
    expect(confirmSentence("schedule", facts({ scheduledAt: OCT_14 }), figureOf(1204))).toBe(
      "Schedule “August update” for Tue, Oct 14 at 9:00 (America/Phoenix), to 1,204 subscribers?",
    );
    expect(confirmSentence("schedule", facts({ scheduledAt: OCT_14 }), absent("unread"))).toBe("Schedule “August update” for Tue, Oct 14 at 9:00 (America/Phoenix)?");
  });

  it("says what stays when a campaign is cancelled", () => {
    expect(confirmSentence("cancel", facts({ status: "scheduled", scheduledAt: OCT_14 }), figureOf(1204))).toBe(
      "Cancel “August update”? Nobody will get it, and it stays on the list as cancelled.",
    );
    expect(confirmSentence("cancel", facts({ status: "paused", sentCount: 1204 }), figureOf(1204))).toBe(
      "Cancel “August update”? The 1,204 already sent stay sent, and nobody else will get it.",
    );
    expect(confirmSentence("cancel-draft", facts(), figureOf(1204))).toBe("Cancel “August update”? It won't be sent, and it stays on the list as cancelled.");
    // A cancelled draft does not stop the series it sources.
    expect(confirmSentence("cancel-draft", facts({}, { series: series() }), figureOf(1204))).toBe(
      "Cancel “August update”? It won't be sent, and it stays on the list as cancelled. Its repeating sends go on until you pause them.",
    );
  });
});
