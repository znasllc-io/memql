import { describe, expect, it } from "vitest";

import { siteFromRow } from "../../src/apps/deployables/rows";
import {
  EMPTY_STORE_DRAFT,
  conflictingStoreIds,
  STORE_SETTINGS_MAX_STORES,
  keptStoreIds,
  storeCountProblem,
  storeSettingsFingerprint,
  storeSettingsFromDraft,
  storeTargets,
  editStore,
  keepDraftOverStored,
  settleDraft,
  storeValuesConflict,
  takeStoredWhereMoved,
  type StoreValuesDraft,
} from "../../src/apps/deployables/store/storeValues";
import { SHOP, siteRow } from "./harness";

// PER-STORE SETTINGS, the arithmetic (memql#5602).
//
// `v1:platform:site.storeSettings` is a map from a BARE store id to that
// store's own {key: value} settings, and the edge merges the entry of the store
// the in-force binding names over `settings`. The editor's claims are claims
// about functions -- which stores a storefront shows, what a save sends, when a
// change made elsewhere is a conflict -- so they are asserted here, with no DOM.

describe("the row", () => {
  it("projects storeSettings, keeping only string values and object entries", () => {
    const site = siteFromRow(
      siteRow({
        ...SHOP,
        id: "site-shop",
        storeSettings: {
          "store-example": { customerAccountClientId: "shp_live", stray: 7 },
          "store-example-dev": { customerAccountClientId: "shp_dev" },
          "not-an-object": "oops",
        },
      } as never),
    );
    // A non-string value is a raw write that bypassed the guard, and a store
    // entry that is not an object is the same -- the edge drops both
    // (component/edge/edge.go rowStoreSettings), so the editor must not show a
    // value the document never serves.
    expect(site.storeSettings).toEqual({
      "store-example": { customerAccountClientId: "shp_live" },
      "store-example-dev": { customerAccountClientId: "shp_dev" },
    });
  });

  it("reads an absent field as no stores at all", () => {
    expect(siteFromRow(SHOP).storeSettings).toEqual({});
  });
});

describe("which stores a storefront shows", () => {
  it("names the Production store first, then the Testing store", () => {
    const site = siteFromRow(siteRow({ ...SHOP, id: "site-shop", previewBinding: { storeId: "store-example-dev" } } as never));
    expect(storeTargets(site)).toEqual([
      { storeId: "store-example", destinations: ["production"] },
      { storeId: "store-example-dev", destinations: ["testing"] },
    ]);
  });

  it("shows a store both destinations use ONCE, because its values are keyed by store", () => {
    const site = siteFromRow(siteRow({ ...SHOP, id: "site-shop", previewBinding: { storeId: "store-example" } } as never));
    expect(storeTargets(site)).toEqual([{ storeId: "store-example", destinations: ["production", "testing"] }]);
  });

  it("keys a binding written in the canonical form by its BARE id, as the guard requires", () => {
    // The store-settings guard refuses `v1:shopify:store:<id>` as a key, and
    // the edge looks the entry up by the bare id its binding names.
    const site = siteFromRow(siteRow({ ...SHOP, id: "site-shop", binding: { storeId: "v1:shopify:store:acme-widgets" } } as never));
    expect(storeTargets(site)).toEqual([{ storeId: "acme-widgets", destinations: ["production"] }]);
  });

  it("shows nothing for a storefront no store is connected to", () => {
    const site = siteFromRow(siteRow({ ...SHOP, id: "site-shop", binding: {} } as never));
    expect(storeTargets(site)).toEqual([]);
  });

  it("lists the stores whose values are kept although no binding names them", () => {
    const stored = { "store-example": { a: "1" }, "old-shop": { a: "2" }, "older-shop": { b: "3" } };
    expect(keptStoreIds(stored, [{ storeId: "store-example", destinations: ["production"] }])).toEqual(["old-shop", "older-shop"]);
  });
});

describe("what a save sends", () => {
  it("is the stored map when nothing was touched", () => {
    const stored = { "store-example": { customerAccountClientId: "shp_live" }, "old-shop": { wholesaleAdapter: "customerTag" } };
    expect(storeSettingsFromDraft(EMPTY_STORE_DRAFT, stored)).toEqual(stored);
  });

  it("replaces a store's entry with its edited rows and KEEPS the entry of a store no binding names", () => {
    // updateSiteStoreSettings REPLACES the whole map, so an entry the panel
    // does not edit -- a store this storefront was bound to before -- has to be
    // sent back, or saving one store's values would erase another's.
    const stored = { "store-example": { customerAccountClientId: "shp_live" }, "old-shop": { wholesaleAdapter: "customerTag" } };
    const draft: StoreValuesDraft = {
      edits: {
        "store-example-dev": [{ id: "new-1", key: "customerAccountClientId", value: "shp_dev" }],
      },
      removed: [],
      basis: {},
    };
    expect(storeSettingsFromDraft(draft, stored)).toEqual({
      "store-example": { customerAccountClientId: "shp_live" },
      "store-example-dev": { customerAccountClientId: "shp_dev" },
      "old-shop": { wholesaleAdapter: "customerTag" },
    });
  });

  it("drops a kept store the person removed, and a store whose rows were all removed", () => {
    const stored = { "store-example": { customerAccountClientId: "shp_live" }, "old-shop": { wholesaleAdapter: "customerTag" } };
    const draft: StoreValuesDraft = { edits: { "store-example": [] }, removed: ["old-shop"], basis: {} };
    // An EMPTY entry is not sent: it serves nothing, and it would count against
    // the sixteen stores a deployable may keep.
    expect(storeSettingsFromDraft(draft, stored)).toEqual({});
  });

  it("drops a row whose name is blank -- somebody part-way through adding one", () => {
    const draft: StoreValuesDraft = {
      edits: { "store-example": [{ id: "new-1", key: "", value: "half typed" }, { id: "k", key: "region", value: "eu" }] },
      removed: [],
      basis: {},
    };
    expect(storeSettingsFromDraft(draft, {})).toEqual({ "store-example": { region: "eu" } });
  });
});

describe("comparing two maps", () => {
  it("ignores key order and empty entries", () => {
    expect(storeSettingsFingerprint({ b: { y: "2", x: "1" }, a: { z: "3" } })).toBe(
      storeSettingsFingerprint({ a: { z: "3" }, b: { x: "1", y: "2" }, c: {} }),
    );
    expect(storeSettingsFingerprint({ a: { z: "3" } })).not.toBe(storeSettingsFingerprint({ a: { z: "4" } }));
  });
});

describe("the sixteen stores a deployable keeps", () => {
  it("says nothing at the cap and names the remedy past it", () => {
    const at: Record<string, Record<string, string>> = {};
    for (let i = 0; i < STORE_SETTINGS_MAX_STORES; i += 1) at[`shop-${i}`] = { a: "1" };
    expect(storeCountProblem(at)).toBe("");
    const past = { ...at, "one-more": { a: "1" } };
    expect(storeCountProblem(past)).toMatch(/17 stores/);
    expect(storeCountProblem(past)).toMatch(/Remove the values kept for stores this storefront no longer uses/);
  });
});

describe("a change made somewhere else", () => {
  const stored = { "store-example": { customerAccountClientId: "shp_live" }, "store-example-dev": { customerAccountClientId: "shp_dev" } };
  // The person edits Production's store while the row says shp_live.
  const edited = editStore(EMPTY_STORE_DRAFT, stored, "store-example", [
    { id: "customerAccountClientId", key: "customerAccountClientId", value: "shp_mine" },
  ]);

  it("is no conflict while the draft is untouched: the panel follows the row", () => {
    const moved = { ...stored, "store-example": { customerAccountClientId: "shp_rotated" } };
    expect(storeValuesConflict(EMPTY_STORE_DRAFT, moved)).toBe(false);
    expect(storeSettingsFromDraft(EMPTY_STORE_DRAFT, moved)).toEqual(moved);
  });

  it("is a conflict when a store the person edited changed, and their edit disagrees with it", () => {
    const moved = { ...stored, "store-example": { customerAccountClientId: "shp_rotated" } };
    expect(storeValuesConflict(edited, moved)).toBe(true);
  });

  it("is no conflict when the row did not move", () => {
    expect(storeValuesConflict(edited, stored)).toBe(false);
  });

  it("is no conflict when the row moved TO what the draft already says -- this save landing", () => {
    const landed = { ...stored, "store-example": { customerAccountClientId: "shp_mine" } };
    expect(storeValuesConflict(edited, landed)).toBe(false);
    // ...and the draft then has nothing left to say about that store.
    expect(settleDraft(edited, landed)).toEqual(EMPTY_STORE_DRAFT);
  });

  it("is no conflict when ANOTHER store changed: a save sends that store as it now is", () => {
    const otherMoved = { ...stored, "store-example-dev": { customerAccountClientId: "shp_dev_rotated" } };
    expect(storeValuesConflict(edited, otherMoved)).toBe(false);
    expect(storeSettingsFromDraft(edited, otherMoved)).toEqual({
      "store-example": { customerAccountClientId: "shp_mine" },
      "store-example-dev": { customerAccountClientId: "shp_dev_rotated" },
    });
  });

  it("names the stores in conflict, so the panel can show what each is now", () => {
    const both = editStore(edited, stored, "store-example-dev", [
      { id: "customerAccountClientId", key: "customerAccountClientId", value: "shp_dev_mine" },
    ]);
    const moved = { ...stored, "store-example": { customerAccountClientId: "shp_rotated" } };
    expect(conflictingStoreIds(both, moved)).toEqual(["store-example"]);
    expect(conflictingStoreIds(both, stored)).toEqual([]);
  });

  it("keeping the edit takes the new row as its starting point, so Save replaces it knowingly", () => {
    const moved = { ...stored, "store-example": { customerAccountClientId: "shp_rotated" } };
    const kept = keepDraftOverStored(edited, moved);
    expect(storeValuesConflict(kept, moved)).toBe(false);
    expect(storeSettingsFromDraft(kept, moved)["store-example"]).toEqual({ customerAccountClientId: "shp_mine" });
  });

  it("taking the new values drops the edit of each store that changed, and only those", () => {
    const both = editStore(edited, stored, "store-example-dev", [
      { id: "customerAccountClientId", key: "customerAccountClientId", value: "shp_dev_mine" },
    ]);
    const moved = { ...stored, "store-example": { customerAccountClientId: "shp_rotated" } };
    const taken = takeStoredWhereMoved(both, moved);
    expect(storeSettingsFromDraft(taken, moved)).toEqual({
      "store-example": { customerAccountClientId: "shp_rotated" },
      "store-example-dev": { customerAccountClientId: "shp_dev_mine" },
    });
    expect(storeValuesConflict(taken, moved)).toBe(false);
  });
});
