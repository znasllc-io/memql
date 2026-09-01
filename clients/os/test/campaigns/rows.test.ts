import { describe, expect, it } from "vitest";

import {
  audienceFingerprint,
  audienceFromRow,
  campaignFingerprint,
  campaignFromRow,
  campaignIsFinished,
  campaignName,
  conceptEntity,
  deliveryFromRow,
  emailReadinessFrom,
  emailRuleFromRow,
  figureOf,
  formatFigure,
  formatRate,
  insertAt,
  mergeTagsFor,
  rateOf,
  recipientFromRow,
  ruleFingerprint,
  ruleSentence,
  senderFingerprint,
  senderIdentityFromRow,
  sendableCount,
  skipReasonSentence,
  statsFromPayload,
  templateFingerprint,
  templateFromRow,
} from "../../src/apps/campaigns/rows";
import {
  campaignRow,
  audienceRow,
  deliveryRow,
  recipientRow,
  ruleRow,
  senderRow,
  templateRow,
} from "./harness";

// The projections and the fingerprints, tested with no React, no browser and
// no cluster -- the reason apps/campaigns/rows.ts is pure.

describe("projecting a campaign", () => {
  it("reads every field campaignFull projects", () => {
    const campaign = campaignFromRow(
      campaignRow({
        id: "v1:campaigns:campaign:c1",
        name: "August update",
        status: "sending",
        recipientCount: 100,
        sentCount: 40,
        skippedCount: 5,
        failedCount: 2,
        lastError: "Graph said 403",
      }),
    );
    expect(campaign.id).toBe("v1:campaigns:campaign:c1");
    expect(campaign.status).toBe("sending");
    expect(campaign.recipientCount).toBe(100);
    expect(campaign.sentCount).toBe(40);
    expect(campaign.lastError).toBe("Graph said 403");
  });

  it("reads a folded event payload the same way it reads a seed row", () => {
    // A row reaches a projection from two places -- a seed (already flattened)
    // and the subscription fold (a CDC envelope whose fields sit inside
    // `payload`) -- and the two have to produce the same object, or a campaign
    // renders one way on load and another the moment anything changes.
    const folded = campaignFromRow({
      id: "v1:campaigns:campaign:c1",
      payload: { name: "August update", status: "sending", sentCount: 40 },
    });
    expect(folded.name).toBe("August update");
    expect(folded.sentCount).toBe(40);
  });

  it("does NOT default an unknown status to draft", () => {
    // Rendering an unknown status as a draft would offer a Start button over
    // a send already in flight.
    expect(campaignFromRow({ id: "x" }).status).toBe("");
  });

  it("reads tracking as ON when the field is absent", () => {
    // `createCampaign` stamps `trackOpens: args.trackOpens ?? true`, so a row
    // that carries no key was created with tracking on.
    const campaign = campaignFromRow({ id: "x" });
    expect(campaign.trackOpens).toBe(true);
    expect(campaign.trackClicks).toBe(true);
  });

  it("names an untitled campaign by its id tail rather than rendering blank", () => {
    expect(campaignName(campaignFromRow({ id: "v1:campaigns:campaign:abc", name: "" }))).toBe(
      "Untitled campaign (abc)",
    );
  });

  it("treats sent, cancelled and failed as finished and nothing else", () => {
    for (const status of ["sent", "cancelled", "failed"]) {
      expect(campaignIsFinished(campaignFromRow({ id: "x", status }))).toBe(true);
    }
    for (const status of ["draft", "scheduled", "sending", "paused", ""]) {
      expect(campaignIsFinished(campaignFromRow({ id: "x", status }))).toBe(false);
    }
  });
});

// ===========================================================================
// THE COUNTER TRAP, PINNED IN BOTH DIRECTIONS
// ===========================================================================
// The two halves of a good arrival cue pull against each other: it has to fire
// when something happened, and it has to not fire the rest of the time. The
// drain worker moves the send counters on every batch, so a cue keyed on one
// is a strobe for the whole duration of every send.
describe("the campaign arrival cue", () => {
  const base = campaignRow({
    id: "v1:campaigns:campaign:c1",
    status: "sending",
    recipientCount: 100,
    sentCount: 40,
    skippedCount: 5,
    failedCount: 2,
  });

  it("stays SILENT when only the send counters move", () => {
    const before = campaignFingerprint(campaignFromRow(base));
    const after = campaignFingerprint(
      campaignFromRow({ ...base, sentCount: 41, skippedCount: 6, failedCount: 3 }),
    );
    expect(after).toBe(before);
  });

  it("stays silent when the frozen recipientCount is stamped", () => {
    // The preflight freezes it at the same moment the status flips, so naming
    // it would fire a second cue for a change the status already announced.
    const before = campaignFingerprint(campaignFromRow({ ...base, recipientCount: 0 }));
    const after = campaignFingerprint(campaignFromRow({ ...base, recipientCount: 4182 }));
    expect(after).toBe(before);
  });

  it("RINGS on a rename", () => {
    const before = campaignFingerprint(campaignFromRow(base));
    const after = campaignFingerprint(campaignFromRow({ ...base, name: "September update" }));
    expect(after).not.toBe(before);
  });

  it("rings on a status change, a reschedule and a retie", () => {
    const before = campaignFingerprint(campaignFromRow(base));
    for (const patch of [
      { status: "paused" },
      { scheduledAt: "2026-09-08T09:00:00Z" },
      { audienceId: "v1:campaigns:audience:other" },
      { templateId: "v1:campaigns:template:other" },
      { senderIdentityId: "v1:campaigns:senderIdentity:other" },
      { accountId: "v1:accounts:account:other" },
    ]) {
      expect(campaignFingerprint(campaignFromRow({ ...base, ...patch }))).not.toBe(before);
    }
  });

  it("rings when a send hits a problem", () => {
    // lastError is not a counter and does not move on a timer: it changes when
    // a send hits something, which is news by construction.
    const before = campaignFingerprint(campaignFromRow(base));
    const after = campaignFingerprint(campaignFromRow({ ...base, lastError: "Graph said 403" }));
    expect(after).not.toBe(before);
  });
});

describe("the other four cues", () => {
  it("rings on an audience rename and an archive", () => {
    const base = audienceRow({ id: "a1" });
    const before = audienceFingerprint(audienceFromRow(base));
    expect(audienceFingerprint(audienceFromRow({ ...base, name: "Other" }))).not.toBe(before);
    expect(audienceFingerprint(audienceFromRow({ ...base, status: "archived" }))).not.toBe(before);
  });

  it("rings on a template COPY EDIT -- somebody fixing a typo is the news", () => {
    const base = templateRow({ id: "t1" });
    const before = templateFingerprint(templateFromRow(base));
    expect(templateFingerprint(templateFromRow({ ...base, textBody: "Hi," }))).not.toBe(before);
    expect(templateFingerprint(templateFromRow({ ...base, subject: "New" }))).not.toBe(before);
  });

  it("rings on a mailbox being retired", () => {
    const base = senderRow({ id: "s1" });
    const before = senderFingerprint(senderIdentityFromRow(base));
    expect(senderFingerprint(senderIdentityFromRow({ ...base, status: "disabled" }))).not.toBe(
      before,
    );
  });

  it("stays SILENT on a rule's liveness fields and RINGS on its form", () => {
    // The concept says it itself: "A LIVENESS field: it moves on its own, so a
    // surface that fingerprints it for arrival cues turns the rules list into
    // a strobe. Display it; do not ring on it."
    const base = ruleRow({ id: "r1", status: "active", firedCount: 4 });
    const before = ruleFingerprint(emailRuleFromRow(base));
    expect(
      ruleFingerprint(
        emailRuleFromRow({ ...base, firedCount: 5, lastFiredAt: "2026-09-01T12:00:00Z" }),
      ),
    ).toBe(before);
    expect(ruleFingerprint(emailRuleFromRow({ ...base, status: "paused" }))).not.toBe(before);
    expect(
      ruleFingerprint(emailRuleFromRow({ ...base, lastError: "the breaker tripped" })),
    ).not.toBe(before);
  });
});

// ---------------------------------------------------------------------------
// Stats: the figures, and the ones that honestly have none
// ---------------------------------------------------------------------------

describe("reading a stats figure", () => {
  it("reads a number, including zero, as a measurement", () => {
    expect(figureOf({ sent: 0 }, "sent").value).toBe(0);
    expect(figureOf({ sent: 12 }, "sent").value).toBe(12);
  });

  it("reads an absent key as NOT MEASURED, never as zero", () => {
    // A zero there would be this window inventing a fact -- and a zero open
    // rate is a thing operators act on.
    expect(figureOf({}, "opensUnique").value).toBeNull();
    expect(figureOf({ opensUnique: null }, "opensUnique").value).toBeNull();
  });

  it("carries the reason a figure is absent", () => {
    const figure = figureOf({}, "opensUnique", "the fold hit its bound");
    expect(figure.value).toBeNull();
    expect(figure.absentBecause).toBe("the fold hit its bound");
  });

  it("reads the nested and the flat spelling of a grouped figure", () => {
    expect(statsFromPayload({ opens: { total: 90, unique: 40 } }).opensUnique.value).toBe(40);
    expect(statsFromPayload({ opensUnique: 40 }).opensUnique.value).toBe(40);
  });

  it("reports an unmeasured unique count WITH its reason", () => {
    const stats = statsFromPayload({ opens: { total: 90 } });
    expect(stats.opensTotal.value).toBe(90);
    expect(stats.opensUnique.value).toBeNull();
    expect(stats.opensUnique.absentBecause).toMatch(/distinct/);
  });

  it("says soft bounces are not measured rather than reporting zero", () => {
    const stats = statsFromPayload({ sent: 10 });
    expect(stats.hardBounces.value).toBeNull();
    expect(stats.hardBounces.absentBecause).toMatch(/soft bounces/i);
  });

  it("renders an absent figure as an em dash", () => {
    expect(formatFigure({ value: null, absentBecause: "" })).toBe("--");
    expect(formatFigure({ value: 0, absentBecause: "" })).toBe("0");
  });
});

describe("engagement rates", () => {
  it("is OF DELIVERED, never of the audience", () => {
    // An open rate over the whole roster silently punishes a campaign for its
    // suppressions, which is the opposite of what suppressing was for.
    expect(rateOf({ value: 40, absentBecause: "" }, 100)).toBe(0.4);
  });

  it("has NO rate when the numerator was not measured", () => {
    expect(rateOf({ value: null, absentBecause: "x" }, 100)).toBeNull();
  });

  it("has NO rate when nothing was delivered -- not 0%", () => {
    expect(rateOf({ value: 0, absentBecause: "" }, 0)).toBeNull();
  });

  it("renders a missing rate as an em dash and a real one as a percentage", () => {
    expect(formatRate(null)).toBe("--");
    expect(formatRate(0.4)).toBe("40%");
    expect(formatRate(0.043)).toBe("4.3%");
    expect(formatRate(0)).toBe("0%");
  });
});

// ---------------------------------------------------------------------------
// Merge tags
// ---------------------------------------------------------------------------

describe("merge tags", () => {
  it("offers the four base tags with no sample, and no fields.*", () => {
    const tags = mergeTagsFor({ recipient: null, campaignName: "", accountName: "" });
    expect(tags.map((t) => t.tag)).toEqual([
      "{{displayName}}",
      "{{email}}",
      "{{campaignName}}",
      "{{accountName}}",
    ]);
    expect(tags.every((t) => t.preview === "")).toBe(true);
  });

  it("DISCOVERS fields.* from a real recipient -- nothing else can", () => {
    // `{{fields.company}}` exists because somebody's CSV had a `company`
    // column. No list in this repo could know that, which is the whole reason
    // the strip samples a recipient instead of documenting a set.
    const recipient = recipientFromRow(
      recipientRow({ id: "r1", fields: { company: "Acme Corp", plan: "gold" } }),
    );
    const tags = mergeTagsFor({ recipient, campaignName: "August", accountName: "Acme" });
    expect(tags.map((t) => t.tag)).toContain("{{fields.company}}");
    expect(tags.find((t) => t.tag === "{{fields.company}}")?.preview).toBe("Acme Corp");
    expect(tags.find((t) => t.tag === "{{fields.company}}")?.fromImport).toBe(true);
  });

  it("shows what each base tag renders to for the sampled person", () => {
    const recipient = recipientFromRow(recipientRow({ id: "r1", displayName: "Dana" }));
    const tags = mergeTagsFor({ recipient, campaignName: "August", accountName: "Acme" });
    expect(tags.find((t) => t.tag === "{{displayName}}")?.preview).toBe("Dana");
    expect(tags.find((t) => t.tag === "{{email}}")?.preview).toBe("dana@acme.com");
    expect(tags.find((t) => t.tag === "{{campaignName}}")?.preview).toBe("August");
  });

  it("keeps a fields key whose value is empty -- present-but-blank is the case a spell check misses", () => {
    const recipient = recipientFromRow(recipientRow({ id: "r1", fields: { company: "" } }));
    const tags = mergeTagsFor({ recipient, campaignName: "", accountName: "" });
    expect(tags.map((t) => t.tag)).toContain("{{fields.company}}");
    expect(tags.find((t) => t.tag === "{{fields.company}}")?.preview).toBe("");
  });

  it("inserts at the cursor and leaves the cursor AFTER the tag", () => {
    const out = insertAt("Hello , welcome", "{{displayName}}", 6, 6);
    expect(out.body).toBe("Hello {{displayName}}, welcome");
    expect(out.cursor).toBe(6 + "{{displayName}}".length);
  });

  it("replaces a selection rather than pushing it aside", () => {
    const out = insertAt("Hello NAME,", "{{displayName}}", 6, 10);
    expect(out.body).toBe("Hello {{displayName}},");
  });

  it("clamps an out-of-range selection instead of throwing the click away", () => {
    const out = insertAt("Hi", "{{email}}", 99, 200);
    expect(out.body).toBe("Hi{{email}}");
  });
});

// ---------------------------------------------------------------------------
// Recipients, deliveries, rules
// ---------------------------------------------------------------------------

describe("a roster", () => {
  it("counts only subscribed addresses as sendable", () => {
    const roster = [
      recipientRow({ id: "r1", subscriptionStatus: "subscribed" }),
      recipientRow({ id: "r2", subscriptionStatus: "unsubscribed" }),
      recipientRow({ id: "r3", subscriptionStatus: "bounced" }),
      recipientRow({ id: "r4", subscriptionStatus: "complained" }),
    ].map(recipientFromRow);
    expect(roster.length).toBe(4);
    expect(sendableCount(roster)).toBe(1);
  });

  it("keeps a non-string field value rather than dropping the key", () => {
    // The merge-tag list's whole job is to say what IS there.
    const recipient = recipientFromRow(recipientRow({ id: "r1", fields: { seats: 4 } }));
    expect(recipient.fields.seats).toBe("4");
  });
});

describe("a skipped delivery", () => {
  it("says why in the reader's words", () => {
    expect(skipReasonSentence("unsubscribed")).toBe("They unsubscribed");
    expect(skipReasonSentence("hard_bounce")).toBe("The address bounced");
  });

  it("keeps an unrecognised reason's own text rather than inventing one", () => {
    // Inventing a friendly sentence for a value this build does not know is
    // how a real cause gets mistaken for a familiar one.
    expect(skipReasonSentence("greylisted_by_relay")).toBe("greylisted_by_relay");
  });

  it("projects a delivery row", () => {
    const delivery = deliveryFromRow(deliveryRow({ id: "d1", status: "skipped", skipReason: "unsubscribed" }));
    expect(delivery.email).toBe("dana@acme.com");
    expect(delivery.status).toBe("skipped");
  });
});

describe("a rule as a sentence", () => {
  it("reads as one sentence a person could have written", () => {
    const rule = emailRuleFromRow(
      ruleRow({ id: "r1", recipientMode: "cluster_roles", recipientRoles: ["owner"] }),
    );
    expect(ruleSentence(rule, { template: "Welcome", audience: "" })).toBe(
      "When a user is created, email Welcome to owner in this cluster.",
    );
  });

  it("says the cluster owner when no roles are picked -- an empty list is an answer", () => {
    const rule = emailRuleFromRow(ruleRow({ id: "r1", recipientRoles: [] }));
    expect(ruleSentence(rule, { template: "Welcome", audience: "" })).toContain(
      "to the cluster owner",
    );
  });

  it("carries the condition as a clause rather than dropping it", () => {
    const rule = emailRuleFromRow(ruleRow({ id: "r1", condition: 'role=="admin"' }));
    expect(ruleSentence(rule, { template: "Welcome", audience: "" })).toContain(
      'but only when role=="admin"',
    );
  });

  it("names the audience for an audience rule and the field for a row-address one", () => {
    const audienceRule = emailRuleFromRow(
      ruleRow({ id: "r1", recipientMode: "audience", audienceId: "a1" }),
    );
    expect(ruleSentence(audienceRule, { template: "T", audience: "Newsletter" })).toContain(
      "everyone in Newsletter",
    );

    const rowRule = emailRuleFromRow(
      ruleRow({ id: "r2", recipientMode: "row_address", recipientField: "primaryContactEmail" }),
    );
    expect(ruleSentence(rowRule, { template: "T", audience: "" })).toContain(
      "the address in primaryContactEmail",
    );
  });

  it("reads as a half-built draft rather than collapsing when pieces are missing", () => {
    const rule = emailRuleFromRow(ruleRow({ id: "r1", triggerConcept: "", templateId: "" }));
    const sentence = ruleSentence(rule, { template: "", audience: "" });
    expect(sentence).toContain("something");
    expect(sentence).toContain("a template");
  });

  it("names a concept by its entity", () => {
    expect(conceptEntity("v1:identity:user")).toBe("user");
    expect(conceptEntity("v1:campaigns:senderIdentity")).toBe("senderIdentity");
    // A malformed id keeps itself rather than becoming an empty noun.
    expect(conceptEntity("nonsense")).toBe("nonsense");
  });
});

// ---------------------------------------------------------------------------
// The email integration's self-report
// ---------------------------------------------------------------------------

describe("reading whether this cluster can send mail", () => {
  it("says NOT configured when the report says so", () => {
    const readiness = emailReadinessFrom({
      integrations: [{ name: "email", configured: "no", health: "unknown", detail: "no sender" }],
    });
    expect(readiness.needsConfiguration).toBe(true);
    expect(readiness.detail).toBe("no sender");
  });

  it("says NOT configured for the log-only sender, which looks fine from every other angle", () => {
    // With no credentials the sender DEGRADES rather than failing: every send
    // returns success and nothing is delivered.
    const readiness = emailReadinessFrom({
      integrations: [{ name: "email", configured: "yes", health: "degraded", mode: "log" }],
    });
    expect(readiness.needsConfiguration).toBe(true);
    expect(readiness.mode).toBe("log");
  });

  it("treats SILENCE as unknown, not as a refusal", () => {
    // Warning on "unknown" would put a permanent banner on a healthy cluster.
    expect(emailReadinessFrom({}).needsConfiguration).toBe(false);
    expect(
      emailReadinessFrom({ integrations: [{ name: "storage", configured: "unknown" }] })
        .needsConfiguration,
    ).toBe(false);
    expect(
      emailReadinessFrom({ integrations: [{ name: "email", configured: "unknown", health: "unknown" }] })
        .needsConfiguration,
    ).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The engine's reply shapes, pinned against the ENGINE's own key names.
//
// These readers were written before the Go handlers existed, from the
// builtins' prose descriptions, and one of them guessed wrong: it knew
// `samples`, `invalidSamples` and `sampleInvalid` and did not know
// `invalidLines`, which is what `component/campaigns/import.go` actually
// sends. The failure had no symptom -- a non-zero invalid count beside an
// empty list of bad lines reads exactly like a clean file with a few
// duplicates, and the operator's next action is to look for a problem that is
// not there.
//
// So the fixtures below are copied from the Go source rather than invented,
// and that is the point of them: they fail the day the engine renames a key,
// which is the only moment anybody could still fix it cheaply.
// ---------------------------------------------------------------------------

describe("the engine's reply shapes", () => {
  it("reads the import report the engine actually sends", async () => {
    const { importReportFrom } = await import("../../src/apps/campaigns/actions");
    // component/campaigns/import.go: resultNode("campaignImport", {...})
    const report = importReportFrom({
      audienceId: "v1:campaigns:audience:a1",
      artifactId: "v1:library:artifact:f1",
      added: 118,
      duplicates: 4,
      invalid: 2,
      total: 124,
      invalidLines: [
        { line: 17, reason: "not an address", value: "dana(at)example.test" },
        { line: 92, reason: "no email column value", value: ",Dana,Acme" },
      ],
    } as never);

    expect(report.added).toBe(118);
    expect(report.duplicates).toBe(4);
    expect(report.invalid).toBe(2);
    expect(report.total).toBe(124);
    expect(report.samples).toHaveLength(2);
    expect(report.samples[0]?.line).toBe(17);
    expect(report.samples[0]?.reason).toBe("not an address");
    // `value` is the engine's name for the offending text. Reading only
    // `text` here would render a line number and a reason with nothing to
    // fix -- which is worse than no sample at all.
    expect(report.samples[0]?.text).toBe("dana(at)example.test");
  });

  it("reads the unresolved merge tags the engine actually sends", async () => {
    const { unresolvedTagsFrom } = await import("../../src/apps/campaigns/actions");
    // component/campaigns/test_send.go: "unresolvedTags"
    expect(unresolvedTagsFrom({ unresolvedTags: ["{{fields.compnay}}"] } as never)).toEqual([
      "{{fields.compnay}}",
    ]);
    // A reply that says nothing is a clean test, never an error about a
    // shape we did not understand.
    expect(unresolvedTagsFrom({} as never)).toEqual([]);
    expect(unresolvedTagsFrom(null)).toEqual([]);
  });
});
