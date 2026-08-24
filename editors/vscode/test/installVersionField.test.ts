import assert from "node:assert/strict";
import { test } from "node:test";

import { AddClusterState, DEFAULT_INPUTS, requiredFields } from "../src/state/addCluster.js";
import { DEFAULT_STACK_REPO, DEFAULT_STACK_TAG } from "../src/install/stackPin.js";
import { listReleaseTags } from "../src/install/tags.js";
import { refusedPlatformGuidance } from "../src/state/installProgress.js";
import {
  latestLabel,
  renderCollectScreen,
  versionChoiceList,
  withCurrentInSortedPosition,
} from "../src/webview/installScreens.js";

// installVersionField.test.ts -- znasllc-io/memql#3882.
//
// An operator uninstalled a local cluster and reinstalled it cleanly. The
// install was correct in every respect and still produced a cluster that could
// not be used: the extension got 404 on api.memql.localhost, sign-in returned
// `500 could not persist client`, and the database had zero tables. It had
// faithfully deployed v0.17.1, and all three fixes were on main.
//
// The operator had no way to say "install the version with the fixes". After an
// uninstall the receipt is gone, so the panel sent an empty tag and `installPlan`
// fell back to the compiled-in pin. The wizard had no tag field -- while `--tag`
// existed on the CLI and `--image-tag` on the capability script. Every layer
// below the wizard already supported this; only the front end could not say it.

// ---------------------------------------------------------------------------
// Latest is labelled, and Latest is what an untouched field ends up on
// (memql#4429)
// ---------------------------------------------------------------------------

function collectForm(): AddClusterState {
  const s = new AddClusterState();
  s.chooseAction("install");
  return s;
}

test("the newest listed release is labelled Latest, and its VALUE is that real tag", () => {
  // NO SENTINEL. What gets submitted is an ordinary version string, so
  // installPlan, imageTagFor and the receipt all see what they would have seen
  // had the operator picked the tag by name -- and the receipt NAMES the release
  // rather than the word "latest", which is what makes the installed version
  // readable back a month later.
  const list = versionChoiceList(["v0.20.3", "v0.19.1"], "v0.19.1");
  assert.equal(list[0]!.value, "v0.20.3");
  assert.equal(list[0]!.label, latestLabel("v0.20.3"));
  assert.match(list[0]!.label, /recommended/);

  // ...and it appears ONCE: the labelled entry replaces the bare row.
  assert.equal(list.filter((c) => c.value === "v0.20.3").length, 1);
});

test("the Latest label follows the LISTING's newest, not whatever sorts first", () => {
  // These differ in exactly the case the sorted insert exists for: a current
  // value the listing does not carry can sort above everything listed, and
  // calling that "recommended" would be a recommendation the listing does not
  // support.
  const list = versionChoiceList(["v0.19.0", "v0.18.0"], "v0.99.0");
  assert.equal(list[0]!.value, "v0.99.0");
  assert.equal(list[0]!.label, "v0.99.0", "an unlisted value is present and in order, never endorsed");
  assert.equal(list[1]!.label, latestLabel("v0.19.0"));
});

test("the listing seeds the field, so the recommendation IS the selection", () => {
  const s = collectForm();
  assert.equal(s.inputs.version, DEFAULT_STACK_TAG, "the offline fallback is where it starts");
  s.seedVersionFromListing("v0.20.3");
  assert.equal(s.inputs.version, "v0.20.3");
});

test("an operator's own answer is never overwritten by a listing landing late", () => {
  // The listing is async. Somebody who has already chosen must not have that
  // choice replaced by a network call arriving a second later.
  const s = collectForm();
  s.setInput("version", "v0.18.0");
  s.seedVersionFromListing("v0.20.3");
  assert.equal(s.inputs.version, "v0.18.0");
  assert.equal(s.versionWasChosen, true);
});

test("choosing main is a choice like any other, and survives the listing", () => {
  const s = collectForm();
  s.setInput("version", "main");
  s.seedVersionFromListing("v0.20.3");
  assert.equal(s.inputs.version, "main");
});

test("an empty listing leaves the offline fallback in place rather than blanking the field", () => {
  // Blanking would turn a failed network call into "A version is required."
  const s = collectForm();
  s.seedVersionFromListing("");
  assert.equal(s.inputs.version, DEFAULT_STACK_TAG);
  assert.equal(s.validate().some((e) => e.field === "version"), false);
});

test("the offline fallback is a real published release, so it can actually install", () => {
  assert.match(DEFAULT_STACK_TAG, /^v\d+\.\d+\.\d+$/);
});

// ---------------------------------------------------------------------------
// what the SCREEN actually renders -- the automated half of the manual checklist
// ---------------------------------------------------------------------------

/** The `<option>` rows of the VERSION select only, as value/selected/label. */
function versionOptions(html: string): { value: string; selected: boolean; label: string }[] {
  const open = html.indexOf('<select id="f-version"');
  assert.notEqual(open, -1, "the version field did not render as a picker");
  const block = html.slice(open, html.indexOf("</select>", open));
  return [...block.matchAll(/<option value="([^"]*)"( selected)?>([^<]*)</g)].map((m) => ({
    value: m[1]!,
    selected: m[2] !== undefined,
    label: m[3]!,
  }));
}

function collectHtml(choices: string[]): string {
  return renderCollectScreen({
    action: "install",
    values: collectForm().inputs,
    errors: [],
    versionChoices: choices,
  });
}

test("the rendered picker puts Latest first AND marks it selected", () => {
  // THE TWO HALVES CAN DISAGREE, which is the whole reason this is asserted on
  // the MARKUP rather than on versionChoiceList alone. Labelling an entry
  // "recommended" and selecting a different one is a form that recommends one
  // thing and does another, and every unit test below would still pass.
  const s = collectForm();
  s.seedVersionFromListing("v0.20.3");
  const html = renderCollectScreen({
    action: "install",
    values: s.inputs,
    errors: [],
    versionChoices: ["v0.20.3", "v0.19.1", "v0.18.0"],
  });

  // SCOPED TO THE VERSION SELECT. The collect screen renders the AI-provider
  // choice too, and a page-wide match counts its options as versions.
  const options = versionOptions(html);
  const releases = options.filter((o) => o.value !== "main");

  assert.equal(releases[0]!.value, "v0.20.3", "newest first");
  assert.equal(releases[0]!.label, latestLabel("v0.20.3"));
  assert.equal(releases[0]!.selected, true, "the recommended entry must be the selected one");
  assert.equal(
    options.filter((o) => o.selected).length,
    1,
    "exactly one option may be selected, or the browser picks for us",
  );
  assert.deepEqual(
    releases.map((o) => o.value),
    ["v0.20.3", "v0.19.1", "v0.18.0"],
    "no older tag may sit above a newer one -- this is the mis-sort the epic exists to fix",
  );
});

test("the rendered main entry names no release, and says what it IS", () => {
  const html = collectHtml(["v0.20.3", "v0.19.1"]);
  const main = versionOptions(html).find((o) => o.value === "main")!;
  assert.match(main.label, /build from source/);
  assert.doesNotMatch(main.label, /v0\.\d+\.\d+/, "the skew label named a release; it must not now");
  assert.equal(main.selected, false, "main must never be preselected");
});

test("an empty listing renders a TEXT BOX prefilled with the offline fallback", () => {
  // The degrade-to-typing path: git ls-remote needs a network and a git, and an
  // operator on a plane has neither and still has a cluster to install.
  const html = collectHtml([]);
  assert.doesNotMatch(html, /<select id="f-version"/, "no listing means no picker");
  assert.match(html, new RegExp(`<input id="f-version"[^>]*value="${DEFAULT_STACK_TAG}"`));
  assert.doesNotMatch(
    html,
    /value="main"/,
    "a text box that accepted 'main' would be an unlabelled branch, which clone-stack.sh refuses",
  );
});

test("the version hint states both what Latest is and what main costs", () => {
  const html = collectHtml(["v0.20.3"]);
  assert.match(html, /Latest is preselected/);
  assert.match(html, /BUILDS the node images/);
  assert.match(html, /Docker/);
  assert.match(html, /several minutes/);
});

test("the pin is what the field starts on -- as the OFFLINE FALLBACK", () => {
  // THIS ASSERTION SURVIVED A REVERSED DECISION, SO ITS REASON IS RESTATED
  // (memql#3882 -> memql#4429). It used to record that a reviewed pin beats
  // "newest tag", the pin being the house answer and the field its override.
  // That argument is retired: all four postmortems in stackPin.ts are failures
  // of installing an OLDER release, none of installing something too new, so a
  // fresh install now recommends Latest.
  //
  // The line still holds, for a different reason. The field starts here because
  // the listing is ASYNC and validate() refuses a blank required field -- an
  // operator pressing Start before the listing lands must get a version, not a
  // complaint. And on a machine that cannot list at all, this is the answer.
  assert.equal(DEFAULT_INPUTS.version, DEFAULT_STACK_TAG);
});

test("an install collects the version; a repair does not", () => {
  assert.ok(
    requiredFields("install").includes("version"),
    "the wizard must be able to say which version to install",
  );
  assert.ok(
    !requiredFields("repair").includes("version"),
    "a repair replays the version the receipt recorded (memql#3605). Collecting it here " +
      "would invite an operator to silently upgrade a cluster they meant to repair -- the " +
      "exact defect #3605 fixed, offered back as a control.",
  );
});

test("the version list is asked of the REPOSITORY, not of a checkout", async () => {
  // The wizard needs the list BEFORE there is a checkout -- it is choosing what
  // to clone. Asking a working tree's `origin` is what the deployment page
  // does, and it has a cluster already installed to ask.
  let askedRemote = "";
  await listReleaseTags({
    cwd: "/tmp",
    repo: DEFAULT_STACK_REPO,
    run: async (_cwd, _timeout, remote) => {
      askedRemote = remote;
      return { stdout: "", error: "" };
    },
  });
  assert.equal(askedRemote, DEFAULT_STACK_REPO);
});

test("with no repo it still asks origin, so the deployment page is unchanged", async () => {
  let askedRemote = "";
  await listReleaseTags({
    cwd: "/tmp",
    run: async (_cwd, _timeout, remote) => {
      askedRemote = remote;
      return { stdout: "", error: "" };
    },
  });
  assert.equal(askedRemote, "origin");
});

test("a failed listing is an empty list with a reason, never a rejection", async () => {
  // The caller's alternative to a list is a TEXT BOX, not an error dialog. An
  // operator on a plane has no network and still has a cluster to install.
  const listing = await listReleaseTags({
    cwd: "/tmp",
    repo: DEFAULT_STACK_REPO,
    run: async () => ({ stdout: "", error: "could not run git: ENOENT" }),
  });
  assert.deepEqual(listing.tags, []);
  assert.match(listing.error, /could not run git/);
});

test("the current value is always an option -- IN ITS SORTED POSITION, not first", () => {
  // TWO PROPERTIES ARRIVED IN ONE LINE AND ONLY ONE WAS WANTED (memql#4429).
  //
  // Guarantee-present is real: a <select> silently DROPS a value that is not one
  // of its options, so a current value the listing does not carry -- a tag cut
  // after this extension was built, or a partial listing -- would leave the
  // field showing the newest release while the value still said something else,
  // and the operator would install a version the page never offered them.
  //
  // Queue-jumping was the accident. The old `dedupeKeepingDefault` returned
  // [current, ...rest], which put the pin at the top of a list whose entire
  // meaning is its order. That is the mis-sort the owner reported.
  const got = withCurrentInSortedPosition(["v0.19.0", "v0.17.1"], "v0.18.0");
  assert.deepEqual(got, ["v0.19.0", "v0.18.0", "v0.17.1"]);
});

test("a current value NEWER than anything listed sorts to the top on merit", () => {
  // Same insert, and here it does land first -- because it belongs there, not
  // because it is the current value.
  assert.deepEqual(withCurrentInSortedPosition(["v0.19.0", "v0.17.1"], "v0.20.3"), [
    "v0.20.3",
    "v0.19.0",
    "v0.17.1",
  ]);
});

test("the current value is not duplicated when the listing already has it", () => {
  const got = withCurrentInSortedPosition(["v0.19.0", "v0.18.0", "v0.17.1"], "v0.18.0");
  assert.deepEqual(got, ["v0.19.0", "v0.18.0", "v0.17.1"]);
});

test("an empty current value adds no blank option", () => {
  assert.deepEqual(withCurrentInSortedPosition(["v0.19.0"], ""), ["v0.19.0"]);
});

test("the listing's own order is never re-sorted when nothing is inserted", () => {
  // The listing arrives from compareSemverDesc already. Returning it untouched
  // -- the same array -- is what makes "the picker renders the listing's order"
  // a property of this function rather than a coincidence of re-sorting.
  const listing = ["v0.19.0", "v0.18.0"];
  assert.equal(withCurrentInSortedPosition(listing, "v0.18.0"), listing);
});

test("Create deployment on an unsupported platform refuses instead of listing tags", async () => {
  // Darwin + no wizard cluster: a tag list is a list of versions that cannot
  // run. The listing must not degrade to a text box (memql#4294).
  let listed = false;
  const g = refusedPlatformGuidance();
  const listing = await listReleaseTags({
    cwd: "/tmp",
    repo: DEFAULT_STACK_REPO,
    platformRefuse: async () => `${g.headline} ${g.advice}`,
    run: async () => {
      listed = true;
      return { stdout: "aa\trefs/tags/v0.19.0\n", error: "" };
    },
  });
  assert.equal(listed, false, "git must not be asked when the platform already refused");
  assert.deepEqual(listing.tags, []);
  assert.equal(listing.refusedPlatform, true);
  assert.match(listing.error, /linux\/amd64/);
  assert.match(listing.error, /make up/);
});
