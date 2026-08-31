import { describe, expect, it } from "vitest";

import { isWorkerOnline, ONLINE_WINDOW_SECONDS } from "../../src/apps/fleet/online";
import {
  DEFAULT_FLEET_SETTINGS,
  FLEET_SECTION_IDS,
  LocalFleetSettingsStore,
  sanitizeFleetSettings,
} from "../../src/apps/fleet/settings";

class MemoryStorage {
  private store = new Map<string, string>();
  getItem(key: string) {
    return this.store.get(key) ?? null;
  }
  setItem(key: string, value: string) {
    this.store.set(key, value);
  }
  raw(key: string) {
    return this.store.get(key);
  }
}

describe("fleet settings", () => {
  it("round-trips through the store", () => {
    const storage = new MemoryStorage();
    const store = new LocalFleetSettingsStore(storage, "k");
    store.save({ version: 1, defaultSection: "routing", showRevoked: true });
    expect(store.load()).toEqual({ version: 1, defaultSection: "routing", showRevoked: true });
  });

  it("defaults to Machines with revoked machines hidden", () => {
    expect(new LocalFleetSettingsStore(new MemoryStorage(), "k").load()).toEqual(
      DEFAULT_FLEET_SETTINGS,
    );
    expect(DEFAULT_FLEET_SETTINGS.defaultSection).toBe("machines");
    expect(DEFAULT_FLEET_SETTINGS.showRevoked).toBe(false);
  });

  it("repairs each field independently, so one bad value does not cost the other", () => {
    // A garbage defaultSection must not discard the operator's show-revoked
    // choice: they are unrelated preferences that happen to share a document.
    expect(sanitizeFleetSettings({ version: 1, defaultSection: 42, showRevoked: true })).toEqual({
      version: 1,
      defaultSection: "machines",
      showRevoked: true,
    });
    expect(sanitizeFleetSettings({ version: 1, defaultSection: "routing", showRevoked: "yes" })).toEqual(
      { version: 1, defaultSection: "routing", showRevoked: false },
    );
  });

  it("repairs a section this app no longer declares", () => {
    expect(sanitizeFleetSettings({ version: 1, defaultSection: "gone", showRevoked: false })
      .defaultSection).toBe("machines");
    // ...and accepts every one it does, so the picker and the manifest agree.
    for (const id of FLEET_SECTION_IDS) {
      expect(sanitizeFleetSettings({ version: 1, defaultSection: id, showRevoked: false })
        .defaultSection).toBe(id);
    }
  });

  it("discards a document of an unknown version wholesale", () => {
    expect(sanitizeFleetSettings({ version: 2, defaultSection: "routing", showRevoked: true })).toEqual(
      DEFAULT_FLEET_SETTINGS,
    );
  });

  it("survives unparseable storage, a hostile value and storage that throws", () => {
    const storage = new MemoryStorage();
    storage.setItem("k", "{not json");
    expect(new LocalFleetSettingsStore(storage, "k").load()).toEqual(DEFAULT_FLEET_SETTINGS);

    storage.setItem("k", JSON.stringify(["array"]));
    expect(new LocalFleetSettingsStore(storage, "k").load()).toEqual(DEFAULT_FLEET_SETTINGS);

    const hostile = {
      getItem() {
        throw new Error("blocked");
      },
      setItem() {
        throw new Error("quota");
      },
    };
    const store = new LocalFleetSettingsStore(hostile, "k");
    expect(store.load()).toEqual(DEFAULT_FLEET_SETTINGS);
    // A preference is not worth failing an interaction over.
    expect(() => store.save(DEFAULT_FLEET_SETTINGS)).not.toThrow();
  });

  it("is a no-op with no storage at all (a private window)", () => {
    const store = new LocalFleetSettingsStore(null, "k");
    expect(() => store.save({ version: 1, defaultSection: "routing", showRevoked: true })).not.toThrow();
    expect(store.load()).toEqual(DEFAULT_FLEET_SETTINGS);
  });
});

describe("the online rule the fleet renders", () => {
  const now = new Date("2026-08-30T12:00:00Z");
  const secondsAgo = (n: number) => new Date(now.getTime() - n * 1000).toISOString();

  it("is online while a heartbeat is inside the window", () => {
    expect(isWorkerOnline({ lastSeenAt: secondsAgo(ONLINE_WINDOW_SECONDS - 1) }, now)).toBe(true);
  });

  it("goes offline past the window", () => {
    expect(isWorkerOnline({ lastSeenAt: secondsAgo(ONLINE_WINDOW_SECONDS + 1) }, now)).toBe(false);
  });

  it("is NEVER online once revoked, whatever the heartbeat says", () => {
    // The case a fleet list gets wrong: a machine revoked seconds ago still
    // has a fresh lastSeenAt, and a clock-only rule renders it green.
    expect(
      isWorkerOnline({ lastSeenAt: secondsAgo(1), revokedAt: "2026-08-30T11:59:59Z" }, now),
    ).toBe(false);
  });

  it("treats a machine that never checked in, and an unreadable timestamp, as offline", () => {
    expect(isWorkerOnline({}, now)).toBe(false);
    expect(isWorkerOnline({ lastSeenAt: "soon" }, now)).toBe(false);
  });
});
