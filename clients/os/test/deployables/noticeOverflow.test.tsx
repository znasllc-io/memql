import { render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, siteRow, withSession, type FakeSeed } from "./harness";

// A REFUSAL MUST NOT WIDEN THE PANE (reported from production with a photo).
//
// ===========================================================================
// WHAT WENT WRONG, AND WHY THE GUARD IS WHERE IT IS
// ===========================================================================
// `.os-rail-answer` is `white-space: nowrap` so a settled stop reads as one
// line, and it carries `min-width: 0` + `text-overflow: ellipsis` so it can
// ellipsize. That is enough for a stop whose answer is a hostname. It is not
// enough for a STOPPED stop, whose answer is the engine's whole refusal
// sentence -- because the ellipsis shrinks the ANSWER inside the button,
// while `.os-rail-line`, the button, was a grid item of `.os-rail-body` with
// `min-width: auto` and went on claiming its min-content. The stage's `1fr`
// track was pinned at the width of the sentence (~900px), `.os-deploy-scroll`
// grew a horizontal scrollbar, and everything stretched to that track came
// with it -- most visibly the refusal Notice under the stop, whose detail sat
// on one unwrapped monospace line ~440px past the panel's right edge while
// the prose beside it wrapped normally.
//
// ===========================================================================
// WHY THIS TEST ASSERTS DECLARATIONS AND NOT PIXELS
// ===========================================================================
// jsdom performs NO layout: every width here is 0 and nothing wraps, so a
// test that measured would pass against the broken stylesheet just as
// happily. What it CAN pin is the two declarations the fix rests on and the
// classes the markup actually reaches them through -- so removing either
// guard, or renaming the element out from under it, fails here.
//
// The pixels were established in a real browser against this page's own
// markup, at 440/520/720/900/1200px: before, `.os-deploy-scroll` overflowed
// by up to 452px; after, 0 at every width, with the detail wrapping to the
// pane. Both guards were confirmed load-bearing by removing each in turn.

const MSG =
  'deployable "web" has never been deployed and no hostname was chosen for it. The first deploy picks a hostname; later ones remember it.';

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

/**
 * Every declaration a selector carries, from EVERY rule that names it.
 *
 * The same shape `test/files/inspector.test.tsx` uses, and for its reason: a
 * selector legitimately appears more than once, so reading only the first
 * block would report a guard missing while it sits further down. Anchoring on
 * a line start is what keeps `.os-notice-detail` from matching inside
 * `.os-logs-day > .os-notice-detail`, which is a different rule.
 */
function block(selector: string): string {
  const rules: string[] = [];
  let from = 0;
  for (;;) {
    const at = CSS.indexOf(`\n${selector} {`, from);
    if (at === -1) break;
    const open = CSS.indexOf("{", at);
    const close = CSS.indexOf("}", open);
    rules.push(CSS.slice(open + 1, close));
    from = close;
  }
  if (rules.length === 0) throw new Error(`no rule in index.css for selector: ${selector}`);
  return rules.join("\n");
}

function memStore() {
  const data = new Map<string, string>();
  data.set(
    "memql-os-deployables-v1",
    JSON.stringify({ version: 1, density: "comfortable", expandedSources: ["pkg:pkg-acme"] }),
  );
  return new LocalDeployablesSettingsStore({
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
    removeItem: (k: string) => void data.delete(k),
  } as unknown as Storage);
}

const ACME: Row = {
  id: "pkg-acme",
  ownerUserId: "u-me",
  name: "acme",
  sourceKind: "repo",
  repoUrl: "https://github.com/acme/storefront",
  repoRef: "main",
  credentialId: "",
  artifactId: "",
  deployedVersion: "aaaaaaaaaaaaaaaaaaaa",
  latestKnownVersion: "aaaaaaaaaaaaaaaaaaaa",
  updateAvailable: false,
  status: "active",
  createdAt: "2026-09-01T10:00:00Z",
};

const STORE = siteRow({
  id: "site-store",
  hostname: "store.memql.example.com",
  kind: "spa",
  status: "live",
  bundleRef: "blob://sites/site-store/v2/",
  packageId: "pkg-acme",
  packageDeployableName: "storefront",
  createdAt: "2026-09-01T12:01:00Z",
});

/** The run that refused, carrying the sentence from the production report. */
const REFUSED: Row = {
  id: "dep-refused",
  packageId: "pkg-acme",
  sourceVersion: "cccccccccccccccccccc",
  status: "refused",
  report: {
    name: "acme",
    formatVersion: 1,
    deployables: [
      { name: "storefront", kind: "spa", path: "clients/web", buildPlan: "already built: dist", output: "dist", prebuilt: true },
    ],
    dslDomains: [],
    problems: [],
    ok: true,
  },
  dslVersion: "",
  deployables: [],
  snapshotArtifactId: "",
  buildLogTail: "",
  error: { code: "deployable_hostname_unchosen", message: MSG, scope: "web" },
  requestedBy: "u-me",
  builtOn: null,
  automatic: false,
  nodeId: "bff-1",
  stoppedAt: "",
  heartbeatAt: "",
  startedAt: "2026-09-01T13:00:00Z",
  finishedAt: "2026-09-01T13:00:30Z",
  createdAt: "2026-09-01T13:00:00Z",
};

const SEED: FakeSeed = { sites: [STORE], packages: [ACME], deployments: { "pkg-acme": [REFUSED] } };

/** Opens the deployable and the one stop the refusal stopped at. */
async function openRefusedStop(): Promise<HTMLElement> {
  h.connection = fakeConnection(SEED);
  render(
    withSession(
      <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: "owner", userId: "u-me" },
    ),
  );
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  await click((await screen.findByText("store.memql.example.com")).closest("button"));
  const page = await screen.findByRole("region", { name: "Deployable store.memql.example.com" });
  const line = within(page)
    .getAllByRole("button")
    .find((b) => b.classList.contains("os-rail-line") && (b.textContent ?? "").startsWith("What it is"));
  if (line === undefined) throw new Error("no rail stop named What it is");
  // EXACTLY ONE STOP IS OPEN, and the rail opens the stopped one by itself --
  // so an unconditional click CLOSES the stop this test came to read.
  if (line.getAttribute("aria-expanded") !== "true") await click(line);
  return page;
}

describe("a refusal never widens the deployable pane", () => {
  it("lets the rail line shrink below the sentence it carries", () => {
    // THE FIX. Without it the button holds the stage's `1fr` track open at
    // the sentence's own width, and no amount of ellipsis on the answer
    // inside it helps -- shrinking a child never shrinks the parent that is
    // holding the width.
    expect(block(".os-rail-line")).toMatch(/min-width:\s*0\s*;/);
  });

  it("keeps the nowrap answer paired with its ellipsis and its own shrink guard", () => {
    // The answer is allowed to be one line ONLY because it can ellipsize.
    // Dropping either half turns a long answer back into a width somebody's
    // container has to find.
    const answer = block(".os-rail-answer");
    expect(answer).toMatch(/white-space:\s*nowrap\s*;/);
    expect(answer).toMatch(/text-overflow:\s*ellipsis\s*;/);
    expect(answer).toMatch(/min-width:\s*0\s*;/);
  });

  it("breaks an unbreakable run in a notice detail rather than growing for it", () => {
    // `anywhere` and not `break-word`: only `anywhere` lowers MIN-CONTENT,
    // and min-content is what the container has to accommodate. A refusal
    // routinely carries one unbreakable run -- a hostname, a bundleRef, an
    // id -- and with `break-word` the glyphs wrap while the box stays wide.
    expect(block(".os-notice-detail")).toMatch(/overflow-wrap:\s*anywhere\s*;/);
  });

  it("renders the refusal through the classes those rules govern", async () => {
    // The declarations above are worth nothing if the markup stops reaching
    // them. This is the half jsdom CAN see: the sentence arrives in a
    // `.os-notice-detail`, and in a `.os-rail-answer` inside a
    // `.os-rail-line` -- the two elements the fix is about.
    const page = await openRefusedStop();

    const detail = page.querySelector(".os-notice-detail");
    expect(detail?.textContent).toBe(MSG);

    // Scoped to the STOPPED stage: every stop has an answer, and the first on
    // the page is Source's hostname, which was never the one holding the pane
    // open.
    const answer = page.querySelector('.os-rail-stage[data-state="stopped"] .os-rail-answer');
    expect(answer?.textContent).toBe(MSG);
    expect(answer?.closest(".os-rail-line")).not.toBeNull();

    // And the detail is inside the stop body, which is the grid track the
    // line was holding open -- that adjacency is the whole bug.
    expect(detail?.closest(".os-stop-body")).not.toBeNull();
  });
});
