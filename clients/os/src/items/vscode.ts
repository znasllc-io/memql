// Opening a file hands off to VS Code (spec D3) -- there is no in-OS editor
// by design. The URL shape is the portal's existing handoff (memql#4251)
// with kind=artifact; the extension side lands in its own epic, so the
// fallback message is the permanent UX for "nothing answered".

export const VSCODE_HANDOFF_TIMEOUT_MS = 2500;

export function artifactHandoffUrl(clusterDomain: string, artifactId: string): string {
  return (
    "vscode://znasllc.memql/open?v=1" +
    `&cluster=${encodeURIComponent(clusterDomain)}` +
    "&kind=artifact" +
    `&id=${encodeURIComponent(artifactId)}`
  );
}

/**
 * The handoff for a CONCEPT (epic memql#5009) -- the reverse direction of
 * the extension's own "open this concept's rows in the console" link.
 *
 * `kind=concept` with `name=`, rather than `kind=artifact` with `id=`,
 * because that is the shape the extension already answers: a construct is
 * addressed by its NAME and a row by its id. The two are different
 * questions and the parameter names say which is being asked.
 */
export function conceptHandoffUrl(clusterDomain: string, conceptId: string): string {
  return (
    "vscode://znasllc.memql/open?v=1" +
    `&cluster=${encodeURIComponent(clusterDomain)}` +
    "&kind=concept" +
    `&name=${encodeURIComponent(conceptId)}`
  );
}

export interface HandoffPorts {
  /** Fires the vscode: URL (injectable so tests never navigate). */
  navigate: (url: string) => void;
  /** setTimeout-compatible scheduler (injectable time). */
  schedule: (fn: () => void, ms: number) => () => void;
}

export const browserHandoffPorts: HandoffPorts = {
  navigate: (url) => {
    window.location.href = url;
  },
  schedule: (fn, ms) => {
    const t = setTimeout(fn, ms);
    return () => clearTimeout(t);
  },
};

/**
 * Fire the handoff; call `onNoAnswer` when the page is still visible after
 * the timeout (VS Code answering blurs/hides the page). Returns a cancel.
 */
export function openInVsCode(
  clusterDomain: string,
  artifactId: string,
  onNoAnswer: () => void,
  ports: HandoffPorts = browserHandoffPorts,
): () => void {
  ports.navigate(artifactHandoffUrl(clusterDomain, artifactId));
  return ports.schedule(() => {
    if (!document.hidden) onNoAnswer();
  }, VSCODE_HANDOFF_TIMEOUT_MS);
}

/**
 * Fire a handoff at an already-composed URL.
 *
 * The generalisation of `openInVsCode`, which stays as the artifact-shaped
 * call every Files surface already makes. Same contract: returns a cancel,
 * and calls `onNoAnswer` when the page is still visible after the timeout,
 * because VS Code answering blurs or hides it.
 */
export function openHandoff(
  url: string,
  onNoAnswer: () => void,
  ports: HandoffPorts = browserHandoffPorts,
): () => void {
  ports.navigate(url);
  return ports.schedule(() => {
    if (!document.hidden) onNoAnswer();
  }, VSCODE_HANDOFF_TIMEOUT_MS);
}

export const VSCODE_NO_ANSWER_MESSAGE =
  "VS Code did not answer. Is it installed, with the MemQL extension signed in to this cluster?";
