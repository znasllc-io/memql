// What an operator has to run on the machine they are adding.
//
// ===========================================================================
// THE ONE-LINER IS THE RUNBOOK'S, NOT A NEW SHAPE
// ===========================================================================
// docs/public/operate/workers-runbook.md section 2 is the documented install,
// and it takes three inputs: the installer script, `--token mql_wkr_...`, and
// `--cluster <url>`. This composes exactly that command with the token just
// minted, so an operator copies one line instead of reading a runbook and
// substituting two values by hand -- which is where the mistakes are (a token
// pasted with a trailing newline, a cluster URL with no scheme).
// "One line" is literal since memql#4875: one PHYSICAL line, and the runbook
// (memql#4874) and the portal composer (memql#4873, the same epic) print the
// same single line -- if the shape changes on any of the three, it changes on
// all of them.
//
// The cockpit installer (memql-cockpit multi-home fleet) upserts one home into
// ~/.memql/workers.yaml; it does not overwrite sibling homes. This emitter
// does not pass --force or --home-id: a fresh Fleet mint is always a new or
// same-URL token refresh, and install scripts derive home id from the URL host.
//
// THE INSTALLER SHIPS FROM memql-cockpit, not from this repo. The worker is a
// run mode of the `memql` command that repo builds; scripts/install/ here
// carries the cluster bring-up installers instead. The panel says so under
// the command rather than implying this repo builds it.

import { localInstallerEnvironment, type LocalCockpitInstall } from "./localInstall";
export type InstallPlatform = "mac" | "linux";

export const INSTALL_PLATFORMS: readonly InstallPlatform[] = ["mac", "linux"];

export const INSTALL_PLATFORM_LABEL: Record<InstallPlatform, string> = {
  mac: "macOS",
  linux: "Linux",
};

/** Rendered when this deployment publishes no domain. Written as something
 *  that is obviously NOT a URL, so a copied command fails loudly at the shell
 *  rather than dialling a host nobody meant. */
export const CLUSTER_URL_PLACEHOLDER = "<your cluster URL>";

/**
 * The value a worker dials, derived from the cluster's own domain.
 *
 * `api.<domain>`, because every front-door role host is a SINGLE LABEL under
 * the domain (memql#3767) and `api` is the one carrying the gRPC edge. The
 * scheme is stated, and that is load-bearing rather than tidy:
 * sdk/go/worker.ParseClusterURL reads a scheme as authoritative and treats a
 * bare `host:port` as PLAINTEXT whatever the port, so a value written without
 * one tells the worker to dial a TLS port in the clear (memql#3437).
 *
 * Returns "" when the domain is unknown; the caller renders the placeholder
 * rather than composing half a URL.
 */
export function workerClusterUrl(domain: string): string {
  const trimmed = domain.trim().replace(/^https?:\/\//, "").replace(/\/+$/, "");
  if (trimmed === "") return "";
  return `https://api.${trimmed}`;
}

export interface InstallCommandInput {
  platform: InstallPlatform;
  clusterUrl: string;
  token: string;
  /**
   * Adds --computeruse, which installs the build that can drive the mouse and
   * keyboard. Off by default: the headless build is the one most work needs,
   * and the computer-use build asks for Accessibility and Screen Recording on
   * first run.
   */
  computerUse: boolean;
  /**
   * Adds --inference, which carries on into `memql worker setup --inference`
   * in the same terminal once the token is written: the installer checks the
   * hardware floor, installs or finds a runtime, pulls a starting model and
   * writes it into the machine's own `models.allow` (epic memql#5103, D5).
   *
   * Off by default, and that is a judgement rather than a convention. It
   * downloads several gigabytes and may install software, which is not
   * something to do to somebody's machine because a checkbox was pre-ticked;
   * and a machine that only ever runs tools needs none of it.
   *
   * INDEPENDENT OF computerUse. The two flags choose different things -- one
   * a BUILD, the other a SETUP STEP -- and both, either or neither is a
   * legitimate machine.
   */
  inference: boolean;
  /** Install into this account's ~/.memql/bin instead of /usr/local/bin. */
  userLocal?: boolean;
  localTest?: LocalCockpitInstall | null;
}

// installCommand composes the runbook's one-liner.
//
// ===========================================================================
// ONE PHYSICAL LINE, BECAUSE PASTE HANDLING SPLITS ANYTHING ELSE
// ===========================================================================
// The first shape here was multi-line with trailing backslashes, defended as
// more readable than a 200-character line and no easier to paste. Field
// evidence reversed that, on this very panel: terminal paste handling
// (bracketed paste, continuation-prompt mangling) split the copied block, so
// `bash -s --` ran with NO arguments and `--token mql_wkr_...` executed as
// its own shell command -- observed live as `--token ...: command not found`,
// with the worker token landing in shell history either way. A flag line
// running as a command is strictly worse than a long line: the install
// half-runs, the failure names nothing an operator can act on, and the
// credential leaks.
//
// So: no newline and no backslash anywhere in the output, whatever the
// inputs. Readability in the panel is the rendering surface's job (the
// AddMachine <pre> scrolls horizontally); correctness under paste is this
// module's, and only a single physical line survives every terminal.
export function installCommand(input: InstallCommandInput): string {
  if (input.localTest && input.platform === "mac") {
    const source = input.localTest;
    const inference = input.inference ? " --inference" : "";
    return `curl -fsSL ${source.base}/scripts/install/install-mac.sh | ${localInstallerEnvironment(source)}bash -s -- --token ${input.token} --cluster ${input.clusterUrl || CLUSTER_URL_PLACEHOLDER} --computeruse${inference} --user-local --download-base=${source.base}/releases/download/v${source.version}`;
  }
  const cluster = input.clusterUrl === "" ? CLUSTER_URL_PLACEHOLDER : input.clusterUrl;
  const script = `https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-${input.platform}.sh`;
  const computeruse = input.computerUse ? " --computeruse" : "";
  // ORDER IS FIXED and asserted word for word by the tests: --computeruse
  // then --inference. The installers parse both, so the order is cosmetic to
  // them and load-bearing to a person comparing what the OS printed with what
  // the runbook prints.
  const inference = input.inference ? " --inference" : "";
  const location = input.userLocal ? " --user-local" : "";
  return `curl -fsSL ${script} | bash -s -- --token ${input.token} --cluster ${cluster}${computeruse}${inference}${location}`;
}

// uninstallCommand composes the uninstaller's one-liner (design record
// 2026-09-08-cockpit-install-wizard, D12).
//
// The same shape as the install line, for the same reasons: one physical line
// with no newline and no backslash, the script fetched from the cockpit
// repository's main branch. It takes no token and no cluster -- removing a
// worker is a fact about the machine, not about the cluster it served. The
// registration on the cluster is revoked from Fleet, which is where this line
// is shown.
//
// WITHOUT --purge the state directory, policy.yaml and the logs stay, so a
// person can read what the worker was doing before it went; the caption beside
// the line says so and names the flag. `userLocal` mirrors the install's own
// --user-local: a worker installed under ~/.memql/bin is removed from there.
export function uninstallCommand(
  platform: InstallPlatform,
  opts: { purge?: boolean; userLocal?: boolean; clusterUrl?: string; localTest?: LocalCockpitInstall | null } = {},
): string {
  if (opts.localTest && platform === "mac") return `curl -fsSL ${opts.localTest.base}/scripts/install/uninstall-mac.sh | ${localInstallerEnvironment(opts.localTest)}bash -s -- --user-local --cluster=${opts.clusterUrl || CLUSTER_URL_PLACEHOLDER}${opts.purge ? " --purge" : ""}`;
  const script = `https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/uninstall-${platform}.sh`;
  const flags = [opts.purge ? " --purge" : "", opts.userLocal ? " --user-local" : ""].join("");
  // `bash -s --` even with no flags, so a person appending one edits the same
  // line the install had rather than learning a second shape.
  return `curl -fsSL ${script} | bash -s --${flags}`;
}

/** The second command a local-models machine needs on a fresh install: the
 *  one-liner runs without a terminal to ask on, so it cannot approve a runtime
 *  install, and prints this for the person to run next (D13). */
export function setupCommand(userLocal = false, inference = false): string {
  // Do not rely on PATH: it may still resolve an older system installation,
  // and a fresh account does not have ~/.memql/bin on its PATH at all.
  const binary = userLocal ? '"$HOME/.memql/bin/memql"' : "/usr/local/bin/memql";
  return `${binary} worker setup${inference ? " --inference" : ""}`;
}
