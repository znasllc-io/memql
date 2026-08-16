// Is this cluster claimed yet? (znasllc-io/memql#3885)
//
// WHAT WAS WRONG. Clicking a freshly installed cluster offered SIGN IN. There
// is no account to sign in to yet -- a cluster is claimed by its FIRST sign-in,
// and that sign-in is what creates the owner -- so the browser flow times out
// and falls back to a device code that cannot complete either:
//
//   the browser sign-in could not complete (No sign-in callback arrived within
//   120 seconds...). Falling back to a device code.
//   enter code AGXC-QCLZ at https://identity.<domain>/device to finish signing in.
//
// The operator is asked to authorize a device against an account that does not
// exist. Nothing in that sequence is wrong on its own; the entry point simply
// never asked which of three states the cluster was in.
//
// # The signal
//
// `GET <issuer>/setup` answers the question, and it is the only thing that
// does without a credential:
//
//   200  the ownership wizard RENDERS -- the cluster is UNCLAIMED
//   404  the wizard is SEALED        -- the cluster is CLAIMED
//
// That is not a side effect being read as a signal. The seal is wired to
// `Store.HasOwnerUser` -- "an active user holding the cluster-OWNER role
// exists" -- which memql#3415 chose precisely because an owner user is
// DEFINITIONAL proof the cluster was claimed and is not something a stray row
// produces. The page is pre-auth by necessity: it mints the first owner, so it
// has to be reachable before anyone holds anything.
//
// It also fails in the safe direction on its own terms. memql#3415 made "cannot
// prove this cluster is unclaimed" SEAL rather than serve, so a cluster whose
// state cannot be read answers 404 and we treat it as claimed -- which offers
// sign-in, exactly today's behaviour. A probe that cannot reach the host at all
// returns `unknown` and does the same.
//
// # Why the remedy is the wizard rather than the recovered magic link
//
// memql#3884 recovers the owner's magic link out of the identity pod's log
// during an install and opens it. That is the fast path and it stays. It cannot
// be the ENTRY POINT's answer, though: it needs `kubectl exec` into the
// workload, so it works only for a local cluster this extension installed, and
// only while the log window still holds the line. The operator who dismissed
// the notification, whose link expired, or who is looking at a remote cluster
// has none of that.
//
// `/setup` needs none of it. If it answers 200 the cluster is offering the
// wizard to whoever asks, so opening it is both the correct remedy and one that
// works identically for local and remote.

/** Binds to `fetch`. Injected so this module is testable without a network. */
export type FetchLike = (
  url: string,
  init?: { method?: string; redirect?: "follow" | "manual" | "error"; signal?: AbortSignal },
) => Promise<{ status: number }>;

/**
 * What we know about a cluster's ownership.
 *
 * `unknown` is a first-class answer, not an error. "I could not ask" and "there
 * is no owner" want opposite remedies, and reporting the first as the second
 * would send an operator to a wizard that is going to 404 at them.
 */
export type ClaimState = "unclaimed" | "claimed" | "unknown";

/** The path whose status code carries the answer. */
export const SETUP_PATH = "/setup";

/**
 * setupUrl composes the wizard URL from a cluster's issuer.
 *
 * Returns "" when the issuer is missing or is not an absolute http(s) URL --
 * there is nothing to probe and nothing to open, and guessing a host from the
 * gRPC endpoint would be inventing an address rather than reading one.
 */
export function setupUrl(issuer: string | undefined): string {
  const raw = (issuer ?? "").trim();
  if (raw === "") return "";
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return "";
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") return "";
  const base = parsed.href.endsWith("/") ? parsed.href.slice(0, -1) : parsed.href;
  return `${base}${SETUP_PATH}`;
}

export interface ProbeDeps {
  fetch: FetchLike;
  signal?: AbortSignal;
}

/**
 * probeClaimState asks the cluster whether it has an owner.
 *
 * REDIRECTS ARE NOT FOLLOWED. The answer is the status code of `/setup` itself,
 * and a host that redirects it somewhere friendly (a login page, an SSO
 * front door) would otherwise be read as whatever that destination answers.
 * A 3xx is `unknown` for the same reason a 500 is: the wizard neither rendered
 * nor sealed, so nothing was established.
 */
export async function probeClaimState(
  issuer: string | undefined,
  deps: ProbeDeps,
): Promise<ClaimState> {
  const url = setupUrl(issuer);
  if (url === "") return "unknown";

  let status: number;
  try {
    const res = await deps.fetch(url, { method: "GET", redirect: "manual", signal: deps.signal });
    status = res.status;
  } catch {
    // Unreachable host, TLS failure, aborted probe. Not evidence about
    // ownership.
    return "unknown";
  }

  if (status === 200) return "unclaimed";
  if (status === 404) return "claimed";
  return "unknown";
}

// WHAT THE ENTRY POINT DOES WITH THIS ANSWER LIVES NEXT DOOR (memql#3906).
//
// `entryActionFor` used to be here, mapping unclaimed -> claim and everything
// else -> sign-in. That mapping survives verbatim as `routeForClaimState` in
// clusters/ownershipRoute.ts; it moved because it is one branch of a three-way
// decision rather than the whole of one, and leaving a second function that
// answers two thirds of the same question is how the two come to disagree.
//
// This module's job stops at reading the status code.
//
// ONE LIMIT WORTH KNOWING BEFORE TRUSTING AN ANSWER FROM HERE. `probeClaimState`
// binds to the caller's `fetch`, and Node's verifies against its OWN bundled
// roots rather than the OS/NSS store the local installer puts its mkcert CA
// into. So a cluster on the machine that installed it answers `unknown` --
// UNABLE_TO_VERIFY_LEAF_SIGNATURE, caught above -- not because its ownership is
// unreadable but because the handshake never completed. That is why
// ownershipRoute.ts asks the install receipt first and reaches this only for
// the clusters local evidence cannot speak for.
