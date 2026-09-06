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
const { fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn, clusterRole = "owner") {
  h.connection = connection;
  return render(withSession(<ModulesSection />, { clusterRole }));
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
      .map((el) => el.textContent);
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
