// Availability on the Deployments surfaces (memql#3996).
//
// The Deployments heading, the cluster page and the version picker are where
// an operator looks at a cluster they are about to move, so this is where "a
// newer release exists" has to be visible -- and nowhere may "we do not know"
// read as "there is nothing newer". All three read the SAME describeVersion
// decision the Clusters surfaces read (memql#3995).
//
// WHAT CHANGED WITH THE REDESIGN. The page no longer carries a "latest" fact
// that printed "not fetched": a fact that is not known is not a row. It says
// "Up to date" when that is KNOWN, offers the update on its bar when one can be
// taken, and says a newer release needs manual steps when it cannot. When the
// listing has not been fetched, it claims nothing either way.

import test from "node:test";
import assert from "node:assert/strict";

import { localOverviewBar } from "../src/deploy/instanceActions.js";
import type { UpgradeVerdict } from "../src/deploy/upgrade.js";
import { selectedViewDescription } from "../src/state/deploymentsCatalog.js";
import type { Instance } from "../src/state/deployments.js";
import type { ReleaseListing } from "../src/version/releaseCache.js";
import { OTHER_VERSION, chooseVersionScreen, localOverviewScreen } from "../src/webview/deploymentScreens.js";

const listing = (tags: string[], error?: string): ReleaseListing => ({ tags, error, fetchedAt: 1000 });

const KNOWN = listing(["v0.19.0", "v0.18.0", "v0.17.1"]);

const healthy = (version?: string): Instance => ({
  name: "local",
  kind: "local",
  presence: "installed-healthy",
  connected: true,
  registered: true,
  ...(version === undefined ? {} : { version, versionLabel: version }),
});

// --- the heading ------------------------------------------------------------

test("a behind cluster's heading says the newer release is available", () => {
  assert.equal(selectedViewDescription(healthy("v0.18.0"), KNOWN, "connected"), "local · Connected · v0.18.0 · v0.19.0 available");
});

test("a current, ahead, unfetched or build-stamped cluster claims nothing about releases", () => {
  for (const [version, releases] of [
    ["v0.19.0", KNOWN],
    ["v0.20.0", KNOWN],
    ["v0.18.0", listing([], "git ls-remote: network unreachable")],
    ["v0.18.0", undefined],
    ["0.15.0-1737072000", KNOWN],
  ] as const) {
    assert.doesNotMatch(selectedViewDescription(healthy(version), releases, "connected"), /available/, version);
  }
});

test("a machine with nothing installed says nothing about versions", () => {
  assert.equal(selectedViewDescription({ ...healthy(), presence: "absent" }, KNOWN), "local · Not installed");
});

// --- the cluster page -------------------------------------------------------

const NONE: UpgradeVerdict = { kind: "none", reason: "not under test" };

function overview(instance: Instance, releases: ReleaseListing | undefined, upgrade: UpgradeVerdict = NONE): string {
  const parts = localOverviewScreen({
    instance,
    bar: localOverviewBar({ instance, connection: "connected", upgrade }),
    runs: [],
    nowMs: 0,
    upgrade,
    releases,
    detailsOpen: false,
    home: "/home/me",
  });
  return parts.head + parts.body + parts.actions;
}

test("the page says Up to date only when that is known", () => {
  assert.match(overview(healthy("v0.19.0"), KNOWN), /<dt>Update<\/dt><dd>Up to date<\/dd>/);
  // Not fetched is not "up to date", and it is not a placeholder row either.
  const unfetched = overview(healthy("v0.18.0"), listing([], "network unreachable"));
  assert.doesNotMatch(unfetched, /Up to date|not fetched|<dt>Update<\/dt>/);
  assert.doesNotMatch(overview(healthy("v0.18.0"), undefined), /Up to date|not fetched/);
});

test("the version is the head's meta, said once", () => {
  const html = overview(healthy("v0.18.0"), KNOWN);
  assert.match(html, /mq-head-meta">v0\.18\.0</);
  assert.equal(html.split("v0.18.0").length - 1, 1, "the version was said more than once");
});

test("a newer release that needs manual steps is a fact with the guide beside it, never a button that refuses", () => {
  const refused: UpgradeVerdict = {
    kind: "refused",
    target: { instanceName: "local", from: "v0.17.1", to: "v0.19.0", flow: "upgradeToTag" },
    label: "Update to v0.19.0",
    message: "v0.19.0 needs manual upgrade steps.",
    barriers: [],
    docHref: "docs/public/operate/upgrade-barriers.md",
  };
  const html = overview(healthy("v0.17.1"), KNOWN, refused);
  assert.match(html, /v0\.19\.0 needs manual upgrade steps\./);
  assert.match(html, /data-act="openGuide" data-value="docs\/public\/operate\/upgrade-barriers\.md"/);
  assert.doesNotMatch(html, /data-act="update"/);
  assert.doesNotMatch(html, /retag/);
});

test("an absent cluster shows no version, no update and no placeholder facts", () => {
  const html = overview({ name: "local", kind: "local", presence: "absent", connected: false, registered: false }, KNOWN);
  assert.doesNotMatch(html, /not recorded|not fetched|unknown|<dt>/);
  assert.match(html, /data-act="install"/);
});

// --- the version picker -----------------------------------------------------

const CHOOSE = {
  instance: healthy("v0.18.0"),
  choice: "",
  typed: "",
  typedError: "",
  target: "",
  plan: [],
  summary: "",
  sameVersion: false,
} as const;

function picker(over: Partial<Parameters<typeof chooseVersionScreen>[0]> = {}): string {
  const parts = chooseVersionScreen({ ...CHOOSE, listing: KNOWN, ...over });
  // The kit escapes an apostrophe; the assertions read the words.
  return (parts.head + parts.body + parts.actions).replace(/&#39;/g, "'");
}

test("the picker marks the newest as Latest and this cluster's as Current, without selecting either", () => {
  const html = picker();
  assert.match(html, /<option value="v0\.19\.0">v0\.19\.0 · Latest<\/option>/);
  assert.match(html, /<option value="v0\.18\.0">v0\.18\.0 · Current<\/option>/);
  // The prompt option is the selected one: a version the page chose silently
  // is not a version the operator can be held to.
  assert.match(html, /<option value="" selected>Choose a version<\/option>/);
  assert.equal(html.match(/<option [^>]*selected/g)?.length, 1, "exactly one option is selected");
});

test("before a version is chosen there is no act to take, and the bar says why", () => {
  const html = picker();
  assert.doesNotMatch(html, /data-act="beginChange"/);
  assert.match(html, /Choose a version/);
  assert.doesNotMatch(html, /disabled/);
});

test("an operator's choice is selected, and the act names it", () => {
  const html = picker({ choice: "v0.19.0", target: "v0.19.0" });
  assert.match(html, /<option value="v0\.19\.0" selected>/);
  assert.match(html, /data-act="beginChange" data-value="v0\.19\.0"[^>]*>Change to v0\.19\.0</);
});

test("Other... asks for a version, and a mistyped one is caught under the field", () => {
  const html = picker({ choice: OTHER_VERSION, typed: "0.20", typedError: "Use a version like v0.24.0." });
  assert.match(html, /data-field="typed"/);
  assert.match(html, /Use a version like v0\.24\.0\./);
  assert.doesNotMatch(html, /data-act="beginChange"/);
});

test("when the list cannot be loaded, typing is the control and the page says why in one line", () => {
  const html = picker({ listing: listing([], "fatal: could not read from remote repository") });
  assert.doesNotMatch(html, /<select/);
  assert.match(html, /data-field="typed"/);
  assert.match(html, /Couldn't load the list of versions\./);
  assert.doesNotMatch(html, /fatal:/, "raw git output reached the page");
});

test("while the list loads, the picker is the shape of a field", () => {
  const html = picker({ listing: undefined });
  assert.match(html, /mq-skeleton/);
  // The words are for screen readers only.
  assert.doesNotMatch(html.replace(/<span class="mq-sr">[^<]*<\/span>/g, ""), /Loading/);
});

test("the crossing off your own build is said when a version is chosen, and only then", () => {
  const checkout: Instance = { ...healthy("v0.18.0"), imageSource: "checkout" };
  assert.match(picker({ instance: checkout, choice: "v0.19.0", target: "v0.19.0" }), /Your own build is replaced with released v0\.19\.0 images\./);
  assert.doesNotMatch(picker({ instance: checkout }), /Your own build/);
  // A released cluster crosses nothing and is told nothing.
  assert.doesNotMatch(picker({ choice: "v0.19.0", target: "v0.19.0" }), /Your own build/);
});

test("the same version again says what it does, in one line", () => {
  const html = picker({ choice: "v0.18.0", target: "v0.18.0", sameVersion: true });
  assert.match(html, /Already on v0\.18\.0\./);
  assert.match(html, /the same as Repair/);
  assert.doesNotMatch(html, /overlay/);
});
