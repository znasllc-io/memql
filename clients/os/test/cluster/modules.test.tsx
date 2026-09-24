import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read, and its provider dials a
// real socket. Replacing the READ is what lets the real ModulesClient, the
// real payload narrowing and the real error semantics run under jsdom.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { ModulesSection } = await import("../../src/apps/cluster/modules/ModulesSection");
const { packBarDetail, packOffReading } = await import("../../src/apps/cluster/modules/rows");
const { fakeConnection, withSession } = await import("./harness");
import type { SessionFacts } from "../../src/chrome/access";

type Conn = ReturnType<typeof fakeConnection>;

function mount(
  connection: Conn,
  role = "owner",
  readiness?: SessionFacts["readiness"],
) {
  h.connection = connection;
  return render(withSession(<ModulesSection />, { role, readiness }));
}

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

// Deliberately seeded in the WRONG order -- component first, pack last -- so
// a rendering that simply echoed the wire would pass nothing here.
const MODULES = [
  { kind: "component", name: "identity", state: "built_in", scope: "node", description: "The identity service." },
  // `storage` is also a READINESS module id, which is what makes the
  // cluster-wide column reachable in this suite.
  { kind: "component", name: "storage", state: "built_in", scope: "node", description: "Blob storage." },
  { kind: "node-type", name: "planner", state: "compiled_out", scope: "cluster", description: "Task planning." },
  { kind: "integration", name: "shopify", state: "credential_gated", scope: "node", description: "The Shopify connector." },
  { kind: "pack", name: "referencepack", state: "enabled", scope: "cluster", description: "The reference pack." },
];

const ENV = {
  "component/identity": [
    {
      name: "MEMQL_IDENTITY_SIGNING_KEY",
      description: "The private key the identity service signs JWTs with.",
      secret: true,
      set: true,
      // A VALUE ON A SECRET ENTRY, which the engine's contract says never
      // happens -- present here precisely so the assertion is about this
      // build's refusal to render one rather than about an empty field.
      value: "super-secret-key-material",
      scope: "node",
      requiredFor: ["identity"],
    },
    {
      name: "MEMQL_IDENTITY_BASE_URL",
      description: "Where the identity service is reachable.",
      secret: false,
      set: true,
      value: "https://identity.memql.example.com",
      defaultValue: "",
      scope: "node",
      requiredFor: [],
    },
    {
      name: "MEMQL_IDENTITY_UNSET_SECRET",
      description: "A secret nobody has configured.",
      secret: true,
      set: false,
      value: "",
      scope: "node",
      requiredFor: [],
    },
  ],
};

beforeEach(() => {
  h.connection = null;
});

describe("the modules inventory", () => {
  it("groups by kind in the fixed order, never alphabetically", async () => {
    mount(fakeConnection({}, { modules: MODULES }));
    await screen.findByText("referencepack");

    const headings = screen
      .getAllByRole("heading", { level: 4 })
      .map((el) => el.childNodes[0]?.textContent);
    // Pack -> integration -> node-type -> component. Alphabetical would put
    // Components first and Packs last, so this fails on a plain sort.
    expect(headings).toEqual(["Packs", "Integrations", "Node types", "Components"]);
  });

  it("names the node that answered, because another replica can answer differently", async () => {
    mount(
      fakeConnection({}, { modules: MODULES, reportingNodeId: "bff-7f2c", reportingNodeType: "bff" }),
    );
    expect(await screen.findByText(/answered by bff-7f2c \(bff\)/)).toBeTruthy();
  });

  it("renders a secret env var as set or unset and NEVER its value", async () => {
    mount(fakeConnection({}, { modules: MODULES, envVars: ENV }));
    await click(await screen.findByText("identity"));

    const list = await screen.findByRole("list", { name: "identity environment" });
    const secret = within(list)
      .getByText("MEMQL_IDENTITY_SIGNING_KEY")
      .closest(".os-cluster-env-row") as HTMLElement;
    expect(within(secret).getByText("set")).toBeTruthy();
    // THE REGRESSION THAT MATTERS: the value is on the wire and must not be
    // anywhere in the DOM. Asserted against the whole document rather than
    // the row, so a copy leaking into a title, a caption or a detail panel
    // fails too.
    expect(document.body.textContent).not.toContain("super-secret-key-material");

    const unset = within(list)
      .getByText("MEMQL_IDENTITY_UNSET_SECRET")
      .closest(".os-cluster-env-row") as HTMLElement;
    expect(within(unset).getByText("unset")).toBeTruthy();

    // A non-secret DOES show its resolved value -- otherwise this test would
    // pass against a page that renders no values at all.
    const plain = within(list)
      .getByText("MEMQL_IDENTITY_BASE_URL")
      .closest(".os-cluster-env-row") as HTMLElement;
    expect(within(plain).getByText("https://identity.memql.example.com")).toBeTruthy();

    // And the page says WHY the secret is two words rather than a value.
    expect(screen.getByText(/the value never leaves the engine/i)).toBeTruthy();
  });
});

describe("the pack switch", () => {
  it("says a restart is required, before and after the flip", async () => {
    const connection = fakeConnection({}, { modules: MODULES });
    mount(connection);
    await click(await screen.findByText("referencepack"));

    // Before: the bar's own detail line.
    expect(screen.getByText(/read by each node at its NEXT BOOT/i)).toBeTruthy();

    await click(screen.getByRole("button", { name: "Disable this pack" }));
    // The confirm is a step of its own: nothing was sent yet.
    const sentFlip = () =>
      connection.dispatcher.sendAndWait.mock.calls.some(
        (call) => "setPackEnabled" in (call[0] as Record<string, unknown>),
      );
    expect(sentFlip()).toBe(false);

    await click(screen.getByRole("button", { name: "Disable" }));
    // After: the outcome says the same thing in the past tense, and says
    // nothing running has changed.
    const written = await screen.findByText(/Nothing running has changed/);
    expect(written.textContent).toContain("NEXT BOOT");
    expect(sentFlip()).toBe(true);
  });

  it("is ABSENT for a non-owner, not disabled", async () => {
    mount(fakeConnection({}, { modules: MODULES }), "admin");
    await click(await screen.findByText("referencepack"));

    // Not "present and disabled" -- absent. A greyed control is one an admin
    // has to read past to learn it is not for them (DESIGN.md rule 12).
    expect(screen.queryByRole("button", { name: /this pack/i })).toBeNull();
    expect(screen.getByText(/Only a cluster owner can change what a pack does/)).toBeTruthy();
  });

  it("is ABSENT for an integration and for a node type, with a sentence saying what does change them", async () => {
    mount(fakeConnection({}, { modules: MODULES }));

    await click(await screen.findByText("shopify"));
    expect(screen.queryByRole("button", { name: /this pack/i })).toBeNull();
    expect(screen.getByText(/An integration has no switch/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Modules" }));

    await click(await screen.findByText("planner"));
    expect(screen.queryByRole("button", { name: /this pack/i })).toBeNull();
    expect(screen.getByText(/A node type has no switch/)).toBeTruthy();
  });
});

describe("the cluster-wide readiness column", () => {
  // THE CLUSTER-WIDE READING beside this node's own. The two are different
  // scopes and may honestly differ mid-rollout, so the column says WHICH kind
  // of partial it is -- nodes that differ read "Set up on some nodes" -- and
  // the nodes behind it are one hover away, never the raw nodeId=state pairs
  // that used to stretch a row into a list of pod names (memql#5259).
  it("names nodes that differ as set up on some nodes, with the nodes in the title", async () => {
    const readiness = {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: (id: string) =>
        id === "storage"
          ? {
              module: "storage",
              state: "partial" as const,
              core: true,
              disagreement: ["bff-b=unconfigured", "bff-a=configured"],
              nodes: [
                { nodeId: "bff-b", nodeType: "bff", state: "unconfigured" as const, reportedAt: "2026-09-13T11:58:00Z" },
                { nodeId: "bff-a", nodeType: "bff", state: "configured" as const, reportedAt: "2026-09-13T11:58:00Z" },
              ],
              lanes: [],
              unknown: [],
              stale: [],
              aside: [],
            }
          : null,
    };
    mount(fakeConnection({}, { modules: MODULES }), "owner", readiness);
    const chip = await screen.findByText("Set up on some nodes");
    expect(chip.getAttribute("title") ?? chip.closest("[title]")?.getAttribute("title")).toMatch(/Not set up on bff-b/);
    expect(screen.queryByText(/bff-b=unconfigured/)).toBeNull();
  });

  // A NODE THE FOLD SET ASIDE IS COUNTED, NOT VOTED. The verdict reads as it
  // would without it -- "Set up" -- and a quiet count says how many nodes are
  // behind or could not check, so the operator knows there is something to
  // read in the detail without a warning colour on a working module.
  it("counts the nodes the fold set aside beside an unchanged verdict", async () => {
    const readiness = {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: (id: string) =>
        id === "storage"
          ? {
              module: "storage",
              state: "configured" as const,
              core: true,
              disagreement: [],
              nodes: [{ nodeId: "agent-a", nodeType: "agent", state: "configured" as const, reportedAt: "2026-09-13T11:58:00Z" }],
              lanes: [],
              unknown: ["bff-b"],
              stale: ["edge-a", "identity-a"],
              aside: [],
            }
          : null,
    };
    mount(fakeConnection({}, { modules: MODULES }), "owner", readiness);
    expect(await screen.findByText("Set up")).toBeTruthy();
    // ONE label, the more actionable of the two; the rest is in the title.
    const label = screen.getByText("1 could not check");
    expect(label.getAttribute("title")).toMatch(/2 nodes catching up: edge-a, identity-a/);
    expect(screen.queryByText("2 catching up")).toBeNull();
  });

  // THE DETAIL OPENS THE FOLD UP (memql#5259): the module on every live
  // node, voters first, then -- quieter -- the node catching up with the time
  // it last checked, and the node that could not check with the reason.
  it("lists the module on every live node in the detail, set-aside nodes included", async () => {
    const readiness = {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: (id: string) =>
        id === "storage"
          ? {
              module: "storage",
              state: "configured" as const,
              core: true,
              disagreement: [],
              nodes: [{ nodeId: "agent-a", nodeType: "agent", state: "configured" as const, reportedAt: new Date().toISOString() }],
              lanes: [],
              unknown: ["bff-b"],
              stale: ["edge-a"],
              aside: [
                { nodeId: "edge-a", nodeType: "edge", state: "unconfigured" as const, reportedAt: new Date(Date.now() - 3 * 3600_000).toISOString(), why: "stale" as const },
                { nodeId: "bff-b", nodeType: "bff", state: "unknown" as const, reportedAt: new Date().toISOString(), reason: "fleetReadFailed", why: "unknown" as const },
              ],
            }
          : null,
    };
    mount(fakeConnection({}, { modules: MODULES }), "owner", readiness);
    await click(await screen.findByText("storage"));

    expect(await screen.findByText("Across the cluster")).toBeTruthy();
    expect(screen.getByText("Set up. The one node that counts says so.")).toBeTruthy();
    const list = screen.getByRole("list", { name: "storage on each live node" });
    const rows = Array.from(list.querySelectorAll(".os-record-row"));
    expect(rows.map((r) => r.querySelector(".os-row-name")?.textContent)).toEqual(["agent-a", "edge-a", "bff-b"]);
    // Counted and set aside are told apart in the markup, not only the words.
    expect(rows.map((r) => r.hasAttribute("data-dim"))).toEqual([false, true, true]);
    expect(within(rows[1] as HTMLElement).getByText("Catching up")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText(/last checked 3h ago/)).toBeTruthy();
    expect(within(rows[2] as HTMLElement).getByText(/the fleet could not be read/)).toBeTruthy();
    // Every row carries its exact reported moment, so an operator comparing
    // two nodes that both read "3h ago" has something to compare.
    for (const row of rows) {
      expect(row.querySelector(".os-record-status")?.getAttribute("title")).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    }
  });

  // memql#5325: a stopped node's rows are REMOVED, not merely uncounted, so
  // the list is the cluster rather than every pod that ever booted. The
  // caption is the only place a person is told that, and without it a short
  // list after a rollout is indistinguishable from a list that is hiding
  // something.
  it("says that a stopped node is gone from the list, not just uncounted", async () => {
    const readiness = {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: (id: string) =>
        id === "storage"
          ? {
              module: "storage",
              state: "configured" as const,
              core: true,
              disagreement: [],
              nodes: [{ nodeId: "agent-a", nodeType: "agent", state: "configured" as const, reportedAt: new Date().toISOString() }],
              lanes: [],
              unknown: [],
              stale: [],
              aside: [],
            }
          : null,
    };
    mount(fakeConnection({}, { modules: MODULES }), "owner", readiness);
    await click(await screen.findByText("storage"));

    expect(await screen.findByText("Across the cluster")).toBeTruthy();
    expect(screen.getByText(/A node that has stopped is not here at all/)).toBeTruthy();
    expect(screen.getByText(/this\s+list is the cluster now, not every pod that has ever run/)).toBeTruthy();
  });

  it("draws no cluster reading in the detail of a module readiness does not know", async () => {
    mount(fakeConnection({}, { modules: MODULES }), "owner", {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: () => null,
    });
    await click(await screen.findByText("identity"));
    await screen.findByRole("heading", { name: "identity" });
    expect(screen.queryByText("Across the cluster")).toBeNull();
  });

  // Most registry modules have no readiness counterpart, and drawing an
  // empty chip for them would put a column of nothing beside every row.
  it("draws no readiness chip for a module that has none", async () => {
    const readiness = {
      loaded: true,
      state: "live" as const,
      reseed: () => {},
      of: () => null,
    };
    mount(fakeConnection({}, { modules: MODULES }), "owner", readiness);
    await screen.findByText("referencepack");
    expect(screen.queryByText(/Partly set up|Not set up|Not reported/)).toBeNull();
  });
});

// ===========================================================================
// WHY A PACK IS OFF IS TWO DIFFERENT FACTS (epic memql#5532, issue memql#5549)
// ===========================================================================

describe("a pack's off state", () => {
  it("tells a pack that ships disabled from one an operator switched off", () => {
    expect(
      packOffReading(
        "no v1:platform:packState row; this pack ships DISABLED and stays mounted-inert " +
          "until an operator enables it",
      ),
    ).toContain("has not been switched on");

    expect(packOffReading("set by an operator in v1:platform:packState; reason: not needed")).toBe(
      "An operator switched this off for this instance.",
    );
  });

  it("says nothing when the engine said nothing this build recognises", () => {
    expect(packOffReading("")).toBe("");
    expect(packOffReading("some future wording")).toBe("");
  });

  it("puts what a flip DOES before when it lands", () => {
    const detail =
      "no v1:platform:packState row; when enabled, publishes 2 shopper route(s) on any " +
      "deployable whose shopperForms is on: form reviews/review, read reviews/published";
    const bar = packBarDetail(detail);

    expect(bar.indexOf("publishes 2 shopper route")).toBeLessThan(bar.indexOf("NEXT BOOT"));
    expect(bar).toContain("Nothing running changes until they restart");
  });

  it("still says when a flip lands for a pack the engine described with nothing", () => {
    expect(packBarDetail("")).toContain("NEXT BOOT");
    expect(packBarDetail("   ")).toContain("NEXT BOOT");
  });
});
