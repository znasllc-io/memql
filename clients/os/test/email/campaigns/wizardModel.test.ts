import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  askingActs,
  campaignSentence,
  reviewActs,
  reviewQuestion,
  reviewReading,
  stepReachable,
  stepsFor,
  type CampaignDraft,
  type ReviewFacts,
  type When,
} from "../../../src/apps/email/campaigns/wizardModel";
import type { ActSpec } from "../../../src/apps/email/acts";
import type { TemplateRow } from "../../../src/apps/email/rows";
import { absent, figureOf } from "../../../src/kit/measure";

// ADD A CAMPAIGN, AS A PURE MODEL (memql#6033, Ruling 8).
//
// Which steps the wizard walks, what its floor says on Review and which acts it
// offers there are functions of the answers and the facts, so "an act that is
// not legal is ABSENT, never disabled" is a property of a list asserted without
// a DOM. Pinned in one zone with no daylight saving, so the sentences that name
// a time read the same on every machine.

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

/** Before the times the drafts name (October 14, 2031, a Tuesday). */
const NOW = new Date("2031-10-01T12:00:00Z");
/** After them. */
const LATER = new Date("2031-11-01T12:00:00Z");

const NOW_WHEN: When = { kind: "now" };
const SCHEDULE: When = { kind: "schedule", at: "2031-10-14T09:00" };
const REPEAT: When = { kind: "repeat", weeks: 2, at: "2031-10-14T09:00" };

function draft(over: Partial<CampaignDraft> = {}): CampaignDraft {
  return {
    accountId: "acme",
    audienceId: "a1",
    templateId: "t1",
    senderIdentityId: "s1",
    when: NOW_WHEN,
    name: "August update",
    trackOpens: true,
    trackClicks: true,
    ...over,
  };
}

function template(over: Partial<TemplateRow> = {}): TemplateRow {
  return {
    id: "t1",
    ownerUserId: "v1:identity:user:me",
    name: "Welcome 2",
    subject: "Welcome",
    status: "ready",
    accountId: "acme",
    layoutId: "",
    preheader: "",
    sourceArtifactId: "",
    createdAt: "2026-10-01T00:00:00Z",
    ...over,
  };
}

function facts(over: Partial<ReviewFacts> = {}): ReviewFacts {
  return {
    subscribers: figureOf(1204),
    template: template(),
    sendingReady: true,
    sendingKnown: true,
    sendingReason: "",
    organizationName: "Acme",
    audienceName: "Newsletter",
    draftExists: false,
    canTest: true,
    ...over,
  };
}

const labels = (specs: readonly ActSpec[]) => specs.map((spec) => spec.label);
const dress = (specs: readonly ActSpec[]) => specs.map((spec) => spec.tone ?? (spec.text ? "text" : "quiet"));

describe("the steps", () => {
  it("asks for the organization only when there is a choice", () => {
    expect(stepsFor(draft(), true)).toEqual(["organization", "audience", "template", "sender", "when", "review"]);
    expect(stepsFor(draft(), false)).toEqual(["audience", "template", "sender", "when", "review"]);
  });

  it("asks for the organization when nothing has answered it", () => {
    expect(stepsFor(draft({ accountId: "" }), false)[0]).toBe("organization");
  });

  it("reaches a step once every step before it is answered", () => {
    const fresh = draft({ audienceId: "", templateId: "", senderIdentityId: null, when: null });
    expect(stepReachable("audience", fresh, false)).toBe(true);
    expect(stepReachable("template", fresh, false)).toBe(false);
    expect(stepReachable("review", fresh, false)).toBe(false);

    // This cluster's own sender is an answer; no sender at all is not.
    expect(stepReachable("when", draft({ senderIdentityId: "", when: null }), false)).toBe(true);
    expect(stepReachable("when", draft({ senderIdentityId: null, when: null }), false)).toBe(false);

    // A time that is not a time yet does not answer When.
    expect(stepReachable("review", draft({ when: { kind: "schedule", at: "" } }), false)).toBe(false);
    expect(stepReachable("review", draft({ when: SCHEDULE }), false)).toBe(true);

    // A step the draft does not walk is never reached.
    expect(stepReachable("organization", draft(), false)).toBe(false);
  });
});

describe("what Review says", () => {
  it("is ready, and names the act, when nothing is missing", () => {
    expect(reviewReading(draft(), facts(), NOW)).toEqual({ word: "Ready to send", detail: "", forward: "Send now" });
    expect(reviewReading(draft({ when: SCHEDULE }), facts(), NOW)).toEqual({ word: "Ready to schedule", detail: "", forward: "Schedule" });
    expect(reviewReading(draft({ when: REPEAT }), facts(), NOW)).toEqual({
      word: "Ready to start repeating",
      detail: "A draft doesn't repeat until you start it.",
      forward: "Start repeating",
    });
  });

  it("names the first thing missing, in order: the template, sending, subscribers, the time", () => {
    const everything = facts({ template: template({ status: "draft" }), sendingReady: false, sendingReason: "Verify the sending domain first.", subscribers: figureOf(0) });
    expect(reviewReading(draft({ when: SCHEDULE }), everything, LATER)).toEqual({ word: "Publish “Welcome 2” before sending.", detail: "", forward: null });

    const noTemplateProblem = { ...everything, template: template() };
    expect(reviewReading(draft({ when: SCHEDULE }), noTemplateProblem, LATER)).toEqual({
      word: "Sending isn't set up for Acme.",
      detail: "Verify the sending domain first.",
      forward: null,
    });

    const sending = { ...noTemplateProblem, sendingReady: true, sendingReason: "" };
    expect(reviewReading(draft({ when: SCHEDULE }), sending, LATER)).toEqual({ word: "Newsletter has no subscribers.", detail: "", forward: null });

    const subscribed = { ...sending, subscribers: figureOf(3) };
    expect(reviewReading(draft({ when: SCHEDULE }), subscribed, LATER)).toEqual({ word: "Choose a time in the future.", detail: "", forward: null });
    expect(reviewReading(draft({ when: REPEAT }), subscribed, LATER).word).toBe("Choose a time in the future.");
    // Sending now has no time to be late for.
    expect(reviewReading(draft(), subscribed, LATER).forward).toBe("Send now");
  });

  it("does not take a count it could not read for an empty audience", () => {
    for (const subscribers of [absent("unread"), absent("failed", "Counting is not yours to do."), absent("unmeasured")]) {
      expect(reviewReading(draft(), facts({ subscribers }), NOW).forward).toBe("Send now");
    }
  });

  it("says it could not check sending, rather than that sending isn't set up, when the check failed", () => {
    expect(reviewReading(draft(), facts({ sendingReady: false, sendingFailed: true, sendingReason: "the readiness check timed out" }), NOW)).toEqual({
      word: "Couldn't check sending for Acme.",
      detail: "the readiness check timed out",
      forward: null,
    });
  });

  it("offers nothing forward, and warns of nothing, while sending is still being read", () => {
    expect(reviewReading(draft(), facts({ sendingKnown: false, sendingReady: false, sendingReason: "Sending readiness is not confirmed." }), NOW)).toEqual({
      word: "Review the campaign",
      detail: "",
      forward: null,
    });
  });

  it("keeps saying a repeat is not stored on a draft while something else is missing", () => {
    expect(reviewReading(draft({ when: REPEAT }), facts({ subscribers: figureOf(0) }), NOW)).toEqual({
      word: "Newsletter has no subscribers.",
      detail: "A draft doesn't repeat until you start it.",
      forward: null,
    });
  });
});

describe("the acts on Review", () => {
  it("before a draft exists: Send a test, Save as draft, then the act", () => {
    const specs = reviewActs(draft(), facts(), NOW);
    expect(labels(specs)).toEqual(["Send a test", "Save as draft", "Send now"]);
    expect(dress(specs)).toEqual(["text", "text", "primary"]);
    expect(labels(reviewActs(draft({ when: SCHEDULE }), facts(), NOW))).toEqual(["Send a test", "Save as draft", "Schedule"]);
    expect(labels(reviewActs(draft({ when: REPEAT }), facts(), NOW))).toEqual(["Send a test", "Save as draft", "Start repeating"]);
  });

  it("once a draft exists: Cancel, Send a test, then the act", () => {
    const specs = reviewActs(draft(), facts({ draftExists: true }), NOW);
    expect(labels(specs)).toEqual(["Cancel", "Send a test", "Send now"]);
    expect(dress(specs)).toEqual(["text", "text", "primary"]);
  });

  it("leaves out an act that cannot run rather than disabling it", () => {
    // Nothing to send to: no test.
    expect(labels(reviewActs(draft(), facts({ canTest: false }), NOW))).toEqual(["Save as draft", "Send now"]);
    // Something missing: no forward act, and saving a draft is still there.
    expect(labels(reviewActs(draft(), facts({ template: template({ status: "draft" }) }), NOW))).toEqual(["Send a test", "Save as draft"]);
    expect(labels(reviewActs(draft(), facts({ draftExists: true, sendingReady: false }), NOW))).toEqual(["Cancel", "Send a test"]);
  });

  it("never offers more than three, puts the primary last, and has no disabled entry", () => {
    for (const when of [NOW_WHEN, SCHEDULE, REPEAT]) {
      for (const draftExists of [false, true]) {
        for (const canTest of [false, true]) {
          for (const blocked of [false, true]) {
            for (const at of [NOW, LATER]) {
              const specs = reviewActs(draft({ when }), facts({ draftExists, canTest, subscribers: figureOf(blocked ? 0 : 5) }), at);
              expect(specs.length).toBeLessThanOrEqual(3);
              const primary = specs.findIndex((spec) => spec.tone === "primary" || spec.tone === "danger");
              if (primary >= 0) expect(primary).toBe(specs.length - 1);
              expect(new Set(specs.map((spec) => spec.id)).size).toBe(specs.length);
              for (const spec of specs) expect(spec).not.toHaveProperty("disabled");
            }
          }
        }
      }
    }
  });

  it("asks in the bar: Not yet beside the act, Keep beside Cancel campaign", () => {
    expect(labels(askingActs("forward", "Send now"))).toEqual(["Not yet", "Send now"]);
    expect(dress(askingActs("forward", "Schedule"))).toEqual(["text", "primary"]);
    expect(labels(askingActs("cancel", null))).toEqual(["Keep", "Cancel campaign"]);
    expect(dress(askingActs("cancel", null))).toEqual(["text", "danger"]);
    // A question about an act that is no longer legal asks nothing.
    expect(askingActs("forward", null)).toEqual([]);
  });
});

describe("the campaign as a sentence", () => {
  // Review says each answer once: the rail lists them, and the sentence adds only
  // what the rail cannot -- how many people the audience reaches, and, when the
  // person chooses between organizations, whose campaign it is.
  const names = { template: "Welcome 2", audience: "Newsletter", sender: "news@acme.com", subscribers: figureOf(1204) };

  it("says what goes, to whom and how many, as whom and when", () => {
    expect(campaignSentence(names, NOW_WHEN, ZONE)).toBe("Send “Welcome 2” to Newsletter (1,204 subscribers) as news@acme.com now.");
    expect(campaignSentence(names, SCHEDULE, ZONE)).toBe(
      "Send “Welcome 2” to Newsletter (1,204 subscribers) as news@acme.com on Tue, Oct 14 at 9:00 (America/Phoenix).",
    );
    expect(campaignSentence(names, REPEAT, ZONE)).toBe(
      "Send “Welcome 2” to Newsletter (1,204 subscribers) as news@acme.com every 2 weeks from Tue, Oct 14 at 9:00 (America/Phoenix).",
    );
    expect(campaignSentence({ ...names, subscribers: figureOf(1) }, NOW_WHEN, ZONE)).toBe("Send “Welcome 2” to Newsletter (1 subscriber) as news@acme.com now.");
  });

  it("names the organization only when it was a choice", () => {
    expect(campaignSentence({ ...names, organization: "Operator organization" }, NOW_WHEN, ZONE)).toBe(
      "Send “Welcome 2” for Operator organization to Newsletter (1,204 subscribers) as news@acme.com now.",
    );
  });

  it("leaves out a count it could not read rather than inventing one", () => {
    for (const subscribers of [absent("unread"), absent("failed", "No."), absent("unmeasured")]) {
      expect(campaignSentence({ ...names, subscribers }, NOW_WHEN, ZONE)).toBe("Send “Welcome 2” to Newsletter as news@acme.com now.");
    }
  });

  it("says a count that stopped at the engine's window is at least that many", () => {
    expect(campaignSentence({ ...names, subscribers: figureOf(5000, true) }, NOW_WHEN, ZONE)).toBe(
      "Send “Welcome 2” to Newsletter (5,000 or more subscribers) as news@acme.com now.",
    );
  });
});

describe("the question before the act", () => {
  it("names the audience size", () => {
    expect(reviewQuestion(draft(), facts(), ZONE)).toBe("Send “August update” to 1,204 subscribers now?");
    expect(reviewQuestion(draft({ when: SCHEDULE }), facts(), ZONE)).toBe(
      "Schedule “August update” for Tue, Oct 14 at 9:00 (America/Phoenix), to 1,204 subscribers?",
    );
    expect(reviewQuestion(draft(), facts({ subscribers: figureOf(1) }), ZONE)).toBe("Send “August update” to 1 subscriber now?");
    // A count that stopped at the engine's window is a floor, and says so.
    expect(reviewQuestion(draft(), facts({ subscribers: figureOf(5000, true) }), ZONE)).toBe("Send “August update” to 5,000 or more subscribers now?");
  });

  it("names the audience, not a count, for a campaign that repeats", () => {
    expect(reviewQuestion(draft({ when: REPEAT }), facts(), ZONE)).toBe("Send “August update” to Newsletter every 2 weeks from Tue, Oct 14 at 9:00?");
  });

  it("drops a count it could not read rather than inventing one", () => {
    expect(reviewQuestion(draft(), facts({ subscribers: absent("failed", "No.") }), ZONE)).toBe("Send “August update” now?");
    expect(reviewQuestion(draft({ when: SCHEDULE }), facts({ subscribers: absent("unread") }), ZONE)).toBe(
      "Schedule “August update” for Tue, Oct 14 at 9:00 (America/Phoenix)?",
    );
  });

  it("calls a campaign with no name by its template's", () => {
    expect(reviewQuestion(draft({ name: "  " }), facts(), ZONE)).toBe("Send “Welcome 2” to 1,204 subscribers now?");
  });
});
