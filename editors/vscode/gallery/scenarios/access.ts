// The ACCESS screens: the cluster page in every state, and the two pages the
// browser lands on after a sign-in.
//
// Built exactly as the panel builds them -- clusterPage() into pageDocument()
// -- so a capture is the page a person sees, at real size, in both themes.

import { clusterPage, type ClusterPageInput } from "../../src/clusters/connectionView.js";
import type { ClusterFacts } from "../../src/clusters/facts.js";
import type { ClusterConfig } from "../../src/clusters/model.js";
import type { ConnectionState } from "../../src/connection/manager.js";
import { loopbackPage } from "../../src/auth/loopbackPage.js";
import { bodyThemeAttr } from "../../src/webview/appearance.js";
import { pageDocument } from "../../src/webview/ui/document.js";
import { GALLERY_NONCE, type GalleryTheme, type Scenario } from "../harness.js";

const GROUP = "Access";

const LOCAL: ClusterConfig = {
  name: "local",
  displayName: "memql.localhost",
  endpoint: "api.memql.localhost:443",
  domain: "memql.localhost",
  local: true,
  version: "main",
};

const REMOTE: ClusterConfig = {
  name: "staging",
  displayName: "staging.memql.io",
  endpoint: "api.staging.memql.io:443",
  domain: "staging.memql.io",
  version: "v0.19.1",
};

const SESSION: ClusterFacts = { session: true, signedIn: true, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const NOTHING: ClusterFacts = { session: false, signedIn: false, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const LISTING = { tags: ["v0.21.0", "v0.19.1"], fetchedAt: 1 };

function error(cluster: ClusterConfig, reason: "missingCredential" | "reauthenticationRequired" | "unreachable" | "lost", message = "x"): ConnectionState {
  return { status: "error", clusterName: cluster.name, reason, message };
}

function page(id: string, title: string, over: Partial<ClusterPageInput> & { cluster?: ClusterConfig }): Scenario {
  const cluster = over.cluster ?? LOCAL;
  const input: ClusterPageInput = {
    clusterName: cluster.name,
    cluster,
    facts: SESSION,
    connection: { status: "disconnected" },
    identity: "loading",
    consoleUrl: cluster === REMOTE ? "https://os.staging.memql.io/" : "https://os.memql.localhost/",
    listing: LISTING,
    ...over,
  };
  return {
    id,
    group: GROUP,
    title,
    render: (theme: GalleryTheme) => {
      const view = clusterPage(input);
      return pageDocument({
        nonce: GALLERY_NONCE,
        title: view.title,
        themeAttr: bodyThemeAttr(theme),
        screen: view.screen,
        head: view.head,
        body: view.body,
        actions: view.actions,
      });
    },
  };
}

function landing(id: string, title: string, outcome: "success" | "failure"): Scenario {
  return {
    id,
    group: GROUP,
    title,
    render: (theme: GalleryTheme) => loopbackPage(outcome, { scheme: theme, nonce: GALLERY_NONCE }),
  };
}

export const scenarios: readonly Scenario[] = [
  page("access-cluster-connected", "Cluster page: connected", {
    connection: { status: "connected", clusterName: "local", nodeId: "bff-0" },
    identity: { email: "ada@example.com", role: "owner" },
  }),
  page("access-cluster-connected-reading", "Cluster page: connected, account loading", {
    connection: { status: "connected", clusterName: "local", nodeId: "bff-0" },
    identity: "loading",
  }),
  page("access-cluster-connecting", "Cluster page: connecting", {
    connection: { status: "connecting", clusterName: "local" },
  }),
  page("access-cluster-signin", "Cluster page: sign in needed", {
    cluster: REMOTE,
    facts: NOTHING,
  }),
  page("access-cluster-signin-first-run", "Cluster page: first run, owner passkey offered", {
    facts: { ...NOTHING, ownerSetup: true },
  }),
  page("access-cluster-expired", "Cluster page: session ended", {
    cluster: REMOTE,
    connection: error(REMOTE, "reauthenticationRequired"),
  }),
  page("access-cluster-signing-in", "Cluster page: signing in, code offered", {
    facts: NOTHING,
    signingIn: { phase: "waiting", codeOffered: true },
  }),
  page("access-cluster-unreachable-local", "Cluster page: local cluster not running", {
    connection: error(LOCAL, "unreachable", "connect ECONNREFUSED 127.0.0.1:443"),
  }),
  page("access-cluster-unreachable-remote", "Cluster page: remote cluster can't be reached", {
    cluster: REMOTE,
    connection: error(REMOTE, "unreachable", "getaddrinfo ENOTFOUND api.staging.memql.io"),
  }),
  page("access-cluster-untrusted", "Cluster page: certificate not trusted", {
    connection: error(LOCAL, "unreachable", "unable to verify the first certificate"),
  }),
  page("access-cluster-lost", "Cluster page: connection lost", {
    cluster: REMOTE,
    connection: error(REMOTE, "lost"),
  }),
  page("access-cluster-notconfigured", "Cluster page: no address", {
    cluster: { name: "draft", displayName: "draft", endpoint: "" },
    facts: { ...NOTHING, consoleUrl: "" },
    consoleUrl: "",
  }),
  page("access-cluster-idle", "Cluster page: signed in, not connected", {
    cluster: REMOTE,
  }),
  page("access-cluster-loading", "Cluster page: loading", { facts: undefined }),
  page("access-cluster-removed", "Cluster page: removed from the list", { cluster: undefined, clusterName: "local" }),
  page("access-cluster-registry-error", "Cluster page: cluster list unreadable", {
    registryError: "clusters.yaml is malformed: line 3",
  }),
  landing("access-loopback-signed-in", "Browser page: signed in", "success"),
  landing("access-loopback-failed", "Browser page: sign-in didn't finish", "failure"),
];
