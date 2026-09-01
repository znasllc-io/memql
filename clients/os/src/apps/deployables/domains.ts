import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// Custom domains, as this panel understands them (epic memql#4805).
//
// Everything here is PURE -- a row in, a string or a record out -- so the
// vocabulary, the guidance and the failure sentences are testable without a
// DOM, a cluster or a subscription. The panel is presentation over server law:
// v1:platform:customDomain is clusterOwner tier and the three D10 guards run in
// Go, so nothing in this file is a check.

/** The bindings themselves (`dsl/platform/concepts.memql`). */
export const CUSTOM_DOMAIN_CONCEPT = "v1:platform:customDomain";

export interface DomainRow {
  id: string;
  siteId: string;
  hostname: string;
  accountId: string;
  token: string;
  status: string;
  failureReason: string;
  failureDetail: string;
  lastCheckedAt: string;
  verifiedAt: string;
  issuedAt: string;
  removedAt: string;
  createdAt: string;
}

/** Projects a raw wire row. Ids arrive bare; the fold does no projection. */
export function domainFromRow(row: Row): DomainRow {
  return {
    id: rowString(row, "id"),
    siteId: rowString(row, "siteId"),
    hostname: rowString(row, "hostname"),
    accountId: rowString(row, "accountId"),
    token: rowString(row, "token"),
    status: rowString(row, "status"),
    failureReason: rowString(row, "failureReason"),
    failureDetail: rowString(row, "failureDetail"),
    lastCheckedAt: rowString(row, "lastCheckedAt"),
    verifiedAt: rowString(row, "verifiedAt"),
    issuedAt: rowString(row, "issuedAt"),
    removedAt: rowString(row, "removedAt"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * What counts as NEWS on a binding, for the arrival cue.
 *
 * A HEARTBEAT IS NOT NEWS, and this is the row where getting that wrong would
 * be loudest: `lastCheckedAt` moves on every sweep pass for every non-terminal
 * binding, every two minutes, forever. Naming it here would turn the panel into
 * a strobe on a two-minute cycle -- the standing badge the cue exists not to be
 * (clients/os/README.md).
 *
 * What a PERSON would call a change on a domain is its status flipping or the
 * reason it is stuck changing. Both are here; the timestamp is not, and the
 * panel displays it continuously instead, which is the right home for something
 * that is always true and never news.
 */
export function domainFingerprint(d: DomainRow): string {
  return `${d.status}|${d.failureReason}|${d.failureDetail}`;
}

// ---------------------------------------------------------------------------
// The walk
// ---------------------------------------------------------------------------

/**
 * The four stops on the way to serving, in order.
 *
 * A STEPPED RAIL IS HONEST HERE because this genuinely is a sequence: a binding
 * passes through each of these in turn and cannot skip one. `removing` and
 * `removed` are deliberately NOT on it -- they are a different journey that can
 * start from anywhere, and drawing them as "step five" would say a removed
 * domain had got further than a live one.
 */
export const DOMAIN_STEPS = [
  {
    status: "pending_dns",
    label: "Records",
    blurb: "Create the two DNS records below at your registrar.",
  },
  {
    status: "verifying",
    label: "Checking DNS",
    blurb: "We look every couple of minutes and say which record is still missing.",
  },
  {
    status: "issuing",
    label: "Certificate",
    blurb: "Both records check out. Getting a certificate for the domain.",
  },
  {
    status: "live",
    label: "Serving",
    blurb: "The domain is serving this deployable over HTTPS.",
  },
] as const;

export const TERMINAL_STATUSES = new Set(["removed"]);

/** Where a status sits on the rail; -1 for the removal path. */
export function stepIndexFor(status: string): number {
  return DOMAIN_STEPS.findIndex((s) => s.status === status);
}

export function isRemovalPath(status: string): boolean {
  return status === "removing" || status === "removed";
}

/** The word for a status, in the reader's vocabulary rather than the schema's. */
export function statusLabel(status: string): string {
  switch (status) {
    case "pending_dns":
      return "waiting for DNS";
    case "verifying":
      return "checking DNS";
    case "issuing":
      return "getting a certificate";
    case "live":
      return "serving";
    case "removing":
      return "removing";
    case "removed":
      return "removed";
    default:
      // An unrecognised status renders AS ITSELF. Inventing a friendly word
      // for a value this build has never seen would hide a newer cluster's
      // real answer behind a guess.
      return status || "status unknown";
  }
}

export type DomainTone = "ok" | "warn" | "error" | "muted";

/** The tone a status carries, before its failure reason is considered. */
export function statusTone(d: DomainRow): DomainTone {
  if (d.status === "live") return "ok";
  if (isRemovalPath(d.status)) return "muted";
  // A binding that is mid-walk with a typed reason is BLOCKED, and looks it.
  // One that is simply waiting is not a problem and must not be coloured like
  // one -- most of a domain's life is spent legitimately waiting for DNS.
  return d.failureReason === "" ? "warn" : "error";
}

// ---------------------------------------------------------------------------
// The typed failure reasons, rendered
// ---------------------------------------------------------------------------

/**
 * The server's four typed reasons, as sentences.
 *
 * KEYED ON THE STABLE REASON, and an UNKNOWN reason keeps its own token rather
 * than being given a friendly sentence: inventing one for a failure this build
 * does not recognise is how a real fault gets mistaken for a user error. The
 * same rule publishRefusal.ts already states for publishes.
 *
 * Each sentence says WHAT IS WRONG and WHAT TO DO, because the whole point of
 * the typed reason is that the panel can name exactly which record still needs
 * fixing (design D5). `failureDetail` renders beside it and says what we
 * actually saw -- the sentence and the observation are two different facts and
 * both are needed.
 */
const FAILURE_SENTENCES: Record<string, string> = {
  dns_token_missing:
    "The ownership record is not published yet. Create the TXT record below, exactly as shown.",
  dns_not_pointing:
    "The domain does not point at this cluster yet. Create the second record below, then give your DNS provider a few minutes.",
  no_acme_issuer:
    "This cluster is not set up to issue certificates, so the domain cannot be served over HTTPS. An operator sets an ACME issuer for the cluster; everything else about this binding is ready.",
  issuance_failed:
    "The certificate could not be issued. The detail below is what the cluster reported.",
};

export function failureSentence(reason: string): string {
  if (reason === "") return "";
  return FAILURE_SENTENCES[reason] ?? reason;
}

/** Whether this build recognises the reason, so the panel can mark a raw one. */
export function isKnownFailure(reason: string): boolean {
  return reason !== "" && reason in FAILURE_SENTENCES;
}

// ---------------------------------------------------------------------------
// The records a client has to create
// ---------------------------------------------------------------------------

export interface DnsRecord {
  /** The registrar's "Type" field. */
  kind: string;
  /** The registrar's "Name" / "Host" field, fully qualified. */
  name: string;
  /** The registrar's "Value" / "Points to" field. */
  value: string;
  /** What this record is for, in one line. */
  purpose: string;
}

/**
 * The label the ownership record sits under.
 *
 * MIRRORS integrations/customdomain's VerifyPrefix. The underscore is
 * load-bearing rather than stylistic: it is not legal in a hostname, so this
 * name can never collide with something the client is actually serving.
 */
export const VERIFY_PREFIX = "_memql-verify";

/** Lowercase, trimmed, no trailing root dot. */
export function normalizeHostname(h: string): string {
  return h.trim().toLowerCase().replace(/\.$/, "");
}

/**
 * Whether a hostname is a zone apex rather than a subdomain.
 *
 * A LABEL COUNT, mirroring the server's own approximation and for the same
 * stated reason: what it decides is only which record the guidance asks for,
 * and the rule that actually governs it -- a CNAME is illegal at a zone apex --
 * depends on where the client's zone starts, which no string can tell you.
 * Two labels is right for acme.com and wrong for acme.co.uk, and being wrong
 * that way means suggesting an ALIAS where a CNAME would also have worked.
 */
export function isApex(hostname: string): boolean {
  const h = normalizeHostname(hostname);
  return h !== "" && (h.match(/\./g) ?? []).length <= 1;
}

/**
 * The host a client points their own domain AT.
 *
 * MIRRORED FROM THE SERVER, NOT AUTHORITATIVE -- the same relationship
 * hostname.ts has with the site slug rules. `component/frontdoor.OsHost` is the
 * derivation, and an operator may override it with
 * MEMQL_CUSTOM_DOMAIN_EDGE_HOST for a cluster fronted by a CDN. The panel
 * cannot see that override, so the failure detail is what corrects it: a
 * `dns_not_pointing` miss names the host and the addresses the cluster
 * actually wanted, verbatim, and that sentence outranks this one.
 */
export function edgeHostFor(domain: string): string {
  const d = normalizeHostname(domain);
  return d === "" ? "" : `os.${d}`;
}

/**
 * The two records, in the order somebody should create them.
 *
 * OWNERSHIP FIRST, because it is the one whose failure a person can act on
 * immediately: "publish this TXT record" is a complete instruction, while "your
 * domain does not point here yet" often means "wait for propagation". The
 * server's sweep checks them in the same order for the same reason.
 */
export function recordsFor(d: DomainRow, domain: string): DnsRecord[] {
  const host = normalizeHostname(d.hostname);
  const edge = edgeHostFor(domain);
  if (host === "") return [];

  const ownership: DnsRecord = {
    kind: "TXT",
    name: `${VERIFY_PREFIX}.${host}`,
    value: d.token,
    purpose: "Proves you control this domain. Nothing is issued until it checks out.",
  };

  // A CNAME IS ILLEGAL AT A ZONE APEX -- RFC 1034 forbids one alongside the SOA
  // and NS records every apex carries -- so an apex is asked for the ALIAS /
  // ANAME record most providers offer instead, with the A-record fallback in
  // the caption rather than as a second row nobody with ALIAS support needs.
  const pointing: DnsRecord = isApex(host)
    ? {
        kind: "ALIAS",
        name: host,
        value: edge,
        purpose: "Sends the domain's traffic here. A CNAME is not legal at a domain's root.",
      }
    : {
        kind: "CNAME",
        name: host,
        value: edge,
        purpose: "Sends the domain's traffic here.",
      };

  return [ownership, pointing];
}

/**
 * Which record a typed reason is about, so the panel can mark the one that is
 * still wrong rather than making somebody read both.
 */
export function recordAtFault(reason: string): string {
  if (reason === "dns_token_missing") return "TXT";
  if (reason === "dns_not_pointing") return "pointing";
  return "";
}

export function isRecordAtFault(record: DnsRecord, reason: string): boolean {
  const fault = recordAtFault(reason);
  if (fault === "") return false;
  if (fault === "TXT") return record.kind === "TXT";
  return record.kind !== "TXT";
}

/** Sorts a site's bindings: the ones needing attention first, removed last. */
export function sortDomains(rows: DomainRow[]): DomainRow[] {
  const rank = (d: DomainRow): number => {
    if (d.status === "removed") return 4;
    if (d.status === "removing") return 3;
    if (d.status === "live") return 2;
    // Anything mid-walk is what somebody opened this panel to look at.
    return d.failureReason === "" ? 1 : 0;
  };
  return [...rows].sort((a, b) => {
    const byRank = rank(a) - rank(b);
    return byRank !== 0 ? byRank : a.hostname.localeCompare(b.hostname);
  });
}
