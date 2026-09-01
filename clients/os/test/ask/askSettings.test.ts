import { describe, expect, it } from "vitest";

import {
  DEFAULT_ASK_SETTINGS,
  LocalAskSettingsStore,
  sanitizeAskSettings,
} from "../../src/apps/settings/askSettings";

// The same discipline every per-app store in this shell follows: a wrong
// version is wholesale, everything else is repaired field by field.

function memStorage(): Storage {
  const map = new Map<string, string>();
  return {
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
    clear: () => map.clear(),
    key: () => null,
    get length() {
      return map.size;
    },
  } as Storage;
}

describe("Ask settings", () => {
  it("sends on release by default", () => {
    expect(DEFAULT_ASK_SETTINGS.commit).toBe("send");
    expect(DEFAULT_ASK_SETTINGS.spaceToTalk).toBe(true);
  });

  it("repairs one bad field without spending the others", () => {
    const repaired = sanitizeAskSettings({ version: 1, commit: "yell", spaceToTalk: false });
    expect(repaired.commit).toBe("send");
    expect(repaired.spaceToTalk).toBe(false);
  });

  it("discards a document from a version it cannot read", () => {
    expect(sanitizeAskSettings({ version: 7, commit: "review", spaceToTalk: false })).toEqual(
      DEFAULT_ASK_SETTINGS,
    );
  });

  it("survives absent, corrupt and unwritable storage", () => {
    const store = new LocalAskSettingsStore(memStorage());
    expect(store.load()).toEqual(DEFAULT_ASK_SETTINGS);
    store.save({ version: 1, commit: "review", spaceToTalk: false });
    expect(store.load()).toEqual({ version: 1, commit: "review", spaceToTalk: false });

    // A private window with storage disabled keeps working, on defaults.
    const none = new LocalAskSettingsStore(null);
    expect(none.load()).toEqual(DEFAULT_ASK_SETTINGS);
    expect(() => none.save(DEFAULT_ASK_SETTINGS)).not.toThrow();
  });
});
