// A local install cannot work from a remote extension host, and saying so is
// the whole fix (memql#4623).
//
// -----------------------------------------------------------------------------
// WHY IT CANNOT WORK, AND WHY IT LOOKED LIKE IT DID
// -----------------------------------------------------------------------------
//
// The install wizard creates a k3d cluster on the machine the EXTENSION HOST
// runs on and hands the operator credential links at
// `https://identity.<domain>/...`, defaulting to `identity.memql.localhost`.
// Under Remote-SSH, Codespaces or a dev container that machine is the server,
// and three separate things then break:
//
//   1. `asExternalUri` tunnels LOOPBACK authorities. `identity.memql.localhost`
//      is not one, so the URL comes back unchanged and no tunnel exists.
//   2. RFC 6761 reserves the whole `.localhost` subtree to loopback, so the
//      USER's browser resolves it to THEIR 127.0.0.1 -- where the cluster is
//      not -- rather than following anything.
//   3. Even forwarded, the mkcert CA went into the REMOTE's trust store
//      (scripts/install/mkcert-setup.sh), not the browser's.
//
// The install SUCCEEDED. Every credential button then opened a tab that could
// not connect, and the README and two source comments said the flow "falls back
// automatically" -- so the operator's reasonable conclusion was that MemQL was
// broken rather than that this combination is not supported.
//
// -----------------------------------------------------------------------------
// WHY A REFUSAL RATHER THAN A WORKAROUND
// -----------------------------------------------------------------------------
//
// There is no client-side fix. Port forwarding does not reach a `.localhost`
// name, and trusting the CA would mean writing into the user's machine from the
// server. A remote k3d install is a real thing an operator can do -- from a
// terminal ON that host, with `make up` -- and pointing at that is an answer.
// Pretending it works here is not.
//
// CONNECTING to an existing cluster is untouched: that is a reachable https
// endpoint, and sign-in works from anywhere since the vscode:// callback
// (auth/uriCallback.ts).
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// `remoteName` is read at the call site and passed in.

/** What the editor reports about where this extension host runs. */
export interface RemoteHostContext {
  /** `vscode.env.remoteName` -- undefined on a local editor. */
  remoteName: string | undefined;
}

/**
 * localInstallRefusal returns the sentence to show, or undefined when a local
 * install can proceed.
 *
 * A VALUE rather than a thrown error, so the caller decides whether it is a
 * modal, a toast or a disabled button -- and so the reason is testable without
 * a webview.
 */
export function localInstallRefusal(ctx: RemoteHostContext): string | undefined {
  const remote = (ctx.remoteName ?? "").trim();
  if (remote === "") return undefined;

  return (
    `A local install cannot be driven from a ${describeRemote(remote)} window. ` +
    `The cluster would be created on the remote machine, but its credential links ` +
    `use .localhost names that your browser resolves to your OWN machine, and its ` +
    `certificate authority is trusted only on the remote. The install would report ` +
    `success and every sign-in link would fail to connect.\n\n` +
    `Install from a terminal on that machine with "make up", then use ` +
    `"MemQL: Add Cluster" here to connect to it -- signing in to a reachable ` +
    `cluster works from a remote window.`
  );
}

/** A human name for the remote kind, from VS Code's own authority strings. */
function describeRemote(remoteName: string): string {
  const kind = remoteName.split("+")[0]?.toLowerCase() ?? "";
  switch (kind) {
    case "ssh-remote":
      return "Remote-SSH";
    case "dev-container":
    case "attached-container":
      return "dev container";
    case "codespaces":
      return "Codespaces";
    case "wsl":
      return "WSL";
    default:
      return `remote (${remoteName})`;
  }
}
