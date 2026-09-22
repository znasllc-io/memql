import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// =============================================================================
// NOTHING LOST (design decision D5, memql#5159)
// =============================================================================
// "Every act the current screens offer survives: the vendor forms and verify,
// the fleet door, the machine strategy fields, model pull, rename, labels,
// revoke. A test enumerates the current acts by accessible name and fails if
// one is missing after the redesign."
//
// This file is the Fleet half of that (the Settings half lives beside the
// screens it covers). It mounts each section, opens the disclosures, and
// compares the accessible names on screen against a written inventory.
//
// -----------------------------------------------------------------------------
// WHY IT COUNTS AND DOES NOT ONLY LOOK
// -----------------------------------------------------------------------------
// An earlier epic in this repo RELOCATED lifecycle acts and left the originals
// rendering, so each act existed TWICE on one page -- with the whole suite
// green. Both halves were individually correct: the test for the new location
// passed, and the test for the old one passed too, because the control was
// still there. Nothing in a suite asks "is this the ONLY one".
//
// So every entry below carries a COUNT, not a presence flag, and a count of
// zero is a real assertion: it is how the mutually exclusive states are
// pinned (the revoke confirm REPLACES its opener; the pull form is ABSENT
// while a pull runs; a refresh control never stands beside a live feed).
//
// -----------------------------------------------------------------------------
// A FLOOR, NOT A CEILING
// -----------------------------------------------------------------------------
// An accessible name that is NOT in a case's inventory is not a failure. The
// redesign is expected to add acts, and a test that fired on every addition
// would be reverted rather than read. What it refuses is an act named here
// going missing, or arriving a second time.
//
// -----------------------------------------------------------------------------
// COUNTS ARE PER MOUNTED SURFACE
// -----------------------------------------------------------------------------
// `Re-read` legitimately appears five times across this app (the call
// history, Routing, Workbenches twice, Apps) and `Discard changes` twice
// (Routing, Apps). Those are not duplicates -- they are one act offered by
// several surfaces, which is why the inventory is per case rather than per
// app, and why a document-wide `getByRole` would throw on both.

const h = vi.hoisted(() => ({ connection: null as unknown, mint: vi.fn() }));

// The connection is a module-level context read whose provider dials a real
// socket. Replacing the READ is what lets the real collections, the real
// retain path and the real projections run under jsdom -- the same seam every
// other test in this directory uses.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The mint is the one call the add-machine panel makes that is not a graph
// read; without it the token half of that panel is unreachable and four of
// its acts could never be swept.
vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  createWorkerToken: (...args: unknown[]) => h.mint(...args),
}));

const { MachinesProvider } = await import("../../src/live/machines");
const { ModelsSection } = await import("../../src/apps/fleet/models/ModelsSection");
const { RoutingSection } = await import("../../src/apps/fleet/routing/RoutingSection");
const { AppsSection } = await import("../../src/apps/fleet/apps/AppsSection");
const { WorkbenchesSection } = await import("../../src/apps/fleet/workbenches/WorkbenchesSection");
const { FleetApp } = await import("../../src/apps/fleet/FleetApp");
const { fakeConnection, machineRow, modelPullRow, delegationPolicyRow, withSession, MachinesWithFlow } = await import(
  "./harness"
);

type Conn = ReturnType<typeof fakeConnection>;

// -----------------------------------------------------------------------------
// Reading an act off the page
// -----------------------------------------------------------------------------

/**
 * Everything that can BE an act, plus the containers that name a group of
 * them.
 *
 * Written as element shapes rather than as roles, because this shell's
 * controls are not the platform's: a choice is a `<button role="radio">`, a
 * select is a `<button role="combobox">` with a visually-hidden `<label for>`,
 * a chip list is a `<div role="list">`. A role-only sweep would miss the ones
 * carrying no role attribute at all, and a `getByRole` sweep would resolve
 * names through a different implementation than the one below.
 */
const ACT_SELECTOR = [
  "button",
  '[role="radio"]',
  '[role="checkbox"]',
  '[role="combobox"]',
  "input",
  "textarea",
  '[role="group"]',
  '[role="status"]',
  '[role="progressbar"]',
  "ul[aria-label]",
  "section[aria-label]",
  // `Chips` is a `<div role="list">`, and `LiveList` a `<ul>`. Both name
  // themselves, and both are how a group of acts is found.
  '[role="list"][aria-label]',
  // Anything else that names itself. Deliberately last and deliberately wide:
  // this test's failure mode is a control it does not look at.
  "[aria-label]",
].join(", ");

/** Elements whose accessible name a `<label>` may supply. */
const LABELABLE = new Set(["INPUT", "TEXTAREA", "SELECT", "BUTTON", "METER", "OUTPUT", "PROGRESS"]);

function squash(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** The `<label>` naming this control -- `for=` first, then a wrapping one. */
function labelTextFor(el: Element): string {
  const id = el.getAttribute("id") ?? "";
  if (id !== "") {
    // Compared field by field rather than through a selector: ids here carry
    // canonical row ids (`fleet-rename-v1:worker:registration:live`), and a
    // `label[for="..."]` selector would need escaping to survive the colons.
    for (const candidate of document.querySelectorAll("label[for]")) {
      if ((candidate as HTMLLabelElement).htmlFor === id) return squash(candidate.textContent ?? "");
    }
  }
  const wrapping = el.closest("label");
  return wrapping ? squash(wrapping.textContent ?? "") : "";
}

/**
 * What this element is CALLED, to a person who is not looking at it.
 *
 * Computed here rather than taken from testing-library so the test can sweep
 * the whole surface in one pass and report every act at once -- a per-act
 * `getByRole` throws on the first duplicate and tells you nothing about the
 * other twenty.
 */
function accessibleName(el: Element): string {
  const aria = squash(el.getAttribute("aria-label") ?? "");
  if (aria !== "") return aria;

  const labelledBy = squash(el.getAttribute("aria-labelledby") ?? "");
  if (labelledBy !== "") {
    return squash(
      labelledBy
        .split(" ")
        .map((id) => document.getElementById(id)?.textContent ?? "")
        .join(" "),
    );
  }

  if (LABELABLE.has(el.tagName)) {
    const labelled = labelTextFor(el);
    if (labelled !== "") return labelled;
  }

  return squash(el.textContent ?? "");
}

interface Named {
  el: Element;
  name: string;
}

function namedActs(): Named[] {
  const out: Named[] = [];
  for (const el of document.querySelectorAll(ACT_SELECTOR)) {
    const name = accessibleName(el);
    // An unnamed element is not an act this test can talk about. The
    // announcement regions (`<p role="status">` carrying a sentence that is
    // empty until something is saved) are the population this drops.
    if (name === "") continue;
    out.push({ el, name });
  }
  return out;
}

// -----------------------------------------------------------------------------
// The inventory
// -----------------------------------------------------------------------------

interface Act {
  /**
   * The accessible name, spelled exactly.
   *
   * A RegExp is used only where the name IS a sentence -- the add-machine
   * checkboxes, whose label is a paragraph of copy. Pinning those word for
   * word would make this test fire on a copy edit, which is not the thing D5
   * protects: the act is the checkbox, and what is pinned is the clause that
   * says which checkbox it is.
   */
  name: string | RegExp;
  /** How many elements must carry it. ZERO IS AN ASSERTION -- see the header. */
  count: number;
  /** Count only inside the element with this accessible name (a radiogroup). */
  within?: string;
  /** Why, when the entry is not self-explanatory. */
  note?: string;
}

interface ActsCase {
  /** Names the mounted surface, and appears in every failure from it. */
  what: string;
  /** Mounts it and opens whatever the acts sit behind. */
  open: () => Promise<void>;
  acts: Act[];
}

function matches(act: Act, name: string): boolean {
  return typeof act.name === "string" ? name === act.name : act.name.test(name);
}

function scopeFor(within: string, all: Named[]): Element[] {
  return all.filter((one) => one.name === within).map((one) => one.el);
}

/** Every complaint this case has, in one list. */
function auditCase(one: ActsCase): string[] {
  const all = namedActs();
  const problems: string[] = [];

  for (const act of one.acts) {
    let population = all;
    if (act.within !== undefined) {
      const scopes = scopeFor(act.within, all);
      if (scopes.length === 0) {
        problems.push(`${one.what}: no element named "${act.within}" to look inside`);
        continue;
      }
      population = all.filter((candidate) =>
        scopes.some((scope) => scope !== candidate.el && scope.contains(candidate.el)),
      );
    }
    const found = population.filter((candidate) => matches(act, candidate.name)).length;
    if (found === act.count) continue;
    const where = act.within === undefined ? "" : ` inside "${act.within}"`;
    const spelling = typeof act.name === "string" ? JSON.stringify(act.name) : String(act.name);
    problems.push(
      `${one.what}: ${spelling}${where} -- expected ${act.count}, found ${found}` +
        (act.note ? ` (${act.note})` : ""),
    );
  }

  if (problems.length > 0) {
    // The names actually on screen, once per failing case. A bare count
    // mismatch is useless to whoever broke it: the usual cause is a rename,
    // and the new spelling is right here.
    problems.push(
      `${one.what}: what IS on screen -- ${[...new Set(all.map((n) => n.name))]
        .map((n) => JSON.stringify(n.length > 60 ? `${n.slice(0, 60)}...` : n))
        .sort()
        .join(", ")}`,
    );
  }
  return problems;
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

const MACHINE_ID = "v1:worker:registration:live";
const MACHINE_LABEL = "Studio mini";

const MACHINE = machineRow({
  id: MACHINE_ID,
  displayName: MACHINE_LABEL,
  labels: {
    os: "darwin",
    "runtime:ollama": "1",
    "model:llama3.1:8b": "ctx=131072,structured=1,tools=1,params=8000000000,quant=Q4_K_M",
  },
  // One operator label, so the per-chip Remove act has something to remove.
  operatorLabels: { tier: "gold" },
  apps: [
    { id: "claude-code", version: "1.2.3", allowed: true, signedIn: true, subscription: "present" },
  ],
});

const POLICY = {
  id: "v1:worker:routingPolicy:p1",
  ownerUserId: "v1:identity:user:me",
  active: true,
  strategy: "leastLoaded",
  fallback: "nextMatching",
  requireLabels: { tier: "gold" },
  preferLabels: { room: "studio" },
  modelPreference: ["llama3.1:8b"],
};

const CATALOG_MODEL = {
  id: "llama3.1:8b",
  modelId: "llama3.1:8b",
  params: 8_000_000_000,
  contextWindow: 131072,
  structuredOutput: true,
  tools: true,
  embeddings: false,
  online: true,
  quant: "Q4_K_M",
  machineCount: 1,
  onlineCount: 1,
  machines: [],
};

const DOORS = {
  id: "v1:platform:inferenceStatus:self",
  eligible: true,
  doorsOpen: ["local"],
  localEligible: true,
  localModelCount: 1,
  eligibleModelIds: ["llama3.1:8b"],
  appEligible: false,
  runnableApps: [],
  appSessionsInstalled: true,
  cloudConfigured: false,
  federationConfigured: false,
  fleetInferenceInstalled: false,
  fleetCatalogInstalled: true,
  minimumContextWindow: 8192,
};

const WORKSPACE = {
  id: "v1:workbench:workspace:a",
  runId: "v1:work:run:1",
  nodeId: "workbench-0",
  status: "provisioned",
  storageRoot: "/var/lib/memql/workbenches/1",
  createdAt: "2026-08-29T09:00:00Z",
};

const WORKBENCH_NODE = {
  id: "workbench-0",
  nodeType: "workbench",
  health: "healthy",
  createdAt: "2026-08-01T00:00:00Z",
};

// ASSEMBLED FROM PARTS, as test/fleet/addMachine.test.tsx is: the repo's
// secret scanner matches `mql_<kind>_<43 base64url chars>` as one literal, and
// joining at runtime means no line in this file can ever match.
const TOKEN = ["mql", "wkr", "notARealTokenOnlyATestFixture"].join("_");

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

async function type(el: HTMLInputElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** Let the on-demand reads and the seeds land before the sweep looks. */
async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

function mountMachines(connection: Conn) {
  h.connection = connection;
  render(
    withSession(
      <MachinesProvider>
        <MachinesWithFlow showRevoked={false} />
      </MachinesProvider>,
    ),
  );
}

/** Mount the directory and open one machine's detail. */
async function openMachineDetail(connection: Conn) {
  mountMachines(connection);
  await click(await screen.findByText(MACHINE_LABEL));
  await settle();
}

// -----------------------------------------------------------------------------
// The cases
// -----------------------------------------------------------------------------

const CASES: ActsCase[] = [
  {
    what: "Machines -- the directory",
    open: async () => {
      mountMachines(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
      await screen.findByText(MACHINE_LABEL);
    },
    acts: [
      { name: "Add a machine", count: 1, note: "the Head's one act, before it opens the guided install" },
      { name: "Your machines", count: 1 },
      { name: `Labels on ${MACHINE_LABEL}`, count: 1 },
    ],
  },

  {
    what: "Machines -- Add a machine, before the mint",
    open: async () => {
      mountMachines(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
      await screen.findByText(MACHINE_LABEL);
      await click(screen.getByLabelText("Add a machine"));
      // Mint is ABSENT until there is a name (interface rule 12): the engine
      // refuses a nameless mint, so it is not offered. Typing one is what
      // makes the act appear, and the sweep asserts both halves.
      await type(screen.getByLabelText("Machine name") as HTMLInputElement, "mini");
    },
    acts: [
      {
        name: "Add a machine",
        count: 1,
        note: "the page's own region; the Head's act went with the list it opened over (design record 2026-09-08-cockpit-install-wizard, D1)",
      },
      { name: "Back to Machines", count: 1 },
      { name: "Machine name", count: 1 },
      { name: "Operating system", count: 1 },
      { name: "Mint a token", count: 1 },
      { name: "Cancel", count: 1, note: "always reachable while there is something to cancel (D6)" },
      { name: /^Computer use$/, count: 1 },
      { name: /^Run local models$/, count: 1 },
      // The token half is behind a successful mint, and offering any of it
      // before one would be an act with nothing to act on.
      { name: "Copy the worker token", count: 0 },
      { name: "Copy the install command", count: 0 },
      { name: "Done", count: 0 },
    ],
  },

  {
    what: "Machines -- Add a machine, the token minted",
    open: async () => {
      mountMachines(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
      await screen.findByText(MACHINE_LABEL);
      await click(screen.getByLabelText("Add a machine"));
      await type(screen.getByLabelText("Machine name") as HTMLInputElement, "mini");
      await click(screen.getByRole("button", { name: "Mint a token" }));
      await settle();
    },
    acts: [
      { name: "the worker token", count: 1, note: "the copy field itself" },
      { name: "Copy the worker token", count: 1 },
      { name: "the install command", count: 1 },
      { name: "Copy the install command", count: 1 },
      { name: "Cancel", count: 1, note: "the one act while the cluster listens; after a mint it asks Keep or Revoke" },
      { name: "Back to Machines", count: 1, note: "asks the same question Cancel does" },
      // GONE WITH THE PANEL, deliberately: the acknowledgement box that gated
      // Done is replaced by the cancel question (D6), Done is legal only once
      // the machine has registered, and a second mint is a second credential
      // that this flow does not offer while the first is waiting.
      { name: /^I have copied the token\b/, count: 0 },
      { name: "Done", count: 0 },
      { name: "Mint a token", count: 0 },
    ],
  },

  {
    what: "Machines -- one machine's detail",
    open: async () => {
      await openMachineDetail(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
      // The call history is a disclosure and the read runs when it opens. A
      // sweep that does not click it misses Re-read entirely -- which is how
      // a real defect survived a twenty-four-route sweep in this repo.
      await click(screen.getByLabelText(`Recent calls on ${MACHINE_LABEL}`));
      await settle();
    },
    acts: [
      { name: `${MACHINE_LABEL} detail`, count: 1 },
      { name: `Name for ${MACHINE_LABEL}`, count: 1 },
      { name: "Rename", count: 1 },
      { name: "Reported labels", count: 1, note: "shown, and deliberately carrying no edit act" },
      {
        name: "Operator labels",
        count: 2,
        note: "the chip list and the draft field, both named for the map they edit",
      },
      { name: "Add", count: 1 },
      { name: "Remove tier=gold", count: 1 },
      { name: `Model to pull onto ${MACHINE_LABEL}`, count: 1 },
      { name: "Pull", count: 1 },
      { name: `Recent calls on ${MACHINE_LABEL}`, count: 1 },
      { name: "Refresh recent calls", count: 1, note: "the call history's own; telemetry is not broadcast" },
      { name: "Remove this machine", count: 1, note: "revoke, then the uninstall line (D12)" },
      { name: "Copy the uninstall command", count: 0, note: "offered inside the confirm, not beside the opener" },

      // The scanner's four (epic memql#5146), moved here from the
      // ARRIVING_WITH_5146 list now that they exist -- which is the edit that
      // list existed to force. It is DELETED rather than emptied: an empty
      // list would leave a test passing over nothing, which its own header
      // forbade.
      //
      // Two of the four are ZERO, and the zeros are the assertions worth
      // having.
      {
        name: "Probe this model",
        count: 1,
        note: "one per advertised model, and this fixture advertises one",
      },
      {
        name: "Share with the cluster",
        count: 1,
        note: "the owner's half of the two consents; the machine's half is a file on the machine",
      },
      {
        name: "Stop sharing with the cluster",
        count: 0,
        note: "the other half of the same toggle -- this machine is not shared, so it replaces nothing here",
      },
      {
        name: "Pull recommended set",
        count: 0,
        note: "absent because this fixture's catalog read returns no recommended entries, not because the act is gone -- an act with nothing to do does not render",
      },

      // The confirmation REPLACES its opener rather than sitting beside it.
      { name: `Removing ${MACHINE_LABEL}`, count: 0 },
      { name: "Reason (optional)", count: 0 },
      { name: "Keep it", count: 0 },
    ],
  },

  {
    what: "Machines -- the remove confirmation",
    open: async () => {
      await openMachineDetail(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
      await click(screen.getByRole("button", { name: "Remove this machine" }));
    },
    acts: [
      {
        name: `Remove ${MACHINE_LABEL}`,
        count: 1,
        note: "the confirm group NAMES the machine, which is the point",
      },
      { name: `Removing ${MACHINE_LABEL}`, count: 1, note: "the confirm REGION, named for the state it puts the page in -- the danger button inside it is `Remove <machine>`, and two controls must not answer to one accessible name" },
      { name: "Reason (optional)", count: 1 },
      { name: "Keep it", count: 1 },
      { name: "the uninstall command", count: 1, note: "the machine's half of the act, as a copy field (D12)" },
      { name: "Copy the uninstall command", count: 1 },
      { name: "Remove this machine", count: 0, note: "replaced by the confirm, never beside it" },
    ],
  },

  {
    what: "Machines -- a pull in flight",
    open: async () => {
      await openMachineDetail(
        fakeConnection({
          myWorkersWithStatus: [MACHINE],
          modelPullsForWorker: [
            modelPullRow({
              id: "v1:worker:modelPull:p1",
              workerId: MACHINE_ID,
              status: "running",
              statusLine: "pulling manifest",
              // Stated by the runtime, so the step's bar is drawn. Left at
              // its default zero the bar is ABSENT rather than empty, which
              // is a different fact and has its own test.
              totalBytes: 100,
              completedBytes: 40,
            }),
          ],
        }),
      );
    },
    acts: [
      { name: "Pulling llama3.1:8b", count: 1 },
      { name: "Current step of llama3.1:8b", count: 1 },
      // One pull at a time per machine, and the form is ABSENT rather than
      // disabled while one runs.
      { name: `Model to pull onto ${MACHINE_LABEL}`, count: 0 },
      { name: "Pull", count: 0 },
    ],
  },

  {
    what: "Models",
    open: async () => {
      h.connection = fakeConnection({
        fleetModels: [CATALOG_MODEL],
        inferenceStatus: [DOORS],
        // The preference chips are read off the LIVE routing policy, so the
        // ordering act only exists when there is one.
        myRoutingPolicies: [POLICY],
      });
      render(withSession(<ModelsSection />));
      await settle();
      await settle();
    },
    acts: [
      { name: "Refresh model library", count: 1 },
      { name: "Inference sources", count: 2, note: "the navigation destination and its named panel" },
      { name: "Your preferred order", count: 1 },
      { name: "Capabilities", count: 1, note: "one per model shown; the fixture has one model" },
    ],
  },

  {
    // THE CASE THIS FILE EXISTS FOR. Routing is being restyled, and its acts
    // have to come out the other side spelled identically.
    what: "Routing -- a saved policy",
    open: async () => {
      h.connection = fakeConnection({ myRoutingPolicies: [POLICY] });
      render(withSession(<RoutingSection />));
      await settle();
    },
    acts: [
      { name: "About Machine routing", count: 1 },

      { name: "Routing strategy", count: 1 },

      { name: "Routing fallback", count: 1 },

      {
        name: "Required labels",
        count: 2,
        note: "the chip list and the draft field",
      },
      { name: "Preferred labels", count: 2 },
      { name: "Preferred model order, one model id per line", count: 1 },
      { name: "Add", count: 2, note: "one per label map, and they must stay two" },
      { name: "Remove tier=gold", count: 1 },
      { name: "Remove room=studio", count: 1 },
      { name: "Save policy", count: 1 },
      { name: "Create policy", count: 0, note: "an existing row is edited in place" },
      {
        name: "Discard changes",
        count: 1,
        note: "offered (disabled) with nothing touched -- a disabled control is still the act",
      },
      {
        name: "Re-read",
        count: 0,
        note: "never beside a live feed; the routing policy broadcasts",
      },
    ],
  },

  {
    // The same section with its feed behind. `Re-read` is offered ONLY here,
    // and its appearance is itself the signal -- so a sweep of the live state
    // alone would report the act missing and a sweep of this state alone
    // would miss that it is conditional.
    //
    // A null connection is what puts the feed in `disconnected`, which is
    // `feedIsBehind` by that function's own definition. The alternative --
    // degrading a live collection -- is unreachable from the surface: the
    // only thing that re-seeds it is the very button under test.
    what: "Routing -- the feed behind",
    open: async () => {
      h.connection = null;
      render(withSession(<RoutingSection />));
      await settle();
    },
    acts: [
      { name: "Reconnect machine routing", count: 1 },
      { name: "Create policy", count: 1, note: "no row read, so the first save creates one" },
      { name: "Save policy", count: 0 },
      { name: "Routing strategy", count: 1 },
      { name: "Routing fallback", count: 1 },
      { name: "Discard changes", count: 1 },
    ],
  },

  {
    what: "Apps -- a saved delegation policy",
    open: async () => {
      h.connection = fakeConnection({ delegationPolicyForUser: [delegationPolicyRow()] });
      render(withSession(<AppsSection />));
      await settle();
      await settle();
    },
    acts: [
      {
        name: "Refresh app activity",
        count: 1,
        note: "standing here, unlike Routing: neither read on this screen is live",
      },
      { name: "Delegation", count: 2 },
      { name: "Delegate eligible tasks to my local apps", count: 1 },
      { name: "Claude Code", count: 1 },
      { name: "Codex", count: 1 },
      { name: "Run commands", count: 1 },
      { name: "Process files", count: 1 },
      { name: "Use tools", count: 1 },
      { name: "Save results", count: 1 },
      { name: "Most sessions at once", count: 1 },
      { name: "Workspace root on the machine", count: 1 },
      { name: "Save delegation policy", count: 1 },
      { name: "Turn delegation on", count: 0 },
      { name: "Discard changes", count: 1 },
      { name: "Delegated runs", count: 1 },
    ],
  },

  {
    // The absent row is the common case, and the save button says so.
    what: "Apps -- no delegation policy yet",
    open: async () => {
      h.connection = fakeConnection({ delegationPolicyForUser: [] });
      render(withSession(<AppsSection />));
      await settle();
      await settle();
    },
    acts: [
      { name: "Turn delegation on", count: 0 },
      { name: "Save delegation policy", count: 1 },
      { name: "Discard changes", count: 1 },
    ],
  },

  {
    what: "Workbenches -- both feeds live",
    open: async () => {
      h.connection = fakeConnection({ myWorkspaces: [WORKSPACE], clusterNodes: [WORKBENCH_NODE] });
      render(withSession(<WorkbenchesSection />));
      await screen.findAllByText("v1:work:run:1");
    },
    acts: [
      { name: "Show released", count: 1 },
      {
        name: "Workbench replicas",
        count: 2,
        note: "the panel and the list inside it; the panel is what the replicas are FOR",
      },
      { name: "Your workspaces", count: 1 },
      { name: "Re-read", count: 0, note: "a refresh control beside a live list contradicts it" },
    ],
  },

  {
    what: "Workbenches -- the feeds behind",
    open: async () => {
      h.connection = null;
      render(withSession(<WorkbenchesSection />));
      await settle();
    },
    acts: [
      {
        name: /^Reconnect (workspaces|replicas)$/,
        count: 2,
        note: "one per feed -- workspaces and replicas go behind independently",
      },
      { name: "Show released", count: 1 },
      { name: "Workbench replicas", count: 2 },
      { name: "Your workspaces", count: 1 },
    ],
  },

  {
    what: "Fleet settings",
    open: async () => {
      h.connection = fakeConnection();
      render(
        withSession(
          <FleetApp
            sectionId="settings"
            navigate={vi.fn()}
            askContext={vi.fn()}
            store={{
              load: () => ({ version: 1 as const, defaultSection: "machines", showRevoked: false }),
              save: () => {},
            }}
          />,
        ),
      );
      await settle();
    },
    acts: [
      { name: "Fleet settings", count: 1 },
      { name: "Default section", count: 1 },
      // The choices themselves. The manifest's section ORDER is gated in
      // app.test.tsx; what is gated here is that each one is still a thing a
      // person can pick.
      { name: "Machines", count: 1, within: "Default section" },
      { name: "Model library", count: 1, within: "Default section" },
      { name: "Machine routing", count: 1, within: "Default section" },
      { name: "Workspaces", count: 1, within: "Default section" },
      { name: "Activity", count: 1, within: "Default section" },
      { name: "Logs", count: 1, within: "Default section" },
      { name: "Settings", count: 1, within: "Default section" },
      { name: "List revoked machines", count: 1 },
    ],
  },
];

// -----------------------------------------------------------------------------

beforeEach(() => {
  h.connection = null;
  h.mint.mockReset();
  h.mint.mockResolvedValue({
    success: true,
    plainToken: TOKEN,
    identityId: "v1:identity:identity:1",
    ownerUserId: "v1:identity:user:me",
    errorCode: "",
    errorMessage: "",
  });
  globalThis.localStorage.clear();
});

afterEach(cleanup);

describe("every act the Fleet offers (D5, memql#5159)", () => {
  for (const one of CASES) {
    it(`keeps every act on: ${one.what}`, async () => {
      await one.open();
      expect(auditCase(one)).toEqual([]);
    });
  }

  it("sweeps a surface that actually has acts on it", async () => {
    // THE CONTROL. Every assertion above is a comparison against a count, and
    // a sweep that returned nothing would satisfy every count of zero and
    // report the rest as missing -- but a sweep that silently stopped MATCHING
    // (a broken accessible-name reading, say) would still report the missing
    // ones, so the failure would look like a redesign rather than like a
    // broken instrument. This is the assertion that tells those apart.
    await openMachineDetail(fakeConnection({ myWorkersWithStatus: [MACHINE] }));
    const names = namedActs().map((one) => one.name);
    expect(names.length).toBeGreaterThan(10);
    // Named through three different mechanisms -- an aria-label, a
    // visually-hidden `<label for>`, and a button's own text -- so a reading
    // that lost any one of them fails here rather than somewhere it reads as
    // a missing control.
    expect(names).toContain(`Recent calls on ${MACHINE_LABEL}`);
    expect(names).toContain(`Name for ${MACHINE_LABEL}`);
    expect(names).toContain("Rename");
  });

  it("fires when an act goes missing, and again when one arrives twice", async () => {
    // THE OTHER HALF OF THE CONTROL, and the one that matters most: a test
    // whose expectations were written FROM the screen passes whatever the
    // screen does. This drives the detector against a surface that has been
    // broken on purpose, both ways, so a green run above means the acts are
    // there rather than that nothing is being compared.
    const detail = CASES.find((one) => one.what === "Machines -- one machine's detail")!;
    await detail.open();
    expect(auditCase(detail)).toEqual([]);

    // MISSING -- an act relocated by the redesign and not brought along.
    const rename = screen.getByRole("button", { name: "Rename" });
    const parent = rename.parentElement!;
    rename.remove();
    expect(auditCase(detail).join("\n")).toContain('"Rename" -- expected 1, found 0');

    // TWICE -- the exact defect these counts exist for: the act relocated
    // AND the original left rendering. Presence-only assertions pass on both
    // copies, which is how it survived a green suite of sixteen hundred
    // tests. The two here are indistinguishable by anything but a count.
    parent.append(rename, rename.cloneNode(true));
    expect(auditCase(detail).join("\n")).toContain('"Rename" -- expected 1, found 2');
  });
});


describe("model availability through a BFF catalog", () => {
  it("shows catalog availability without requiring local dispatch", async () => {
    h.connection = fakeConnection({ inferenceStatus: [{
      ...DOORS, eligible: false, localEligible: false, localModelCount: 0,
      eligibleModelIds: [], fleetCatalogInstalled: true, fleetInferenceInstalled: false,
    }] });
    render(withSession(<ModelsSection />));
    await screen.findByText("your fleet offers no models");
    expect(screen.queryByText(/cannot place fleet calls/)).toBeNull();
  });

  it("distinguishes unreadable inventory from no eligible models", async () => {
    h.connection = fakeConnection({ inferenceStatus: [{
      ...DOORS, eligible: false, localEligible: false, localModelCount: 0,
      eligibleModelIds: [], fleetCatalogInstalled: false, fleetInferenceInstalled: true,
    }] });
    render(withSession(<ModelsSection />));
    await screen.findByText("fleet inventory cannot be read here");
    expect(screen.queryByText("your fleet offers no models")).toBeNull();
  });
});
