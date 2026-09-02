// The Fleet's pure derivations, with no browser, no server and no React.
//
// Everything asserted here is a function of its inputs, and every one of them
// is a rule the SERVER also holds -- the online window, the label merge, the
// latest-per-id collapse. That is why they are tested in isolation rather than
// through a render: a rendered assertion passes through three layers that can
// each fail for unrelated reasons, and when it goes red it does not say which
// of the three was wrong.
//
// The online cases are the ones that matter most. The failure mode this guards
// is not "the dot is the wrong colour" -- it is a machine reading as reachable
// when it is not, which sends an operator to debug a computer that is fine.

import { describe, expect, it } from "vitest";

import {
  installCommand,
  workerClusterUrl,
  CLUSTER_URL_PLACEHOLDER,
  INSTALL_PLATFORMS,
} from "../src/fleet/install";
import {
  chipsFromMap,
  formatLabelChip,
  mapFromChips,
  mergeLabels,
  parseLabelChip,
} from "../src/fleet/labels";
import { isWorkerOnline, ONLINE_WINDOW_SECONDS } from "../src/fleet/online";
import { latestPerId, type WorkbenchNode } from "../src/fleet/rows";

const NOW = new Date("2026-08-23T12:00:00.000Z");

function secondsAgo(seconds: number): string {
  return new Date(NOW.getTime() - seconds * 1000).toISOString();
}

describe("the online derivation", () => {
  // The window is READ FROM THE SOURCE rather than written as 30 here, so this
  // file cannot be the thing that disagrees with component/worker/online.go.
  // The literal itself is pinned by TestFleetOnlineWindowMatchesPortal, which
  // parses online.ts; a second copy in a test would just be a third place to
  // forget.
  it("is online inside the window", () => {
    expect(isWorkerOnline(secondsAgo(1), "", NOW)).toBe(true);
    expect(isWorkerOnline(secondsAgo(ONLINE_WINDOW_SECONDS - 1), "", NOW)).toBe(true);
  });

  it("is online exactly ON the window boundary", () => {
    // The rule is "within", not "strictly within". Asserted because an
    // off-by-one here flips a machine at the boundary on every other tick,
    // which reads as a flapping fleet rather than as a comparison bug.
    expect(isWorkerOnline(secondsAgo(ONLINE_WINDOW_SECONDS), "", NOW)).toBe(true);
  });

  it("is offline past the window", () => {
    expect(isWorkerOnline(secondsAgo(ONLINE_WINDOW_SECONDS + 1), "", NOW)).toBe(false);
    expect(isWorkerOnline(secondsAgo(3600), "", NOW)).toBe(false);
  });

  it("is offline when revoked, however fresh the heartbeat", () => {
    // The vacuous pass here would be a machine that is offline anyway, so the
    // heartbeat is deliberately one second old: only the revocation can make
    // this false.
    expect(isWorkerOnline(secondsAgo(1), "", NOW)).toBe(true);
    expect(isWorkerOnline(secondsAgo(1), "2026-08-23T11:59:00.000Z", NOW)).toBe(false);
  });

  it("is offline when it has never been seen, and when the value cannot be read", () => {
    expect(isWorkerOnline("", "", NOW)).toBe(false);
    expect(isWorkerOnline("   ", "", NOW)).toBe(false);
    expect(isWorkerOnline("not a timestamp", "", NOW)).toBe(false);
  });

  it("treats a future heartbeat as online rather than as a negative age", () => {
    // Clock skew between the cluster and the browser. Refusing it would make
    // the whole list go dark on a laptop whose clock is a minute slow.
    const ahead = new Date(NOW.getTime() + 90_000).toISOString();
    expect(isWorkerOnline(ahead, "", NOW)).toBe(true);
  });
});

describe("the label merge", () => {
  it("gives the operator's value precedence and says so", () => {
    const merged = mergeLabels(
      { os: "darwin", arch: "arm64" },
      { os: "linux", role: "render" },
    );
    // Sorted by key: arch, os, role.
    expect(merged.map((one) => one.key)).toEqual(["arch", "os", "role"]);

    const os = merged.find((one) => one.key === "os");
    expect(os?.value).toBe("linux");
    expect(os?.source).toBe("operator");
    // `overrides` is a DIFFERENT claim from source==="operator" -- an
    // operator-only key overrides nothing -- and the page renders the two
    // differently, so both are asserted.
    expect(os?.overrides).toBe(true);

    const role = merged.find((one) => one.key === "role");
    expect(role?.source).toBe("operator");
    expect(role?.overrides).toBe(false);

    const arch = merged.find((one) => one.key === "arch");
    expect(arch?.value).toBe("arm64");
    expect(arch?.source).toBe("reported");
  });

  it("keeps an operator value that is deliberately empty", () => {
    // "" is a value somebody set, not an absence. Falling back to the reported
    // value here would silently un-clear a label an operator blanked.
    const merged = mergeLabels({ tier: "gpu" }, { tier: "" });
    expect(merged[0]?.value).toBe("");
    expect(merged[0]?.source).toBe("operator");
  });

  it("round-trips through the key=value chip form", () => {
    const map = { "has-blender": "true", os: "darwin" };
    expect(chipsFromMap(map)).toEqual(["has-blender=true", "os=darwin"]);
    expect(mapFromChips(chipsFromMap(map))).toEqual(map);
  });

  it("splits a chip on the FIRST separator so a value may contain one", () => {
    expect(parseLabelChip("path=/opt/a=b")).toEqual({ key: "path", value: "/opt/a=b" });
    expect(formatLabelChip("path", "/opt/a=b")).toBe("path=/opt/a=b");
  });

  it("refuses a chip that is not a pair", () => {
    // A bare word is a value with no key. Guessing which half was meant is how
    // a routing rule ends up matching something nobody wrote.
    expect(parseLabelChip("darwin")).toBeNull();
    expect(parseLabelChip("=darwin")).toBeNull();
    expect(mapFromChips(["darwin", "os=darwin"])).toEqual({ os: "darwin" });
  });
});

describe("the cluster-node collapse", () => {
  function node(id: string, createdAt: string, health: string): WorkbenchNode {
    return {
      id,
      nodeType: "workbench",
      address: "",
      health,
      lastSeen: createdAt,
      labels: {},
      capabilities: [],
      region: "",
      provider: "",
      createdAt,
    };
  }

  it("keeps the newest row per id", () => {
    // clusterNodes returns the WHOLE append-only history -- one row per
    // liveness transition -- so the vacuous pass here is "three rows in, three
    // rows out". The fixture gives one node three rows on purpose.
    const collapsed = latestPerId([
      node("workbench-a", "2026-08-01T00:00:00.000Z", "connecting"),
      node("workbench-a", "2026-08-02T00:00:00.000Z", "healthy"),
      node("workbench-b", "2026-08-01T00:00:00.000Z", "healthy"),
      node("workbench-a", "2026-08-03T00:00:00.000Z", "draining"),
    ]);
    expect(collapsed.map((one) => one.id)).toEqual(["workbench-a", "workbench-b"]);
    expect(collapsed[0]?.health).toBe("draining");
  });
});

describe("the install command", () => {
  it("states the transport in the cluster URL", () => {
    // A bare host:port is dialled in the clear whatever its port
    // (sdk/go/worker.ParseClusterURL), so the scheme is not cosmetic.
    expect(workerClusterUrl("example.com")).toBe("https://api.example.com");
    expect(workerClusterUrl("https://example.com/")).toBe("https://api.example.com");
    expect(workerClusterUrl("  ")).toBe("");
  });

  it("composes the runbook's one-liner with the minted token", () => {
    const command = installCommand({
      platform: "mac",
      clusterUrl: "https://api.example.com",
      token: "mql_wkr_abc",
      computerUse: true,
    });
    expect(command).toContain("install-mac.sh");
    expect(command).toContain("--token mql_wkr_abc");
    expect(command).toContain("--cluster https://api.example.com");
    expect(command).toContain("--computeruse");
  });

  it("is ONE physical line, whatever the inputs", () => {
    // The multi-line-with-trailing-backslashes form was split by terminal
    // paste handling: `bash -s --` ran with no arguments and `--token
    // mql_wkr_...` executed as its own failing command, with the worker token
    // in shell history either way (memql#4873). What is pinned is the ABSENCE
    // of the two characters a terminal can mis-handle -- any newline or
    // backslash reintroduces the split. Every combination is swept because
    // the computer-use branch was exactly where the old shape changed its
    // line structure.
    for (const platform of INSTALL_PLATFORMS) {
      for (const computerUse of [false, true]) {
        for (const clusterUrl of ["https://api.example.com", ""]) {
          const command = installCommand({
            platform,
            clusterUrl,
            token: "mql_wkr_abc",
            computerUse,
          });
          expect(command).not.toContain("\n");
          expect(command).not.toContain("\\");
        }
      }
    }
  });

  it("pins the exact composed shape", () => {
    // Word for word, so a re-ordering of flags or a doubled space fails HERE
    // rather than on an operator's machine. The runbook prints this same
    // single line (memql#4874); if this assertion has to change, the runbook
    // changes with it.
    const command = installCommand({
      platform: "mac",
      clusterUrl: "https://api.example.com",
      token: "mql_wkr_abc",
      computerUse: true,
    });
    expect(command).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-mac.sh" +
        " | bash -s -- --token mql_wkr_abc --cluster https://api.example.com --computeruse",
    );
  });

  it("omits --computeruse for the headless build", () => {
    const command = installCommand({
      platform: "linux",
      clusterUrl: "https://api.example.com",
      token: "mql_wkr_abc",
      computerUse: false,
    });
    expect(command).toContain("install-linux.sh");
    expect(command).not.toContain("--computeruse");
  });

  it("uses an obviously-not-a-URL placeholder when the cluster publishes no domain", () => {
    // It has to FAIL at the shell rather than dial something. A silently
    // plausible default would be worse than no default.
    const command = installCommand({
      platform: "mac",
      clusterUrl: "",
      token: "mql_wkr_abc",
      computerUse: false,
    });
    expect(command).toContain(CLUSTER_URL_PLACEHOLDER);
  });
});
