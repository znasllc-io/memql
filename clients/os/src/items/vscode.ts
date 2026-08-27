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

export const VSCODE_NO_ANSWER_MESSAGE =
  "VS Code did not answer. Is it installed, with the MemQL extension signed in to this cluster?";
