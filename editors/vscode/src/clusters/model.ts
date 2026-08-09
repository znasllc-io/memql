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
  // token is the BEARER this extension dials with. It must be an
  // identity-issued JWT ACCESS TOKEN -- the thing `POST <issuer>/oauth/token`
  // returns as `access_token`.
  //
  // It is NOT a Personal Access Token, and the field is named `token` rather
  // than `pat` because that mis-naming was itself the bug (memql#3383). A PAT
  // is a database lookup that only the identity node can perform: on a bff the
  // verifier has no PAT path wired at all and returns "verifier: PAT path not
  // wired on this node" BEFORE any lookup, so a perfectly VALID PAT fails
  // exactly like a forged one. The mesh verifies bearers via JWKS, so a JWT is
  // the only credential class that can work here. src/connection/credentials.ts
  // detects an `mql_pat_` / `mql_wkr_` value and refuses it by name rather
  // than letting it fail as a bare handshake error.
  //
  // Short-lived by design: identity issues 900-second access tokens
  // (component/identity/config.go DefaultAccessTokenTTLSeconds). `refreshToken`
  // below is what keeps a session alive past that.
  token?: string;
  // refreshToken is the INGEST path for the long-lived (30-day) refresh token
  // that renews `token`.
  //
  // It is deliberately not the storage: this file is plaintext and shared with
  // the memQL Cockpit, and a 30-day credential does not belong there. The
  // resolver takes custody on the first successful exchange -- the rotated
  // token goes into VS Code's SecretStorage and this key is DELETED from the
  // file. See src/connection/credentials.ts for the full rationale, including
  // why the access token itself stays in the shared file.
  refreshToken?: string;
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

// needsAuth reports whether a cluster lacks enough to dial at all.
//
// Configured means an endpoint AND some credential to present -- either a
// bearer token or a refresh token the resolver can exchange for one. An
// issuer/clientId pair is NOT a credential: it names where tokens come from,
// not one you hold. Treating it as sufficient is what made a PAT-less OIDC
// cluster look configured and then fail at the handshake (memql#3383).
//
// An empty endpoint counts as not-configured: even with a live token there is
// nowhere to dial.
//
// This is the RESTING, synchronous view -- it cannot see a refresh token that
// has already been taken into SecretStorage. That is only ever wrong in the
// harmless direction: the first successful exchange writes the fresh access
// token back into `token`, so the resting view is accurate from then on.
//
// RE-EXAMINED FOR memql#3403, AND DELIBERATELY UNCHANGED.
//
// The extension can now authenticate itself (`memql.clusters.signIn`), and the
// question that raised was whether an `issuer` should therefore stop counting
// as "needs auth". It should not, and the reason is that this predicate answers
// a question about the PRESENT, not about what is reachable: a cluster with an
// issuer and no token still cannot dial, and a row that claimed otherwise would
// send an operator to a connect attempt that fails at the handshake. What
// memql#3403 changed is the REMEDY, not the fact -- the recovery for `true` is
// no longer "go and paste a token from somewhere else" but "run Sign In", and
// that lives in the command and in the offer made on a failed connect, both of
// which consult src/auth/signin.ts `canSignIn` for whether an issuer exists.
//
// Keeping the two separate is what stops the predicate from lying in either
// direction: `needsAuth` says a credential is missing, `canSignIn` says whether
// this editor can go and get one, and a cluster can genuinely be any
// combination of the two.
export function needsAuth(c: ClusterConfig): boolean {
  if (c.endpoint.trim() === "") return true;
  return (c.token ?? "").trim() === "" && (c.refreshToken ?? "").trim() === "";
}
