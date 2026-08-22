import assert from "node:assert/strict";
import { test } from "node:test";

import { DEFAULT_INPUTS, requiredFields } from "../src/state/addCluster.js";
import { DEFAULT_STACK_REPO, DEFAULT_STACK_TAG } from "../src/install/stackPin.js";
import { listReleaseTags } from "../src/install/tags.js";
import { refusedPlatformGuidance } from "../src/state/installProgress.js";
import { dedupeKeepingDefault } from "../src/webview/installScreens.js";

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

test("the pinned default is what the field starts on", () => {
  // THE PIN IS NOT THE BUG. stackPin.ts argues that a reviewed pin beats
  // "newest tag" -- a packaged extension carries a staged copy of scripts/ and
  // runs it against whatever the checkout contains, so the pairing should be
  // something somebody chose. The gap is that the pin was the ONLY thing the
  // wizard could express. It stays the default; the field is the override.
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

test("the current value is always an option, and is first", () => {
  // A <select> silently DROPS a value that is not one of its options. A pinned
  // default the remote listing does not carry -- a tag cut after this extension
  // was built, or a partial listing -- would leave the field showing the newest
  // release while the value still said something else, and the operator would
  // install a version the page never offered them.
  const got = dedupeKeepingDefault(["v0.19.0", "v0.17.1"], "v0.18.0");
  assert.deepEqual(got, ["v0.18.0", "v0.19.0", "v0.17.1"]);
});

test("the current value is not duplicated when the listing already has it", () => {
  const got = dedupeKeepingDefault(["v0.19.0", "v0.18.0", "v0.17.1"], "v0.18.0");
  assert.deepEqual(got, ["v0.18.0", "v0.19.0", "v0.17.1"]);
});

test("an empty current value adds no blank option", () => {
  assert.deepEqual(dedupeKeepingDefault(["v0.19.0"], ""), ["v0.19.0"]);
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
