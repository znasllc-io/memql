import { describe, expect, it } from "vitest";

import {
  DOMAIN_STEPS,
  domainFingerprint,
  domainFromRow,
  edgeHostFor,
  failureSentence,
  isApex,
  isKnownFailure,
  isRecordAtFault,
  recordsFor,
  sortDomains,
  statusLabel,
  statusTone,
  stepIndexFor,
  type DomainRow,
} from "../../src/apps/deployables/domains";

// The Domains panel's vocabulary, guidance and cue -- pure, so every rule it
// states can be checked without a DOM, a cluster or a subscription.

function domain(over: Partial<DomainRow> = {}): DomainRow {
  return {
    id: "cd-1",
    siteId: "site-shop",
    hostname: "www.acme.com",
    accountId: "",
    token: "tok-abcdef0123456789",
    status: "pending_dns",
    failureReason: "",
    failureDetail: "",
    lastCheckedAt: "",
    verifiedAt: "",
    issuedAt: "",
    removedAt: "",
    createdAt: "2026-09-01T00:00:00Z",
    ...over,
  };
}

// ===========================================================================
// The arrival cue
// ===========================================================================

describe("the arrival cue's fingerprint", () => {
  // A HEARTBEAT IS NOT NEWS, and this is the row where getting it wrong is
  // loudest: `lastCheckedAt` moves for every non-terminal binding every two
  // minutes, forever. Naming it would turn the panel into a strobe.
  it("does not move when only lastCheckedAt does", () => {
    const before = domainFingerprint(domain({ lastCheckedAt: "2026-09-01T12:00:00Z" }));
    const after = domainFingerprint(domain({ lastCheckedAt: "2026-09-01T12:02:00Z" }));
    expect(after).toBe(before);
  });

  it("does not move when only verifiedAt or issuedAt does", () => {
    const before = domainFingerprint(domain());
    expect(domainFingerprint(domain({ verifiedAt: "2026-09-01T12:00:00Z" }))).toBe(before);
    expect(domainFingerprint(domain({ issuedAt: "2026-09-01T12:00:00Z" }))).toBe(before);
  });

  // A STATUS FLIP IS NEWS -- it is the thing somebody watching this panel is
  // waiting for.
  it("moves on a status flip", () => {
    const before = domainFingerprint(domain({ status: "issuing" }));
    const after = domainFingerprint(domain({ status: "live" }));
    expect(after).not.toBe(before);
  });

  // SO IS THE REASON CHANGING. "the TXT record is missing" becoming "it does
  // not point here" is real progress and the reader should see it announce
  // itself.
  it("moves when the typed failure reason changes", () => {
    const before = domainFingerprint(domain({ status: "verifying", failureReason: "dns_token_missing" }));
    const after = domainFingerprint(domain({ status: "verifying", failureReason: "dns_not_pointing" }));
    expect(after).not.toBe(before);
  });

  it("moves when what we observed changes, even under one reason", () => {
    const a = domainFingerprint(domain({ failureReason: "dns_not_pointing", failureDetail: "resolves to 1.2.3.4" }));
    const b = domainFingerprint(domain({ failureReason: "dns_not_pointing", failureDetail: "resolves to 5.6.7.8" }));
    expect(a).not.toBe(b);
  });
});

// ===========================================================================
// The records
// ===========================================================================

describe("the DNS records a client has to create", () => {
  it("asks for the ownership TXT record first", () => {
    const records = recordsFor(domain(), "memql.example.com");
    expect(records[0]?.kind).toBe("TXT");
    expect(records[0]?.name).toBe("_memql-verify.www.acme.com");
    expect(records[0]?.value).toBe("tok-abcdef0123456789");
  });

  it("asks a subdomain for a CNAME to this cluster's edge host", () => {
    const records = recordsFor(domain({ hostname: "www.acme.com" }), "memql.example.com");
    const pointing = records[1];
    expect(pointing?.kind).toBe("CNAME");
    expect(pointing?.name).toBe("www.acme.com");
    expect(pointing?.value).toBe("os.memql.example.com");
  });

  // A CNAME IS ILLEGAL AT A ZONE APEX -- RFC 1034 forbids one alongside the
  // SOA and NS records every apex carries -- so an apex is asked for ALIAS.
  it("asks an apex for an ALIAS rather than a CNAME", () => {
    const records = recordsFor(domain({ hostname: "acme.com" }), "memql.example.com");
    expect(records[1]?.kind).toBe("ALIAS");
    expect(records[1]?.name).toBe("acme.com");
  });

  it("normalises the hostname it renders", () => {
    const records = recordsFor(domain({ hostname: "WWW.Acme.com." }), "memql.example.com");
    expect(records[0]?.name).toBe("_memql-verify.www.acme.com");
    expect(records[1]?.name).toBe("www.acme.com");
  });

  it("renders nothing rather than half a record when the hostname is blank", () => {
    expect(recordsFor(domain({ hostname: "" }), "memql.example.com")).toEqual([]);
  });

  // The typed reason says which record it is about, so the panel can point at
  // the one that is still wrong rather than making somebody read both.
  it("marks the record a typed reason is about, and only that one", () => {
    const [txt, pointing] = recordsFor(domain(), "memql.example.com");
    expect(isRecordAtFault(txt!, "dns_token_missing")).toBe(true);
    expect(isRecordAtFault(pointing!, "dns_token_missing")).toBe(false);
    expect(isRecordAtFault(txt!, "dns_not_pointing")).toBe(false);
    expect(isRecordAtFault(pointing!, "dns_not_pointing")).toBe(true);
    // An issuance failure is about neither record: both are correct by then.
    expect(isRecordAtFault(txt!, "no_acme_issuer")).toBe(false);
    expect(isRecordAtFault(pointing!, "no_acme_issuer")).toBe(false);
  });

  it("composes nothing when the cluster did not say which domain it serves", () => {
    expect(edgeHostFor("")).toBe("");
  });
});

describe("apex detection", () => {
  it("counts labels", () => {
    expect(isApex("acme.com")).toBe(true);
    expect(isApex("ACME.COM.")).toBe(true);
    expect(isApex("www.acme.com")).toBe(false);
    expect(isApex("shop.eu.acme.com")).toBe(false);
    expect(isApex("")).toBe(false);
  });
});

// ===========================================================================
// The typed failure reasons
// ===========================================================================

describe("failure sentences", () => {
  it("renders each of the server's four typed reasons as something actionable", () => {
    for (const reason of ["dns_token_missing", "dns_not_pointing", "no_acme_issuer", "issuance_failed"]) {
      const sentence = failureSentence(reason);
      expect(sentence).not.toBe("");
      expect(sentence).not.toBe(reason);
      expect(isKnownFailure(reason)).toBe(true);
    }
  });

  // AN UNKNOWN REASON KEEPS ITS OWN TOKEN. Inventing a friendly sentence for a
  // failure this build does not recognise is how a real fault gets mistaken
  // for a user error.
  it("keeps an unrecognised reason as itself", () => {
    expect(failureSentence("something_new")).toBe("something_new");
    expect(isKnownFailure("something_new")).toBe(false);
  });

  it("says nothing when there is no failure", () => {
    expect(failureSentence("")).toBe("");
    expect(isKnownFailure("")).toBe(false);
  });
});

// ===========================================================================
// Tone and ordering
// ===========================================================================

describe("tone", () => {
  // MOST OF A DOMAIN'S LIFE IS SPENT LEGITIMATELY WAITING. A binding with no
  // typed reason must not be coloured like a problem.
  it("does not treat waiting as an error", () => {
    expect(statusTone(domain({ status: "pending_dns" }))).toBe("warn");
    expect(statusTone(domain({ status: "issuing" }))).toBe("warn");
  });

  it("treats a typed reason mid-walk as blocked", () => {
    expect(statusTone(domain({ status: "verifying", failureReason: "dns_token_missing" }))).toBe("error");
  });

  it("treats serving as fine and the removal path as spent", () => {
    expect(statusTone(domain({ status: "live" }))).toBe("ok");
    expect(statusTone(domain({ status: "removing" }))).toBe("muted");
    expect(statusTone(domain({ status: "removed" }))).toBe("muted");
  });
});

describe("the rail", () => {
  it("places each step in order", () => {
    expect(DOMAIN_STEPS.map((s) => s.status)).toEqual([
      "pending_dns",
      "verifying",
      "issuing",
      "live",
    ]);
  });

  // `removing` / `removed` are a DIFFERENT JOURNEY that can start from
  // anywhere. Drawing them as a fifth stop would say a removed domain had got
  // further than a live one.
  it("keeps the removal path off the rail", () => {
    expect(stepIndexFor("removing")).toBe(-1);
    expect(stepIndexFor("removed")).toBe(-1);
  });
});

describe("ordering", () => {
  it("puts what needs attention first and removed rows last", () => {
    const rows = [
      domain({ id: "a", hostname: "d.acme.com", status: "removed" }),
      domain({ id: "b", hostname: "c.acme.com", status: "live" }),
      domain({ id: "c", hostname: "b.acme.com", status: "verifying", failureReason: "dns_token_missing" }),
      domain({ id: "d", hostname: "a.acme.com", status: "issuing" }),
    ];
    expect(sortDomains(rows).map((d) => d.id)).toEqual(["c", "d", "b", "a"]);
  });
});

describe("statusLabel", () => {
  it("speaks the reader's vocabulary, not the schema's", () => {
    expect(statusLabel("pending_dns")).toBe("waiting for DNS");
    expect(statusLabel("live")).toBe("serving");
  });

  // A newer cluster's status renders AS ITSELF rather than behind a guess.
  it("passes an unrecognised status through", () => {
    expect(statusLabel("something_new")).toBe("something_new");
  });
});

describe("projection", () => {
  it("reads every field the panel renders", () => {
    const d = domainFromRow({
      id: "cd-1",
      siteId: "site-shop",
      hostname: "www.acme.com",
      token: "tok",
      status: "verifying",
      failureReason: "dns_not_pointing",
      failureDetail: "resolves to 1.2.3.4",
      lastCheckedAt: "2026-09-01T12:00:00Z",
    });
    expect(d.hostname).toBe("www.acme.com");
    expect(d.token).toBe("tok");
    expect(d.failureDetail).toBe("resolves to 1.2.3.4");
  });
});
