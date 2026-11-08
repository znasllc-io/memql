import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBar, useFakeDate } from "../readTheBar";
import { seededAccessWithout } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection hook is replaced, as in accessCodes.test.tsx; the fake beneath
// answers at `executeNamed`, so the call STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { ReviewsSection } from "../../src/apps/deployables/reviews/ReviewsSection";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, siteRow, type as typeInto, withSession, type FakeConnection, type FakeSeed } from "./harness";

// A STORE'S REVIEWS, AS A SECTION OF ITS DEPLOYABLE (apps/deployables/reviews).
//
// What this surface can get wrong, in the order it would hurt: a decision on
// words nobody read (so every decision names the revision and the fingerprint
// back); an act the engine would refuse (so only the acts reviewsGet answers
// are offered, and a reason act has no button until a reason is chosen); a
// review shown or hidden by the panel's own reckoning (so every state is the
// engine's word); and a dialog where the owner wants the page to change.

const NOW = Date.parse("2026-11-04T12:00:00.000Z");
const HOST = "shop.memql.example.com";
const SHOP = siteRow({ id: "site-shop", hostname: HOST, kind: "shopify_storefront", binding: { storeId: "store-live" }, packageId: "pkg-shop", packageDeployableName: "storefront" });

const REVIEW = {
  id: "v1:reviews:review:r1", state: "waiting", productHandle: "floss-picks", title: "Easy to use", body: "Gentle and easy to use.",
  rating: 4, authorName: "Dana", authorEmail: "dana@example.com", verifiedBuyer: true, revision: 2, contentHash: "hash-r1",
  createdAt: "2026-11-01T12:00:00Z",
};
const OLDER = { ...REVIEW, id: "v1:reviews:review:r0", title: "Works", createdAt: "2026-10-30T12:00:00Z", revision: 1, contentHash: "hash-r0" };

function reviewsSeed(over: Partial<NonNullable<FakeSeed["reviews"]>> = {}): FakeSeed {
  return {
    sites: [SHOP],
    reviews: {
      list: { waiting: { reviews: [OLDER, REVIEW], total: 2, next: "", counts: { waiting: 2, published: 5, declined: 1, removed: 0, all: 8 } } as unknown as Row },
      get: {
        [REVIEW.id]: {
          review: REVIEW,
          decisions: [{ id: "d1", reviewId: REVIEW.id, action: "askToFix", reason: "personal_information", note: "Phone number in it", decidedBy: "u-me", at: "2026-11-02T09:00:00Z", revision: 1 }],
          acts: ["publish", "decline", "askToFix", "escalate"],
          moderates: true,
        } as unknown as Row,
      },
      settings: { exists: true, settings: { linkLifetimeDays: 14, requestDelayDays: 14, requestsEnabled: true, notifyEmail: "support@example.com", removalApproverUserId: "u-owner" }, may: { edit: true, nameApprover: false }, viewer: { moderates: true, approvesRemovals: false } } as unknown as Row,
      ...over,
    },
  };
}

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

async function openDeployable(seed: FakeSeed, session: Parameters<typeof withSession>[1] = {}): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(seed);
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...session }));
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(HOST)).closest("button"));
  return { connection, page: await screen.findByRole("region", { name: `Deployable ${HOST}` }) };
}

async function openReviews(seed: FakeSeed = reviewsSeed()): Promise<FakeConnection> {
  const { connection, page } = await openDeployable(seed);
  await click(within(page).getByRole("button", { name: "Reviews" }));
  await screen.findByRole("heading", { name: "Reviews" });
  return connection;
}

const bar = () => document.querySelector(".os-actbar") as HTMLElement;
const barButtons = () => within(bar()).queryAllByRole("button").map((b) => (b.textContent ?? "").trim()).filter((t) => t !== "");

/** One row of the review settings, by the name beside it. */
function settingRow(label: string): HTMLElement {
  const row = [...document.querySelectorAll(".os-setting-row")].find((r) => r.querySelector(".os-setting-name")?.textContent === label);
  if (row === undefined) throw new Error(`no setting row named ${label}`);
  return row as HTMLElement;
}

useFakeDate();

beforeEach(() => {
  vi.setSystemTime(NOW);
  localStorage.clear();
  sessionStorage.clear();
  h.connection = null;
});

describe("the Reviews icon", () => {
  it("sits after Traffic on a storefront and opens the queue on a page of its own", async () => {
    const { page } = await openDeployable(reviewsSeed());
    const icon = within(page).getByRole("button", { name: "Reviews" });
    await click(icon);
    expect(await screen.findByRole("heading", { name: "Reviews" })).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("region", { name: `Deployable ${HOST}` })).toBeNull();
  });

  it("is absent without the reviews part", async () => {
    const { page } = await openDeployable(reviewsSeed(), { role: "developer", capabilities: seededAccessWithout("developer", "app:deployables/reviews") });
    expect(within(page).getByRole("button", { name: "Traffic" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Reviews" })).toBeNull();
  });

  it("is absent on a deployable that fronts no store", async () => {
    const plain = siteRow({ id: "site-plain", hostname: HOST, kind: "spa" });
    const { page } = await openDeployable({ ...reviewsSeed(), sites: [plain] });
    expect(within(page).queryByRole("button", { name: "Reviews" })).toBeNull();
  });
});

const tabs = () => within(screen.getByRole("navigation", { name: "Review views" })).getAllByRole("button");
const tab = (name: string) => within(screen.getByRole("navigation", { name: "Review views" })).getByRole("button", { name });

describe("the review views", () => {
  it("are tabs under the Reviews heading, the way Fleet shows a machine's, with Waiting open first", async () => {
    await openReviews();
    expect(tabs().map((t) => t.textContent)).toEqual(["Waiting", "Published", "Decision log", "Settings"]);
    expect(tab("Waiting").getAttribute("aria-current")).toBe("page");
    // The views are tabs now: no icon in the heading opens them, and no filter row stands over the list.
    expect(screen.queryByRole("button", { name: "Review settings" })).toBeNull();
    expect(screen.queryByRole("radio")).toBeNull();
  });
});

describe("the queue", () => {
  it("lists what waits, oldest first as the engine answered, with how long each has waited", async () => {
    const connection = await openReviews();
    expect(connection.callsNamed("reviewsList")[0]).toContain('storeId: "store-live"');
    expect(connection.callsNamed("reviewsList")[0]).toContain('siteId: "site-shop"');
    expect(connection.callsNamed("reviewsList")[0]).toContain('state: "waiting"');
    const list = await screen.findByRole("list", { name: "Waiting reviews" });
    const rows = within(list).getAllByRole("button");
    expect(rows.map((r) => r.getAttribute("aria-label"))).toEqual(["Open review Works", "Open review Easy to use"]);
    expect(within(list).getByText("5 days")).toBeTruthy();
    expect(within(list).getByText("3 days")).toBeTruthy();
    expect(within(list).getAllByText("4 of 5")).toHaveLength(2);
    expect(within(list).getAllByText("Dana, verified buyer")).toHaveLength(2);
    // The tab says Waiting once; no row says it again.
    expect(within(list).queryByText("Waiting")).toBeNull();
    expect(bar().textContent).toContain("2 waiting");
  });

  it("shows what is published on its own tab, and says so when nothing is", async () => {
    const connection = await openReviews();
    await click(tab("Published"));
    await waitFor(() => expect(connection.callsNamed("reviewsList").some((c) => c.includes('state: "published"'))).toBe(true));
    expect(tab("Published").getAttribute("aria-current")).toBe("page");
    const empty = await screen.findByRole("region", { name: "Nothing published" });
    expect(within(empty).getByText(/A review appears here once somebody publishes it/)).toBeTruthy();
    expect(bar().textContent).toContain("Nothing published");
  });

  it("tells the cluster's operators they read the reviews and the store's people decide them", async () => {
    const seed = reviewsSeed({ settings: { exists: true, settings: {}, may: { edit: true, nameApprover: true }, viewer: { moderates: false, approvesRemovals: false } } as unknown as Row });
    await openReviews(seed);
    expect(await screen.findByText(/Its own people publish and decline them/)).toBeTruthy();
  });
});

describe("one review", () => {
  async function openReview(seed: FakeSeed = reviewsSeed()): Promise<FakeConnection> {
    const connection = await openReviews(seed);
    await click(await screen.findByRole("button", { name: "Open review Easy to use" }));
    await screen.findByRole("heading", { name: "Easy to use" });
    return connection;
  }

  it("shows the words, who wrote them and every decision, and offers only the acts the engine answered", async () => {
    await openReview();
    expect(screen.getByText("Gentle and easy to use.")).toBeTruthy();
    expect(screen.getByText("dana@example.com")).toBeTruthy();
    expect(screen.getByText("Asked to fix: Personal information")).toBeTruthy();
    expect(screen.getByText("Phone number in it")).toBeTruthy();
    // One button -- Publish -- and every other act a text label beside it.
    expect(barButtons()).toEqual(["Decline", "Ask to fix", "Record a safety concern", "Publish"]);
    expect(within(bar()).queryByText("Remove")).toBeNull();
    expect(within(bar()).queryByText("Erase the writer's data")).toBeNull();
  });

  it("publishes naming back the revision and the fingerprint of the words on screen", async () => {
    const connection = await openReview();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Publish" }));
    await waitFor(() => expect(connection.callsNamed("reviewsDecide")).toHaveLength(1));
    const call = connection.callsNamed("reviewsDecide")[0];
    expect(call).toContain(`reviewId: "${REVIEW.id}"`);
    expect(call).toContain('action: "publish"');
    expect(call).toContain("revision: 2");
    expect(call).toContain('contentHash: "hash-r1"');
    expect(call).not.toContain("reason:");
  });

  it("asks for a reason before a decline, and offers Decline only once one is chosen", async () => {
    const connection = await openReview();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Decline" }));
    expect(await screen.findByText("Which reason applies?")).toBeTruthy();
    // The criteria's own guidance stands beside the reasons: one standard for
    // every rating, criticism is publishable, and a doubt is decided by publishing.
    const guidance = screen.getByText(/The same standards apply to every star rating/);
    expect(guidance.textContent).toMatch(/a low rating, a harsh tone or a bad experience is never a reason/);
    expect(guidance.textContent).toMatch(/taste, results, packaging, shipping, price or service/);
    expect(guidance.textContent).toMatch(/When in doubt, publish\./);
    // Profanity is a person's reading, never a match: whole words, offensive use, context.
    expect(screen.getByText(/used offensively: whole words and phrases only, never letters inside a harmless word/)).toBeTruthy();
    expect(screen.getAllByRole("radio")).toHaveLength(11);
    expect(barButtons()).toEqual(["Cancel"]);

    await click(screen.getByRole("radio", { name: /^Profanity/ }));
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Note"), { target: { value: "Two listed words" } });
    });
    await readTheBar();
    expect(barButtons()).toEqual(["Cancel", "Decline"]);
    await click(within(bar()).getByRole("button", { name: "Decline" }));
    await waitFor(() => expect(connection.callsNamed("reviewsDecide")).toHaveLength(1));
    const call = connection.callsNamed("reviewsDecide")[0];
    expect(call).toContain('action: "decline"');
    expect(call).toContain('reason: "profanity"');
    expect(call).toContain('note: "Two listed words"');
  });

  it("keeps the step open and says why when the engine refuses", async () => {
    await openReview(reviewsSeed({ decideError: "reviews: the review changed since revision 2; read it again" }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Decline" }));
    await click(screen.getByRole("radio", { name: /^Profanity/ }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Decline" }));
    expect(await screen.findByText(/the review changed since revision 2/)).toBeTruthy();
    expect(screen.getByText("Which reason applies?")).toBeTruthy();
  });

  it("offers the store's owner Remove and Erase on a published review, and the writer's own request as a reason", async () => {
    const published = { ...REVIEW, state: "published" };
    const seed = reviewsSeed({
      get: { [REVIEW.id]: { review: published, decisions: [], acts: ["escalate", "remove", "erase"], moderates: true } as unknown as Row },
    });
    await openReview(seed);
    expect(barButtons()).toEqual(["Record a safety concern", "Erase the writer's data", "Remove"]);
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Why is it coming down?")).toBeTruthy();
    expect(screen.getAllByRole("radio")).toHaveLength(12);
    expect(screen.getByRole("radio", { name: /^The writer asked/ })).toBeTruthy();
  });
});

describe("the decision log", () => {
  it("lists every decision newest first and opens its review", async () => {
    const seed = reviewsSeed({
      log: { decisions: [{ id: "d9", reviewId: REVIEW.id, action: "decline", reason: "profanity", decidedBy: "u-me", at: "2026-11-03T10:00:00Z", revision: 2, reviewState: "declined", productHandle: "floss-picks", title: "Easy to use" }], total: 1, next: "" } as unknown as Row,
    });
    const connection = await openReviews(seed);
    await click(tab("Decision log"));
    expect(tab("Decision log").getAttribute("aria-current")).toBe("page");
    await waitFor(() => expect(connection.callsNamed("reviewsDecisionLog")[0]).toContain('storeId: "store-live"'));
    const entry = await screen.findByRole("button", { name: "Open the review: Declined: Profanity" });
    expect(within(entry).getByText("by you")).toBeTruthy();
    expect(within(entry).getByText("“Easy to use” on floss-picks")).toBeTruthy();
    // A decision's row says what was decided; the review's own state is said on the review.
    expect(within(entry).queryByText("Declined")).toBeNull();
    await click(entry);
    expect(await screen.findByRole("heading", { name: "Easy to use" })).toBeTruthy();
  });
});

describe("the settings", () => {
  it("are the last tab, and offer the store's export", async () => {
    // The file is handed over the way a download link would; jsdom has no
    // object URLs and navigates nowhere, so both are stood in for here.
    const original = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
    const created = vi.fn(() => "blob:reviews");
    URL.createObjectURL = created;
    URL.revokeObjectURL = vi.fn();
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
    try {
      const connection = await openReviews();
      await click(tab("Settings"));
      await click(await screen.findByRole("button", { name: "Export reviews" }));
      await waitFor(() => expect(connection.callsNamed("reviewsExportStore")).toHaveLength(1));
      expect(connection.callsNamed("reviewsExportStore")[0]).toContain('storeId: "store-live"');
      await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
      expect(clicked).toHaveBeenCalledTimes(1);
    } finally {
      URL.createObjectURL = original.create;
      URL.revokeObjectURL = original.revoke;
      clicked.mockRestore();
    }
  });

  it("shows what is in force and saves only what changed", async () => {
    // Somebody can moderate the store, so a choice that is absent is absent
    // because this viewer may not name the approver -- not because there is
    // nobody to choose from.
    const people = { storeId: "store-live", people: [{ userId: "u-owner", displayName: "Avery Owner", email: "avery@example.com", mayRemove: true }] } as unknown as Row;
    const connection = await openReviews(reviewsSeed({ people }));
    await click(tab("Settings"));
    const lifetime = (await screen.findByLabelText("Days a review link stays open")) as HTMLInputElement;
    expect(lifetime.value).toBe("14");
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    // Only the owner, or an operator, names who approves removals: to anybody
    // else the choice is absent, never disabled, and so is clearing it.
    expect(screen.queryByRole("combobox", { name: "Choose who approves removals" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Clear the approver/ })).toBeNull();
    // Somebody is named, so who else may name someone is that owner or an operator.
    expect(within(settingRow("Removal approver")).getByText(/Only that owner, or the cluster's operators, can name someone else\.$/)).toBeTruthy();
    expect(barButtons()).toEqual([]);

    await typeInto(lifetime, "21");
    await readTheBar();
    expect(barButtons()).toEqual(["Discard", "Save"]);
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("reviewsUpdateSettings")).toHaveLength(1));
    const call = connection.callsNamed("reviewsUpdateSettings")[0];
    expect(call).toContain("linkLifetimeDays: 21");
    expect(call).not.toContain("notifyEmail");
    expect(call).not.toContain("publicDisplay");
  });

  it("sends a setting emptied as empty, never leaves it out", async () => {
    // The engine changes only the settings a call names: an emptied address
    // left out of the call would stay as it was.
    const connection = await openReviews();
    await click(tab("Settings"));
    await typeInto((await screen.findByLabelText("Email when a review is waiting")) as HTMLInputElement, "");
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("reviewsUpdateSettings")).toHaveLength(1));
    const call = connection.callsNamed("reviewsUpdateSettings")[0];
    expect(call).toContain('notifyEmail: ""');
    expect(call).not.toContain("linkLifetimeDays");
  });

  it("lets only the cluster's operators name the mailbox requests leave from", async () => {
    // A moderator sees the sender and cannot change it.
    await openReviews();
    await click(tab("Settings"));
    expect(((await screen.findByLabelText("Request sender")) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText(/Only the cluster.s operators choose the mailbox/)).toBeTruthy();
  });

  it("saves the sender an operator names", async () => {
    const seed = reviewsSeed({ settings: { exists: true, settings: { linkLifetimeDays: 14, requestDelayDays: 14 }, may: { edit: true, nameApprover: true, nameSender: true }, viewer: { moderates: false, approvesRemovals: false } } as unknown as Row });
    const connection = await openReviews(seed);
    await click(tab("Settings"));
    const sender = (await screen.findByLabelText("Request sender")) as HTMLInputElement;
    expect(sender.disabled).toBe(false);
    await typeInto(sender, "si-house");
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("reviewsUpdateSettings")).toHaveLength(1));
    expect(connection.callsNamed("reviewsUpdateSettings")[0]).toContain('requestSenderIdentityId: "si-house"');
  });

  it("offers no Save for days out of range, and says what is wrong", async () => {
    await openReviews();
    await click(tab("Settings"));
    await typeInto((await screen.findByLabelText("Days a review link stays open")) as HTMLInputElement, "0");
    expect(await screen.findByText(/a whole number from 1 to 365/)).toBeTruthy();
    expect(barButtons()).toEqual(["Discard"]);
  });
});

describe("who can moderate", () => {
  // THE REMOVAL APPROVER IS ONE OF THE STORE'S PEOPLE WHO CAN MODERATE IT
  // (reviewsPeople): chosen from them in a picker, never typed as an id, and
  // the settings say who they are -- in words, on a list of everybody who can
  // moderate the store.
  const PEOPLE = {
    storeId: "store-live",
    people: [
      { userId: "u-owner", displayName: "Avery Owner", email: "avery@example.com", mayRemove: true },
      { userId: "u-mod", displayName: "Robin Moderator", email: "robin@example.com", mayRemove: false },
    ],
  } as unknown as Row;
  /**
   * The settings as an operator reads them, with Avery named: they may name
   * the approver and the sender, and approve nothing themselves -- the engine
   * answers `approvesRemovals` only to the person named, and never to an
   * operator (packs/reviewspack/reads.go, readSettings).
   */
  const APPROVER_SETTINGS = {
    exists: true,
    settings: { linkLifetimeDays: 14, requestDelayDays: 14, notifyEmail: "support@example.com", removalApproverUserId: "u-owner" },
    may: { edit: true, nameApprover: true, nameSender: true },
    viewer: { moderates: false, approvesRemovals: false },
  } as unknown as Row;
  /** The first 100 people who can moderate, by what each is called: the list the engine cuts at. */
  const FIRST_HUNDRED = Array.from({ length: 100 }, (_, n) => {
    const at = String(n).padStart(3, "0");
    return { userId: `u-${at}`, displayName: `Moderator ${at}`, email: `m${at}@example.com`, mayRemove: false };
  });

  /** Type into the approver picker, and what it shows. */
  async function search(text: string): Promise<HTMLElement> {
    const field = await screen.findByRole("combobox", { name: "Choose who approves removals" });
    fireEvent.focus(field);
    fireEvent.change(field, { target: { value: text } });
    return screen.findByRole("listbox", { name: "People who can moderate" });
  }

  it("review settings pick the approver from the people who can moderate", async () => {
    const connection = await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: PEOPLE }));
    await click(tab("Settings"));
    const moderators = await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    expect(connection.callsNamed("reviewsPeople")[0]).toContain('storeId: "store-live"');
    expect(connection.callsNamed("reviewsPeople")[0]).toContain('siteId: "site-shop"');
    const rows = within(moderators).getAllByRole("listitem");
    expect(rows.map((r) => r.querySelector(".os-row-name")?.textContent)).toEqual(["Avery Owner", "Robin Moderator"]);
    // Who approves removals is said in words on their row: no chip, no badge.
    expect(within(rows[0]!).getByText("Approves removals")).toBeTruthy();
    expect(within(rows[1]!).queryByText("Approves removals")).toBeNull();
    expect(within(rows[1]!).getByText("robin@example.com")).toBeTruthy();
    // Everybody who can moderate is listed, so the list says nothing about a cut.
    expect(within(settingRow("Who can moderate")).queryByText(/Only the first/)).toBeNull();
    // The address a waiting review is sent to stays a field.
    expect((screen.getByLabelText("Email when a review is waiting") as HTMLInputElement).value).toBe("support@example.com");
    expect(screen.getByText(/Avery Owner is the one person who can remove a published review/)).toBeTruthy();

    const field = screen.getByRole("combobox", { name: "Choose who approves removals" });
    fireEvent.focus(field);
    const choices = await screen.findByRole("listbox", { name: "People who can moderate" });
    expect(within(choices).getAllByRole("option").map((o) => o.getAttribute("aria-label"))).toEqual(["Avery Owner", "Robin Moderator"]);
    await click(within(choices).getByRole("option", { name: "Robin Moderator" }));
    expect(screen.queryByRole("listbox", { name: "People who can moderate" })).toBeNull();
    expect(screen.getByText(/Robin Moderator is the one person who can remove a published review/)).toBeTruthy();

    await readTheBar();
    expect(barButtons()).toEqual(["Discard", "Save"]);
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("reviewsUpdateSettings")).toHaveLength(1));
    const call = connection.callsNamed("reviewsUpdateSettings")[0];
    expect(call).toContain('removalApproverUserId: "u-mod"');
    expect(call).not.toContain("notifyEmail");
  });

  it("say so when the engine refuses the approver chosen", async () => {
    const refusal = "reviews: Choose someone in this store's organization who can moderate reviews.";
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: PEOPLE, settingsError: refusal }));
    await click(tab("Settings"));
    fireEvent.focus(await screen.findByRole("combobox", { name: "Choose who approves removals" }));
    await click(within(await screen.findByRole("listbox", { name: "People who can moderate" })).getByRole("option", { name: "Robin Moderator" }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Choose someone in this store's organization who can moderate reviews.")).toBeTruthy();
  });

  it("say nobody can moderate when the store's organization has nobody who can", async () => {
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: { storeId: "store-live", people: [] } as unknown as Row }));
    await click(tab("Settings"));
    expect(await screen.findByText(/Nobody in this store.s organization can moderate its reviews\./)).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Who can moderate this store's reviews" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Choose who approves removals" })).toBeNull();
    // An approver is still named, and nobody is listed to point at.
    const approver = settingRow("Removal approver");
    expect(within(approver).queryByText(/not among the people listed/)).toBeNull();
    expect(within(approver).getByText(/Someone this page cannot name is the one person who can remove a published review/)).toBeTruthy();
  });

  it("clear the approver, so Save names nobody", async () => {
    const connection = await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: PEOPLE }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    // A quiet kit button in the approver's row, named by the words it shows.
    const clear = within(settingRow("Removal approver")).getByRole("button", { name: /Clear the approver/ });
    expect(clear.textContent).toBe("Clear the approver");
    expect(clear.classList.contains("os-button")).toBe(true);
    expect(clear.getAttribute("data-tone")).toBe("quiet");

    await click(clear);
    expect(within(settingRow("Removal approver")).getByText(/Nobody can remove a published review until an owner is named\./)).toBeTruthy();
    // Nobody is named now, so there is nobody to clear; somebody can still be chosen.
    expect(screen.queryByRole("button", { name: /Clear the approver/ })).toBeNull();
    expect(screen.getByRole("combobox", { name: "Choose who approves removals" })).toBeTruthy();

    await readTheBar();
    expect(barButtons()).toEqual(["Discard", "Save"]);
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("reviewsUpdateSettings")).toHaveLength(1));
    const call = connection.callsNamed("reviewsUpdateSettings")[0];
    // Named as nobody, never left out: a setting that is not sent is not changed.
    expect(call).toContain('removalApproverUserId: ""');
    expect(call).not.toContain("notifyEmail");
  });

  it("say beside Clear, to the approver who is no operator, that clearing leaves the naming to the cluster's operators", async () => {
    // Only the current approver or an operator names one (writeSettings): an
    // approver who saves nobody can never name anybody again.
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "u-me" },
      may: { edit: true, nameApprover: true, nameSender: false },
      viewer: { moderates: true, approvesRemovals: true },
    } as unknown as Row;
    const people = {
      storeId: "store-live",
      people: [
        { userId: "u-me", displayName: "Sam Me", email: "sam@example.com", mayRemove: true },
        { userId: "u-mod", displayName: "Robin Moderator", email: "robin@example.com", mayRemove: false },
      ],
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    const approver = settingRow("Removal approver");
    expect(within(approver).getByText(/^You are the one person who can remove a published review/)).toBeTruthy();
    const clear = within(approver).getByRole("button", { name: "Clear the approver" });
    const after = within(approver).getByText(/^Once you clear the approver and save, only the cluster.s operators can name someone\.$/);
    expect(after.classList.contains("os-caption")).toBe(true);
    // Beside the act it is about.
    expect(clear.nextElementSibling).toBe(after);
  });

  it("keep saying, once Clear is pressed, what saving leaves, on the line focus moves to", async () => {
    // Clear goes once nobody is named, and the sentence beside it with it --
    // but Save has still to do what it said. So the line that now says nobody
    // approves says it too, and takes the focus the gone button had.
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "u-me" },
      may: { edit: true, nameApprover: true, nameSender: false },
      viewer: { moderates: true, approvesRemovals: true },
    } as unknown as Row;
    const people = {
      storeId: "store-live",
      people: [
        { userId: "u-me", displayName: "Sam Me", email: "sam@example.com", mayRemove: true },
        { userId: "u-mod", displayName: "Robin Moderator", email: "robin@example.com", mayRemove: false },
      ],
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    const approver = settingRow("Removal approver");
    const clear = within(approver).getByRole("button", { name: "Clear the approver" });
    clear.focus();
    await click(clear);

    expect(within(approver).queryByRole("button", { name: "Clear the approver" })).toBeNull();
    const said = "Nobody can remove a published review until an owner is named. Once you save, only the cluster's operators can name someone.";
    const line = within(approver).getByText(said);
    expect(line.classList.contains("os-caption")).toBe(true);
    // Focus is on that line, never left on the page itself.
    const focused = document.activeElement as HTMLElement;
    expect(approver.contains(focused)).toBe(true);
    expect(focused.textContent).toBe(said);
    // And it is still said when Save is pressed.
    await readTheBar();
    expect(barButtons()).toEqual(["Discard", "Save"]);
    expect(within(approver).getByText(said)).toBe(line);
  });

  it("move focus to the line that says nobody approves once an operator presses Clear, and say nothing of operators naming", async () => {
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: PEOPLE }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    const approver = settingRow("Removal approver");
    const clear = within(approver).getByRole("button", { name: "Clear the approver" });
    clear.focus();
    await click(clear);
    const said = "Nobody can remove a published review until an owner is named.";
    const focused = document.activeElement as HTMLElement;
    expect(approver.contains(focused)).toBe(true);
    expect(focused.textContent).toBe(said);
  });

  it("say nothing of the kind beside Clear to an operator, who can always name someone", async () => {
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "u-owner" },
      may: { edit: true, nameApprover: true, nameSender: true },
      viewer: { moderates: false, approvesRemovals: false },
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people: PEOPLE }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    const approver = settingRow("Removal approver");
    expect(within(approver).getByRole("button", { name: "Clear the approver" })).toBeTruthy();
    expect(within(approver).queryByText(/only the cluster.s operators can name/)).toBeNull();
  });

  it("say only the cluster's operators can name an approver while nobody is named, to whoever may not", async () => {
    // There is no owner to point at: nobody is named, and only an operator may
    // name somebody where nobody is.
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "" },
      may: { edit: true, nameApprover: false },
      viewer: { moderates: true, approvesRemovals: false },
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people: PEOPLE }));
    await click(tab("Settings"));
    await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    const approver = settingRow("Removal approver");
    expect(within(approver).getByText("Nobody can remove a published review until an owner is named. Only the cluster's operators can name one.")).toBeTruthy();
    expect(within(approver).queryByText(/Only that owner/)).toBeNull();
    expect(within(approver).queryByRole("combobox", { name: "Choose who approves removals" })).toBeNull();
  });

  it("offer no clearing while nobody approves removals", async () => {
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "" },
      may: { edit: true, nameApprover: true },
      viewer: { moderates: false, approvesRemovals: false },
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people: PEOPLE }));
    await click(tab("Settings"));
    await screen.findByRole("combobox", { name: "Choose who approves removals" });
    expect(within(settingRow("Removal approver")).getByText(/Nobody can remove a published review until an owner is named\./)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Clear the approver/ })).toBeNull();
  });

  it("say why nobody can be chosen when who can moderate could not be read", async () => {
    const connection = await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, peopleError: "reviews: reading the organization's people: the connection to the database dropped" }));
    await click(tab("Settings"));
    const approver = settingRow("Removal approver");
    const why = await within(approver).findByText("Nobody can be chosen, because who can moderate this store could not be read.");
    expect(why.classList.contains("os-caption")).toBe(true);
    expect(within(approver).queryByRole("combobox", { name: "Choose who approves removals" })).toBeNull();
    // An approver is still named. A list that could not be read lists nobody:
    // it says nothing about who they are, and never that there is nobody.
    expect(within(approver).queryByText(/Nobody can remove a published review/)).toBeNull();
    expect(within(approver).queryByText(/not among the people listed/)).toBeNull();
    expect(within(approver).getByText(/Someone this page cannot name is the one person who can remove a published review/)).toBeTruthy();

    // The Who can moderate row keeps its notice, and Try again asks again.
    const moderate = settingRow("Who can moderate");
    expect(within(moderate).getByText("Who can moderate this store could not be read.")).toBeTruthy();
    const reads = connection.callsNamed("reviewsPeople").length;
    await click(within(moderate).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("reviewsPeople")).toHaveLength(reads + 1));
  });

  it("draw who approves as the shape of a name while who can moderate is read, never a sentence about them", async () => {
    // The settings name Avery, whom this session's roster cannot name: until
    // the people who can moderate are read, nothing on hand says who she is.
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, peoplePending: true }));
    await click(tab("Settings"));
    await screen.findByLabelText("Days a review link stays open");
    const approver = settingRow("Removal approver");
    expect(within(approver).queryByText(/not among the people listed/)).toBeNull();
    expect(within(approver).queryByText(/cannot name/)).toBeNull();
    expect(within(approver).queryByText(/Nobody can remove a published review/)).toBeNull();
    const reading = within(approver).getByText("Reading who approves removals");
    expect(reading.closest('[aria-busy="true"]')).not.toBeNull();
  });

  it("say when only the first people by name are listed, and never that nobody approves", async () => {
    // The engine lists the first 100 by what each is called. Zoe approves
    // removals and sorts past the cut, so she is on no row.
    const settings = {
      exists: true,
      settings: { linkLifetimeDays: 14, requestDelayDays: 14, removalApproverUserId: "u-zoe" },
      may: { edit: true, nameApprover: true },
      viewer: { moderates: true, approvesRemovals: false },
    } as unknown as Row;
    await openReviews(reviewsSeed({ settings, people: { storeId: "store-live", people: FIRST_HUNDRED, truncated: true } as unknown as Row }));
    await click(tab("Settings"));
    const list = await screen.findByRole("list", { name: "Who can moderate this store's reviews" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(100);
    const cut = within(settingRow("Who can moderate")).getByText("Only the first 100 people, by name, are listed.");
    expect(cut.classList.contains("os-caption")).toBe(true);
    // No row approves removals, and somebody still does.
    expect(within(list).queryByText("Approves removals")).toBeNull();
    const approver = settingRow("Removal approver");
    expect(within(approver).queryByText(/Nobody can remove a published review/)).toBeNull();
    expect(within(approver).getByText(/Someone not among the people listed is the one person who can remove a published review/)).toBeTruthy();
  });

  it("say nobody among the first listed has the name typed, when the engine cut the list", async () => {
    // Zoe can moderate and sorts past the cut: nobody listed is called that,
    // which is not nobody who can moderate.
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: { storeId: "store-live", people: FIRST_HUNDRED, truncated: true } as unknown as Row }));
    await click(tab("Settings"));
    const choices = await search("Zoe");
    expect(within(choices).queryAllByRole("option")).toEqual([]);
    expect(screen.getByText("Nobody among the first 100 by that name.")).toBeTruthy();
    expect(screen.queryByText("Nobody who can moderate this store by that name.")).toBeNull();
  });

  it("say nobody who can moderate has the name typed, when everybody who can is listed", async () => {
    await openReviews(reviewsSeed({ settings: APPROVER_SETTINGS, people: PEOPLE }));
    await click(tab("Settings"));
    const choices = await search("Zoe");
    expect(within(choices).queryAllByRole("option")).toEqual([]);
    expect(screen.getByText("Nobody who can moderate this store by that name.")).toBeTruthy();
    expect(screen.queryByText(/Nobody among the first/)).toBeNull();
  });
});

describe("review directory pages", () => {
  function fixture() {
    const rows = Array.from({ length: 25 }, (_, n) => ({ ...REVIEW, id: `v1:reviews:review:page-${n}`, title: `Review ${String(n).padStart(2,"0")}`, createdAt: new Date(NOW - (25-n)*60_000).toISOString() }));
    const decisions = rows.map((row,n) => ({ id: `decision-${n}`, reviewId: row.id, title: row.title, action:"escalate", note: n < 2 ? "Earlier needle%_" : "Routine", decidedBy:"u-me", at: row.createdAt, revision:2, reviewState:"waiting", productHandle:"floss" })).reverse();
    const seed = reviewsSeed({ list:{waiting:{reviews:rows,total:25,next:"",counts:{waiting:25,all:25}} as unknown as Row}, log:{decisions,total:25,next:""} as unknown as Row,
      get:Object.fromEntries(rows.map(row=>[row.id,{review:row,decisions:[],acts:["publish"],moderates:true} as unknown as Row])) });
    const connection = fakeConnection(seed);h.connection=connection;
    const mount = () => render(withSession(<ReviewsSection site={siteFromRow(SHOP)} storeId="store-live" name="Shop" nameOf={()=>""} viewerUserId="u-me" trail={[]} back={{label:"Shop",onSelect:()=>{}}} />));
    return {seed,connection,mount,rows};
  }
  const reviews = () => within(screen.getByRole("list",{name:"Waiting reviews"})).getAllByRole("button");
  it("loads ten incrementally, searches beyond them and retains twenty after reopening", async () => {
    const {connection,mount}=fixture();let view=mount();
    await waitFor(()=>expect(reviews()).toHaveLength(10));
    expect(connection.callsNamed("reviewsList")[0]).toContain("limit: 10");
    await click(screen.getByRole("button",{name:"Load more"}));
    await waitFor(()=>expect(reviews()).toHaveLength(20));
    expect(connection.callsNamed("reviewsList")[1]).toContain('after: "10"');
    await typeInto(screen.getByLabelText("Search reviews") as HTMLInputElement,"REVIEW 24");
    await waitFor(()=>expect(reviews().map(row=>row.getAttribute("aria-label"))).toEqual(["Open review Review 24"]));
    await typeInto(screen.getByLabelText("Search reviews") as HTMLInputElement,"");
    await waitFor(()=>expect(reviews()).toHaveLength(20));
    const reads=connection.callsNamed("reviewsList").length;view.unmount();view=mount();
    await waitFor(()=>expect(reviews()).toHaveLength(20));
    expect(connection.callsNamed("reviewsList")).toHaveLength(reads);
    await click(screen.getByRole("button",{name:"Load more"}));
    await waitFor(()=>expect(reviews()).toHaveLength(25));
    expect(screen.queryByRole("button",{name:"Load more"})).toBeNull();
    view.unmount();
  });
  it("retains decision-log pages across tabs and searches notes before pagination",async()=>{
    const {connection,mount}=fixture();mount();await screen.findByRole("list",{name:"Waiting reviews"});
    await click(tab("Decision log"));
    const decisions=()=>within(screen.getByRole("list",{name:"Decision log"})).getAllByRole("button");
    await waitFor(()=>expect(decisions()).toHaveLength(10));
    await click(screen.getByRole("button",{name:"Load more"}));await waitFor(()=>expect(decisions()).toHaveLength(20));
    await typeInto(screen.getByLabelText("Search decisions") as HTMLInputElement,"NEEDLE%_");await waitFor(()=>expect(decisions()).toHaveLength(2));
    await typeInto(screen.getByLabelText("Search decisions") as HTMLInputElement,"");await waitFor(()=>expect(decisions()).toHaveLength(20));
    const reads=connection.callsNamed("reviewsDecisionLog").length;
    await click(tab("Waiting"));await click(tab("Decision log"));await waitFor(()=>expect(decisions()).toHaveLength(20));
    expect(connection.callsNamed("reviewsDecisionLog")).toHaveLength(reads);
  });
  it("refreshes retained depth after a detail decision without rereading an inactive directory",async()=>{
    const {connection,seed,mount,rows}=fixture();mount();await screen.findByRole("list",{name:"Waiting reviews"});
    await click(screen.getByRole("button",{name:"Load more"}));await waitFor(()=>expect(reviews()).toHaveLength(20));
    const original=vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.mocked(connection.query.executeNamed).mockImplementation(async(name,call,options)=>{
      if(name==="reviewsDecide") seed.reviews!.list!.waiting={reviews:rows.slice(1),total:24,next:"",counts:{waiting:24,all:25}} as unknown as Row;
      return original(name,call,options);
    });
    await click(screen.getByRole("button",{name:"Open review Review 00"}));await screen.findByRole("heading",{name:"Review 00"});
    const reads=connection.callsNamed("reviewsList").length;
    await readTheBar();await click(within(bar()).getByRole("button",{name:"Publish"}));await waitFor(()=>expect(connection.callsNamed("reviewsDecide")).toHaveLength(1));
    expect(connection.callsNamed("reviewsList")).toHaveLength(reads);
    await click(screen.getByRole("button",{name:"Back to Waiting"}));
    await waitFor(()=>expect(reviews()[0]?.getAttribute("aria-label")).toBe("Open review Review 01"));expect(reviews()).toHaveLength(20);
    expect(connection.callsNamed("reviewsList").slice(reads)).toHaveLength(2);
  });
});
