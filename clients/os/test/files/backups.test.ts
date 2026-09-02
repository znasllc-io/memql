import { describe, expect, it } from "vitest";

import {
  backupFingerprint,
  backupFromRow,
  fileBelongsToBackup,
  linkToneOf,
  ORIGIN_STATES,
  TONE_LABEL,
  TONE_SENTENCE,
  worstFileState,
  type BackupRow,
} from "../../src/apps/files/backups/rows";
import { folderChoices, parseGlobs, pathPlaceholderFor } from "../../src/apps/files/backups/BackupForm";
import type { FolderRow } from "../../src/apps/files/rows";
import type { MachineRow } from "../../src/apps/fleet/rows";

// The backups surface's pure half (epic memql#4783, the cockpit half #4841).
//
// Every interesting case here is about ABSENCE, which is why these are worth
// testing without a browser: an absent value renders as nothing at all, so a
// fold that reads absence wrongly looks exactly like one that is right.

function backup(over: Partial<BackupRow> = {}): BackupRow {
  return {
    id: "w1",
    ownerUserId: "user-1",
    workerId: "wkr-1",
    localPath: "/Users/ana/Clients",
    folderId: "",
    status: "active",
    excludeGlobs: [],
    includeHidden: false,
    archived: false,
    originState: "ok",
    lastSweepAt: "2026-09-01T10:00:00Z",
    lastSweepError: "",
    filesSeen: 12,
    bytesSeen: 4096,
    ...over,
  };
}

describe("backupFromRow", () => {
  it("reads a row the cockpit has never reported on as reported-on-by-nobody", () => {
    const row = backupFromRow({
      id: "w1",
      workerId: "wkr-1",
      localPath: "/Users/ana/Clients",
      status: "active",
    } as never);
    // Not "ok". This is the whole epic's discipline applied one level up from
    // linkState: "we do not know" and "we know and it is fine" are different
    // answers and must not share a spelling.
    expect(row.originState).toBe("");
    expect(row.lastSweepAt).toBe("");
    expect(row.filesSeen).toBe(0);
  });

  it("reads an unrecognised status as active, which is the reading that asserts least", () => {
    // A build that met a status it does not know must not tell somebody their
    // files have stopped being copied when they have not.
    expect(backupFromRow({ id: "w1", status: "hibernating" } as never).status).toBe("active");
    expect(backupFromRow({ id: "w1", status: "paused" } as never).status).toBe("paused");
  });

  it("reads an unrecognised origin state as not-reported rather than showing a state it cannot describe", () => {
    expect(backupFromRow({ id: "w1", originState: "on_fire" } as never).originState).toBe("");
    // The reachable positive: every value the concept DOES declare survives,
    // so the assertion above is about the filter and not about the projection
    // dropping the field entirely.
    for (const state of ORIGIN_STATES) {
      expect(backupFromRow({ id: "w1", originState: state } as never).originState).toBe(state);
    }
  });

  it("reads a payload-wrapped row the same as a flat one", () => {
    // The seed delivers shape-flattened rows and the subscription fold
    // delivers CDC envelopes. A backup that read one way on load and another
    // way on its next sweep would be unexplainable from the screen.
    const flat = backupFromRow({ id: "w1", localPath: "/a", status: "paused" } as never);
    const wrapped = backupFromRow({ id: "w1", payload: { localPath: "/a", status: "paused" } } as never);
    expect(wrapped.localPath).toBe(flat.localPath);
    expect(wrapped.status).toBe(flat.status);
  });
});

describe("linkToneOf", () => {
  it("says paused before it says anything is wrong", () => {
    // The cockpit stops sweeping when a backup is paused, so originState is
    // the last thing seen BEFORE the pause. Painting that as a live alarm on
    // something somebody deliberately stopped is how a person learns to
    // ignore the colour.
    expect(linkToneOf(backup({ status: "paused", originState: "missing" }), "origin_gone")).toBe("paused");
  });

  it("says waiting before it claims a backup is either working or broken", () => {
    expect(linkToneOf(backup({ originState: "" }), "")).toBe("waiting");
    // And it stays waiting even with files already tracked: the folder-level
    // report is what has not arrived.
    expect(linkToneOf(backup({ originState: "" }), "synced")).toBe("waiting");
  });

  it("separates a machine's refusal from a folder that cannot be read", () => {
    // These are repaired in completely different places -- one in the
    // machine's policy.yaml, one at the folder itself -- so they must not
    // collapse into one tone.
    expect(linkToneOf(backup({ originState: "refused_by_policy" }), "")).toBe("refused");
    expect(linkToneOf(backup({ originState: "unreadable" }), "")).toBe("broken");
    expect(linkToneOf(backup({ originState: "missing" }), "")).toBe("broken");
  });

  it("takes the worst of the file states once the folder itself is fine", () => {
    expect(linkToneOf(backup(), "origin_gone")).toBe("broken");
    expect(linkToneOf(backup(), "stale")).toBe("behind");
    expect(linkToneOf(backup(), "synced")).toBe("settled");
    // No tracked files at all is settled, not broken: a folder that was there
    // and empty has nothing to be behind on.
    expect(linkToneOf(backup(), "")).toBe("settled");
  });

  it("has a label and a sentence for every tone it can return", () => {
    // Colour is never the only carrier, which only holds if the words exist.
    const tones = ["paused", "waiting", "refused", "broken", "behind", "settled"] as const;
    for (const tone of tones) {
      expect(TONE_LABEL[tone].length).toBeGreaterThan(0);
      expect(TONE_SENTENCE[tone].length).toBeGreaterThan(0);
    }
  });
});

describe("fileBelongsToBackup", () => {
  const b = backup({ workerId: "wkr-1", localPath: "/Users/ana/Clients" });

  it("claims a file beneath the watched folder", () => {
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-1", uploadedFromPath: "/Users/ana/Clients/q3.pdf" }, b)).toBe(true);
  });

  it("does not claim a sibling folder whose name merely starts the same", () => {
    // The bug a bare startsWith would have: /Users/ana/Clients2 is a
    // different folder, and claiming its files would put another backup's
    // state on this one.
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-1", uploadedFromPath: "/Users/ana/Clients2/q3.pdf" }, b)).toBe(false);
  });

  it("accepts backslashes, because the path is whatever the reporting machine uses", () => {
    const win = backup({ workerId: "wkr-1", localPath: "C:\\Work\\Renders" });
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-1", uploadedFromPath: "C:\\Work\\Renders\\a.mov" }, win)).toBe(true);
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-1", uploadedFromPath: "C:\\Work\\Renders2\\a.mov" }, win)).toBe(false);
  });

  it("does not claim a file from another machine at the same path", () => {
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-2", uploadedFromPath: "/Users/ana/Clients/q3.pdf" }, b)).toBe(false);
  });

  it("claims nothing from a file with no machine -- a browser upload has no origin", () => {
    // This is the case that would put "changed on the machine" on a file that
    // came from no machine at all, which is why the match is on (machine,
    // path) rather than on the destination folder.
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "", uploadedFromPath: "" }, b)).toBe(false);
  });

  it("claims nothing when the backup names no path", () => {
    expect(fileBelongsToBackup({ uploadedFromWorkerId: "wkr-1", uploadedFromPath: "/anything" }, backup({ localPath: "" }))).toBe(false);
  });
});

describe("worstFileState", () => {
  it("takes the worst and ignores the untracked", () => {
    expect(worstFileState(["synced", "", "stale", "synced"])).toBe("stale");
    expect(worstFileState(["stale", "origin_gone"])).toBe("origin_gone");
    expect(worstFileState(["", ""])).toBe("");
  });
});

describe("backupFingerprint", () => {
  it("does not move when only the sweep's own clock moves", () => {
    // THE HEARTBEAT RULE. A sweep touches lastSweepAt, filesSeen and bytesSeen
    // on a schedule for every backup forever. Naming any of them here would
    // strobe the list on the sweep's own cycle -- the exact failure the
    // concept's own field doc warns about.
    const before = backupFingerprint(backup());
    const after = backupFingerprint(
      backup({ lastSweepAt: "2026-09-01T11:00:00Z", filesSeen: 99, bytesSeen: 999999 }),
    );
    expect(after).toBe(before);
  });

  it("moves for every change a person would call a change", () => {
    // The reachable positive: without this half, a fingerprint that returned
    // a constant would pass the test above and cue nothing, ever.
    const base = backupFingerprint(backup());
    expect(backupFingerprint(backup({ status: "paused" }))).not.toBe(base);
    expect(backupFingerprint(backup({ originState: "missing" }))).not.toBe(base);
    expect(backupFingerprint(backup({ folderId: "f2" }))).not.toBe(base);
    expect(backupFingerprint(backup({ localPath: "/elsewhere" }))).not.toBe(base);
    expect(backupFingerprint(backup({ lastSweepError: "permission denied" }))).not.toBe(base);
  });
});

describe("the form's pure helpers", () => {
  const folders: FolderRow[] = [
    { id: "f1", name: "Clients", parentFolderId: "", archived: false },
    { id: "f2", name: "2026", parentFolderId: "f1", archived: false },
    { id: "f3", name: "Old", parentFolderId: "", archived: true },
  ];

  it("offers every live folder, indented, and never the Bin", () => {
    const choices = folderChoices(folders);
    expect(choices.map((c) => c.id)).toEqual(["f1", "f2"]);
    // The child is indented under its parent, so a flat select still reads
    // like the tree the rail draws.
    expect(choices[1]?.label.startsWith("\u00a0")).toBe(true);
    expect(choices[0]?.label.startsWith("\u00a0")).toBe(false);
  });

  it("shapes the path hint from the machine's own reported OS", () => {
    // A macOS-shaped hint on a Windows machine is worse than no hint: it is a
    // wrong answer presented as help.
    expect(pathPlaceholderFor({ os: "darwin" } as MachineRow)).toContain("/Users/");
    expect(pathPlaceholderFor({ os: "windows" } as MachineRow)).toContain("C:\\");
    expect(pathPlaceholderFor({ os: "linux" } as MachineRow)).toContain("/home/");
    // A machine that reported no OS gets a sentence rather than a guess.
    expect(pathPlaceholderFor(undefined)).not.toContain("/");
  });

  it("drops patterns that are only whitespace", () => {
    // A pattern that matches nothing but reads like a rule is worse than no
    // rule -- somebody would believe those files were being skipped.
    expect(parseGlobs("node_modules/**, , *.tmp ,")).toEqual(["node_modules/**", "*.tmp"]);
    expect(parseGlobs("   ")).toEqual([]);
  });
});
