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

export const DOMAIN_SETUP_STEPS = ["Ownership", "DNS", "Certificate", "Serving"] as const;

export const TERMINAL_STATUSES = new Set(["removed"]);

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
    "The ownership record is not published yet. Create the TXT record in the Ownership step, exactly as shown.",
  dns_not_pointing:
    "The domain does not point at this cluster yet. Create the DNS record shown in the DNS step, then allow time for propagation.",
  no_acme_issuer:
    "This cluster is not set up to issue certificates, so the domain cannot be served over HTTPS. An operator sets an ACME issuer for the cluster; everything else about this binding is ready.",
  issuance_failed:
    "The certificate could not be issued. The detail below is what the cluster reported.",
  removal_failed:
    "The cluster could not take this domain down yet, so its name is still held. It tries again by itself; the detail below is what it reported.",
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

/** A conservative hint for the provider-relative name. Two-label roots use
 * @; other names remain fully qualified, which providers can normalize.
 * This never determines which DNS record types or addresses are supported. */
export function isApex(hostname: string): boolean {
  const h = normalizeHostname(hostname);
  return h !== "" && (h.match(/\./g) ?? []).length <= 1;
}

export interface DomainDNSGuidance {
  edgeHost: string;
  ipv4: string[];
  ipv6: string[];
}

export type PointingMethod = "ADDRESS" | "ALIAS" | "CNAME";

export function recordsFor(d: DomainRow, guidance: DomainDNSGuidance | null, method: PointingMethod = "ADDRESS"): DnsRecord[] {
  const host = normalizeHostname(d.hostname);
  if (host === "") return [];

  const ownership: DnsRecord = {
    kind: "TXT",
    name: `${VERIFY_PREFIX}.${host}`,
    value: d.token,
    purpose: "Proves you control this domain. Nothing is issued until it checks out.",
  };

  if (!guidance) return [ownership];
  const name = isApex(host) ? "@" : host;
  if (method === "ADDRESS") {
    return [ownership, ...guidance.ipv4.map(value => ({ kind: "A", name, value, purpose: "Routes visitors to this cluster; your domain binding selects this website." })),
      ...guidance.ipv6.map(value => ({ kind: "AAAA", name, value, purpose: "Routes IPv6 visitors to this cluster." }))];
  }
  return [ownership, { kind: method, name, value: guidance.edgeHost,
    purpose: "Routes visitors to this cluster; your domain binding selects this website." }];
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

/**
 * Whether a binding belongs in a deployable's list of domains.
 *
 * A DOMAIN SOMEBODY CANCELLED LEAVES THE LIST, at once. The list used to keep
 * every row the concept keeps -- `removing` and `removed` included -- on the
 * argument that what a cluster served, and when, is an audit worth showing. It
 * is worth KEEPING, and the engine keeps it; but a list of the names a
 * deployable answers on is not where it is read. The owner cancelled a domain
 * and watched it sit there: "it should remove the item from the list."
 *
 * So the list is what answers, or is on its way to answering. `removed` is
 * history. `removing` is the cluster tidying up, which is its business and not
 * a row for a person to watch -- UNLESS the tidying has failed. That one stays,
 * and says so, because only `removed` frees a hostname: a removal stuck out of
 * sight would refuse that name to whoever tried to add it again, with nothing
 * on the page to say why.
 */
export function isListedDomain(d: DomainRow): boolean {
  if (d.status === "removed") return false;
  if (d.status === "removing") return d.failureReason !== "";
  return true;
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

/** Ownership is checked first on every sweep. A pointing failure proves that
 * ownership passed that same sweep; a generic verifying status does not. */
export function domainSetupStep(d: DomainRow): number {
  if (d.status === "live") return 3;
  if (d.status === "issuing") return 2;
  if (d.failureReason === "dns_not_pointing") return 1;
  return 0;
}

// ---------------------------------------------------------------------------
// The setup, as steps -- what the add wizard and a binding's own page both draw
// ---------------------------------------------------------------------------

export type DomainStepId = "ownership" | "dns" | "certificate" | "serving";

/** A subset of the rail's closed state set; see kit/Rail. */
export type DomainStepState = "done" | "current" | "open" | "waiting" | "stopped" | "ahead";

export interface DomainSetupStepReading {
  id: DomainStepId;
  name: string;
  state: DomainStepState;
  /** The step's one line: what it settled as, or what it is waiting for. */
  answer: string;
}

/**
 * WAITING FOR A RECORD IS NOT A FAILURE, and the server's vocabulary makes it
 * easy to draw as one. `dns_token_missing` and `dns_not_pointing` arrive in
 * `failureReason`, so the first thing a person saw after binding a domain was
 * a stopped step and a problem sentence -- for the ordinary state of a record
 * they had not had the chance to create yet. Those two mean "not there yet",
 * and the sweep will look again; only a reason the sweep cannot outwait is a
 * stop.
 */
export function isWaitingReason(reason: string): boolean {
  return reason === "" || reason === "dns_token_missing" || reason === "dns_not_pointing";
}

/**
 * The four stages of a binding, read off its row.
 *
 * `siteLive` is the deployable's own status, which is a different fact from
 * the domain's: a binding finishes ITS setup when the certificate is ready,
 * and whether a visitor gets anything is decided by the deployable. A finished
 * binding on a deployable that is not live is `open` -- the next thing that
 * happens is the person's, and it is not on this page.
 */
export function domainSteps(d: DomainRow, siteLive: boolean): DomainSetupStepReading[] {
  const at = domainSetupStep(d);
  const blocked = !isWaitingReason(d.failureReason);
  const stateAt = (index: number): DomainStepState => {
    if (index < at) return "done";
    if (index > at) {
      // THE DNS RECORDS CAN BE READ BEFORE OWNERSHIP PASSES. They are created
      // in the same place as the ownership record, and making somebody wait
      // for one check before showing them the next record costs a second trip
      // to their DNS provider.
      return index === 1 ? "waiting" : "ahead";
    }
    return blocked ? "stopped" : "current";
  };
  const live = d.status === "live";
  return [
    {
      id: "ownership",
      name: "Ownership",
      state: live ? "done" : stateAt(0),
      answer: at > 0 ? "Verified" : blocked ? "Needs attention" : "Waiting for the TXT record",
    },
    {
      id: "dns",
      name: "DNS",
      state: live ? "done" : stateAt(1),
      // SAID ON THE LINE, because the owner asked the moment they saw it: "we
      // are on Ownership, yet I can click DNS -- is that on purpose?" It is.
      // One sweep runs the ownership lookup and then the pointing lookup
      // (integrations/customdomain/reconcile.go `verify`), and reports the
      // first record it cannot find. Neither record depends on the other
      // EXISTING, so creating both in one visit is the fast path: the next
      // pass finds both and walks straight to the certificate.
      answer: at > 1 ? "Points at this cluster" : at === 1 ? (blocked ? "Needs attention" : "Waiting for the records to point here") : blocked ? "" : "Can be created now; checked together with ownership",
    },
    {
      id: "certificate",
      name: "Certificate",
      state: live ? "done" : stateAt(2),
      answer: at > 2 ? "Issued" : at === 2 ? (blocked ? "Needs attention" : "Being issued") : "",
    },
    {
      id: "serving",
      name: "Serving",
      state: live ? (siteLive ? "done" : "open") : "ahead",
      answer: live ? (siteLive ? `https://${d.hostname}` : "Ready. The deployable is not live yet") : "",
    },
  ];
}

export interface DomainSetupReading {
  word: string;
  detail: string;
  tone: "busy" | "live" | "paused" | "none";
}

/**
 * The setup in one line, for the bar on the window's bottom edge.
 *
 * It says WHOSE TURN IT IS. While a record is outstanding the turn is shared --
 * the person creates it, the cluster looks for it -- and the sentence names
 * both halves so that neither is mistaken for the whole.
 */
export function domainSetupReading(d: DomainRow, siteLive: boolean): DomainSetupReading {
  if (d.status === "live") {
    return siteLive
      ? { word: "Serving", detail: `${d.hostname} is verified and has its certificate`, tone: "live" }
      : { word: "Ready", detail: "make the deployable live to serve it at this domain", tone: "none" };
  }
  if (!isWaitingReason(d.failureReason)) {
    return { word: "Needs attention", detail: "the open step says what the cluster reported", tone: "paused" };
  }
  const at = domainSetupStep(d);
  if (at === 0) {
    return { word: "Waiting for the ownership record", detail: "create it at your DNS provider; this keeps checking if you leave", tone: "busy" };
  }
  if (at === 1) {
    return { word: "Waiting for DNS to point here", detail: "a change can take a while to spread; this keeps checking if you leave", tone: "busy" };
  }
  return { word: "Getting a certificate", detail: "nothing for you to do; this moves on by itself", tone: "busy" };
}
