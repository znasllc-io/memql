// What an operator has to run on the machine they are adding.
//
// ===========================================================================
// THE ONE-LINER IS THE RUNBOOK'S, NOT A NEW SHAPE
// ===========================================================================
// docs/public/operate/workers-runbook.md section 2 is the documented install,
// and it takes three inputs: the installer script, `--token mql_wkr_...`, and
// `--cluster <url>`. This module composes exactly that command with the token
// this page just minted, so an operator copies one line instead of reading a
// runbook and substituting two values by hand -- which is where the mistakes
// are (a token pasted with a trailing newline, a cluster URL with no scheme).
// "One line" is literal since memql#4873: one PHYSICAL line, and the runbook
// prints the same single line (memql#4874, the same epic) -- if the shape
// changes on either side, it changes on both.
//
// THE INSTALLER SHIPS FROM memql-cockpit, not from this repo. The worker is a
// run mode of the `memql` command that repo builds, and scripts/install/ here
// carries the cluster bring-up installers instead. The page says so under the
// command rather than implying this repo builds it.
//
// (The engine also builds a `bin/memql`, but that one ships only inside
// container images and runs in pods -- the two never share a PATH. See
// memql/CLAUDE.md.)

export type InstallPlatform = "mac" | "linux";

export const INSTALL_PLATFORMS: readonly InstallPlatform[] = ["mac", "linux"];

export const INSTALL_PLATFORM_LABEL: Record<InstallPlatform, string> = {
  mac: "macOS",
  linux: "Linux",
};

// The placeholder rendered when this deployment publishes no domain. Written
// as something that is obviously NOT a URL so a copied command fails loudly at
// the shell rather than dialling a host nobody meant.

// THE SCRIPT COMES FROM THE memql-cockpit REPO, pinned to main: the cluster
// serves no installer (the old /admin/workers/install path never had a
// server on the engine), and the installer is versioned with the worker it
// installs. The composed command still carries this cluster's URL + the
// minted token as arguments.
export const CLUSTER_URL_PLACEHOLDER = "<your cluster URL>";

// workerClusterUrl derives the value a worker dials from the cluster's own
// domain.
//
// `api.<domain>`, because every front-door role host is a SINGLE LABEL under
// the domain (memql#3767) and `api` is the one carrying the gRPC edge. The
// scheme is stated, and that is load-bearing rather than tidy:
// sdk/go/worker.ParseClusterURL reads a scheme as authoritative and treats a
// bare `host:port` as PLAINTEXT whatever the port, so a value written without
// one tells the worker to dial a TLS port in the clear (memql#3437).
//
// Returns "" when the domain is unknown; the caller renders the placeholder
// rather than composing half a URL.
export function workerClusterUrl(domain: string): string {
  const trimmed = domain.trim().replace(/^https?:\/\//, "").replace(/\/+$/, "");
  if (trimmed === "") return "";
  return `https://api.${trimmed}`;
}

export interface InstallCommandInput {
  platform: InstallPlatform;
  clusterUrl: string;
  token: string;
  // Adds --computeruse, which installs the build that can drive the mouse and
  // keyboard. Off by default: the headless build is the one most work needs,
  // and the computer-use build asks for Accessibility and Screen Recording on
  // first run.
  computerUse: boolean;
}

// installCommand composes the runbook's one-liner.
//
// ===========================================================================
// ONE PHYSICAL LINE, BECAUSE PASTE HANDLING SPLITS ANYTHING ELSE
// ===========================================================================
// The first shape here was multi-line with trailing backslashes, defended as
// more readable than a 200-character line and no easier to paste. Field
// evidence reversed that: terminal paste handling (bracketed paste,
// continuation-prompt mangling) split the copied block, so `bash -s --` ran
// with NO arguments and `--token mql_wkr_...` executed as its own shell
// command -- observed live as `--token ...: command not found`, with the
// worker token landing in shell history either way. A flag line running as a
// command is strictly worse than a long line: the install half-runs, the
// failure names nothing an operator can act on, and the credential leaks.
//
// So: no newline and no backslash anywhere in the output, whatever the
// inputs. Readability in the page is the rendering surface's job (the
// AddMachine <pre> scrolls horizontally); correctness under paste is this
// module's, and only a single physical line survives every terminal.
export function installCommand(input: InstallCommandInput): string {
  const cluster = input.clusterUrl === "" ? CLUSTER_URL_PLACEHOLDER : input.clusterUrl;
  const script = `https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-${input.platform}.sh`;
  const computeruse = input.computerUse ? " --computeruse" : "";
  return `curl -fsSL ${script} | bash -s -- --token ${input.token} --cluster ${cluster}${computeruse}`;
}
