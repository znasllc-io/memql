import { describe, expect, it } from "vitest";
import {
  REASONS,
  WRITER_ASKED,
  decisionLogFrom,
  reasonsFor,
  reviewDetailFrom,
  reviewName,
  reviewPageFrom,
  reviewPeopleFrom,
  reviewSettingsFrom,
  waitedFor,
} from "../../src/apps/deployables/reviews/model";

// How the Reviews section reads the reviews pack's answers
// (src/apps/deployables/reviews/model.ts).

const review = {
  id: "v1:reviews:review:r1", state: "waiting", productHandle: "floss-picks", title: "Easy", body: "Easy to use",
  rating: 4, authorName: "Dana", authorEmail: "dana@example.com", verifiedBuyer: true, revision: 2,
  contentHash: "abc", createdAt: "2026-11-01T12:00:00Z",
  lastDecision: { action: "askToFix", reason: "personal_information", note: "Phone number", decidedBy: "u-1", at: "2026-11-02T09:00:00Z" },
};

describe("the reviews pack's answers", () => {
  it("reads a page of the moderation list with every filter's count", () => {
    const page = reviewPageFrom({ reviews: [review], total: 1, next: "50", counts: { waiting: 1, published: 3, all: 4 } });
    expect(page.reviews).toHaveLength(1);
    expect(page.reviews[0]!).toMatchObject({ state: "waiting", rating: 4, verifiedBuyer: true, revision: 2, contentHash: "abc" });
    expect(page.reviews[0]!.lastDecision).toMatchObject({ action: "askToFix", reason: "personal_information", decidedBy: "u-1" });
    expect(page).toMatchObject({ total: 1, next: "50", counts: { waiting: 1, published: 3, all: 4 } });
  });

  it("reads nested values that arrive as JSON text, the way a builtin's payload can", () => {
    const page = reviewPageFrom({ reviews: JSON.stringify([review]), total: "1", counts: JSON.stringify({ waiting: 1 }) });
    expect(page.reviews[0]!.id).toBe("v1:reviews:review:r1");
    expect(page.total).toBe(1);
    expect(page.counts.waiting).toBe(1);
  });

  it("keeps only a rating of 1 to 5, and an unknown state is waiting", () => {
    const page = reviewPageFrom({ reviews: [{ ...review, rating: 9, state: "hidden" }] });
    expect(page.reviews[0]!.rating).toBe(0);
    expect(page.reviews[0]!.state).toBe("waiting");
  });

  it("reads one review with its log, and only acts it knows", () => {
    const detail = reviewDetailFrom({
      review,
      decisions: [{ id: "d2", action: "publish", decidedBy: "u-2", at: "2026-11-03T10:00:00Z", revision: 2 }, { id: "d1", action: "askToFix", reason: "personal_information", revision: 1 }],
      acts: ["escalate", "remove", "hide"],
      moderates: true,
    });
    expect(detail.decisions.map((d) => d.id)).toEqual(["d2", "d1"]);
    expect(detail.acts).toEqual(["escalate", "remove"]);
    expect(detail.moderates).toBe(true);
  });

  it("reads the store's decision log with the review each entry was about", () => {
    const log = decisionLogFrom({ decisions: [{ id: "d1", reviewId: "v1:reviews:review:r1", action: "decline", reason: "profanity", reviewState: "declined", productHandle: "floss-picks", title: "Easy" }], total: 1, next: "" });
    expect(log.entries[0]).toMatchObject({ action: "decline", reviewState: "declined", productHandle: "floss-picks", title: "Easy" });
  });

  it("reads the settings in force, defaults included", () => {
    const none = reviewSettingsFrom({ exists: false, settings: { linkLifetimeDays: 14, requestDelayDays: 0 }, may: { edit: false }, viewer: { moderates: true } });
    expect(none.exists).toBe(false);
    expect(none.values.requestDelayDays).toBe(0);
    expect(none.may.edit).toBe(false);
    expect(none.viewer.moderates).toBe(true);
    const defaults = reviewSettingsFrom({ exists: true, settings: {} });
    expect(defaults.values).toMatchObject({ linkLifetimeDays: 14, requestDelayDays: 14, publicDisplay: false, requestsEnabled: false });
  });
});

describe("who can moderate", () => {
  it("reads the store's people in the engine's order, parsed or as JSON text", () => {
    const people = [
      { userId: "u-owner", displayName: "Avery Owner", email: "avery@example.com", mayRemove: true },
      { userId: "u-mod", displayName: "", email: "robin@example.com", mayRemove: "false" },
      { userId: "", displayName: "Nobody", email: "", mayRemove: false },
    ];
    for (const answer of [{ storeId: "store-live", people }, { storeId: "store-live", people: JSON.stringify(people) }]) {
      expect(reviewPeopleFrom(answer)).toEqual([
        { userId: "u-owner", displayName: "Avery Owner", email: "avery@example.com", mayRemove: true },
        { userId: "u-mod", displayName: "", email: "robin@example.com", mayRemove: false },
      ]);
    }
    expect(reviewPeopleFrom(null)).toEqual([]);
  });
});

describe("the words", () => {
  it("offers the eleven reasons to decline or ask to fix, and the writer's own request only to remove", () => {
    expect(REASONS).toHaveLength(11);
    expect(reasonsFor("decline")).toEqual(REASONS);
    expect(reasonsFor("askToFix")).toEqual(REASONS);
    expect(reasonsFor("remove")).toContain(WRITER_ASKED);
    expect(reasonsFor("decline")).not.toContain(WRITER_ASKED);
    expect(reasonsFor("publish")).toEqual([]);
  });

  it("names a review by its headline, else the start of its words", () => {
    expect(reviewName({ title: "Easy", body: "x", state: "waiting" })).toBe("Easy");
    expect(reviewName({ title: "", body: "Easy to use", state: "waiting" })).toBe("Easy to use");
    expect(reviewName({ title: "", body: "a".repeat(80), state: "waiting" })).toHaveLength(60);
    expect(reviewName({ title: "", body: "", state: "erased" })).toBe("Erased on request");
  });

  it("says how long a review has waited", () => {
    const now = new Date("2026-11-04T12:00:00Z");
    expect(waitedFor("2026-11-04T11:58:00Z", now)).toBe("a few minutes");
    expect(waitedFor("2026-11-04T11:00:00Z", now)).toBe("1 hour");
    expect(waitedFor("2026-11-03T00:00:00Z", now)).toBe("36 hours");
    expect(waitedFor("2026-11-01T12:00:00Z", now)).toBe("3 days");
    expect(waitedFor("not a time", now)).toBe("");
  });
});
