// The cluster registry's data model, mirroring the cockpit's
// cli/config.ClusterConfig so the two tools read and write the same file.
//
// Field names here are camelCase; the YAML uses snake_case. file.ts owns the
// mapping. Keeping the boundary in one place means the rest of the extension
// never sees a wire spelling.

export interface ClusterConfig {
  // name is the slot key used for lookups (e.g. "local", "staging").
  name: string;
  // displayName is the human-friendly label; falls back to name when empty.
  displayName?: string;
  // domain is the single value the add/edit flow collects (e.g.
  // "staging.example.com"). endpoint / issuer / clientId are composed from it
  // by convention. Stored so an edit can round-trip the domain instead of
  // reverse-engineering it from the endpoint.
  domain?: string;
  // endpoint is the gRPC address (host:port).
  endpoint: string;
  issuer?: string;
  clientId?: string;
  // pat is an optional Personal Access Token (mql_pat_<...>) sent as
  // `Authorization: Bearer <pat>`. When set it short-circuits the OIDC flow --
  // the token IS the credential.
  pat?: string;
  // local marks a cluster whose data is disposable -- a k3d parity cluster, a
  // scratch stack. It gates the write confirmation on a mutation run
  // (memql#3309): reads run freely everywhere, and a mutation against a
  // cluster that is NOT local prompts once, naming the cluster and the
  // construct.
  //
  // ABSENT MEANS NOT LOCAL, and that default is load-bearing. Every cluster
  // already in an operator's clusters.yaml predates this field, so an absent
  // flag reading as "local" would silently disable the confirmation on exactly
  // the clusters -- staging, production -- it exists for.
  //
  // The cockpit carries the same field (`Local bool` with `yaml:"local,omitempty"`,
  // znasllc-io/memql-cockpit#332) so a round trip through either tool preserves
  // it. That `omitempty` is why `false` must serialise as ABSENT rather than as
  // `local: false`: the cockpit drops the key on write, so a tool that wrote it
  // back would make the two churn the file against each other forever.
  local?: boolean;
}

export interface ClustersFile {
  clusters: ClusterConfig[];
  selectedCluster: string;
}

// displayLabel returns the label any UI surface should show.
export function displayLabel(c: ClusterConfig): string {
  return c.displayName && c.displayName !== "" ? c.displayName : c.name;
}

// needsAuth reports whether a cluster lacks enough credentials to dial.
// Configured means an endpoint AND either a PAT or an issuer/clientId pair.
// An empty endpoint counts as not-configured: even with auth fields set there
// is nowhere to dial.
export function needsAuth(c: ClusterConfig): boolean {
  if (c.endpoint === "") return true;
  if (c.pat !== undefined && c.pat !== "") return false;
  return (
    c.issuer === undefined || c.issuer === "" ||
    c.clientId === undefined || c.clientId === ""
  );
}

// isOidcOnly reports a cluster this extension cannot authenticate itself:
// OIDC is configured but no PAT is present. B1 supports PAT auth only, so
// these clusters must be authenticated in the cockpit first.
//
// An empty endpoint disqualifies the cluster, matching needsAuth: with
// nowhere to dial, "authenticate this in the memQL Cockpit and come back" is
// simply the wrong instruction -- the honest answer is "not configured", and
// both callers (ConnectionManager's error message, the Clusters tree tooltip)
// check isOidcOnly BEFORE needsAuth, so without this the misleading half won.
export function isOidcOnly(c: ClusterConfig): boolean {
  if (c.endpoint === "") return false;
  const noPat = c.pat === undefined || c.pat === "";
  const hasOidc =
    c.issuer !== undefined && c.issuer !== "" &&
    c.clientId !== undefined && c.clientId !== "";
  return noPat && hasOidc;
}
