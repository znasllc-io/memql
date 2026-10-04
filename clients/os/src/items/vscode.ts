// File content opens in the configured VS Code host. Browser is the default;
// installed VS Code and Cursor use their extension URI handlers.

import { editorArtifactURL } from "./editorPreference";

export const VSCODE_HANDOFF_TIMEOUT_MS = 2500;

export function artifactHandoffUrl(clusterDomain: string, artifactId: string, name?: string): string {
  return editorArtifactURL(clusterDomain, artifactId, name);
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
  /** Opens the editor URL (injectable so tests never navigate). */
  navigate: (url: string) => void;
  /** Reserve a new tab within a user gesture before an asynchronous metadata read. */
  reserve?: () => { navigate: (url: string) => void; close: () => void };
  /** setTimeout-compatible scheduler (injectable time). */
  schedule: (fn: () => void, ms: number) => () => void;
}

export const browserHandoffPorts: HandoffPorts = {
  reserve: () => {
    const tab = window.open("about:blank", "_blank");
    if (!tab) throw new Error("Allow this app to open a new tab, then open the file again.");
    tab.opener = null;
    const referrer = tab.document.createElement("meta");
    referrer.name = "referrer"; referrer.content = "no-referrer";
    tab.document.head.append(referrer);
    return { navigate: url => tab.location.replace(url), close: () => tab.close() };
  },
  navigate: (url) => {
    if (url.startsWith("https://")) {
      window.open(url, "_blank", "noopener,noreferrer");
    } else {
      window.location.href = url;
    }
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
  name?: string,
): () => void {
  const url = artifactHandoffUrl(clusterDomain, artifactId, name);
  ports.navigate(url);
  if (url.startsWith("https://")) return () => {};
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
  if (url.startsWith("https://")) return () => {};
  return ports.schedule(() => {
    if (!document.hidden) onNoAnswer();
  }, VSCODE_HANDOFF_TIMEOUT_MS);
}

export const VSCODE_NO_ANSWER_MESSAGE =
  "The editor did not answer. Check that it is installed with MemQL and MemQL Productivity Tools, or choose the browser in Files settings.";
