import { describe, expect, it } from "vitest";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  credentialFingerprint,
  credentialFromRow,
} from "../../src/apps/deployables/sources/rows";

// The deployable page, and what stands behind it.

// ---------------------------------------------------------------------------
// The credential card: what a person sees of a token, and never the token
// ---------------------------------------------------------------------------

const CARD: Row = {
  id: "cred-1",
  ownerUserId: "u-me",
  host: "github.com",
  label: "acme deploy token",
  fingerprint: "sha256:ab12cd34",
  status: "active",
  lastUsedAt: "2026-09-01T12:00:00Z",
  revokedAt: "",
  createdAt: "2026-08-20T00:00:00Z",
};

describe("the credential card", () => {
  it("projects the card fields and nothing that could be a value", () => {
    const card = credentialFromRow(CARD);
    expect(card).toEqual({
      id: "cred-1",
      ownerUserId: "u-me",
      host: "github.com",
      label: "acme deploy token",
      fingerprint: "sha256:ab12cd34",
      status: "active",
      lastUsedAt: "2026-09-01T12:00:00Z",
      revokedAt: "",
      createdAt: "2026-08-20T00:00:00Z",
    });
    // The projection has no home for a token. A row that carried one -- it
    // never should -- would be dropped here rather than reaching a chip.
    expect(Object.keys(credentialFromRow({ ...CARD, token: "ghp_should_never_arrive" }))).not.toContain("token");
  });

  it("reads a subscription envelope the same as a seed row", () => {
    const folded = credentialFromRow({
      id: "cred-1",
      createdAt: "2026-08-20T00:00:00Z",
      payload: { ...CARD, id: "payload-id-that-must-not-win" },
    });
    expect(folded.id).toBe("cred-1");
    expect(folded.label).toBe("acme deploy token");
    expect(folded.status).toBe("active");
  });

  describe("what counts as news on a credential", () => {
    // Both directions, pinned. Anything named in a fingerprint announces
    // itself, so a liveness field turns the list into a strobe; a fingerprint
    // that misses a real change makes it go quiet exactly when somebody
    // needed telling.
    const base = credentialFromRow(CARD);

    it("fires on a rename, a revocation, a host change or a rotated fingerprint", () => {
      for (const change of [
        { label: "renamed" },
        { status: "revoked" },
        { host: "gitlab.com" },
        { fingerprint: "sha256:ff00ff00" },
      ]) {
        expect(credentialFingerprint({ ...base, ...change })).not.toBe(credentialFingerprint(base));
      }
    });

    it("stays SILENT on lastUsedAt -- a heartbeat is not news", () => {
      // `lastUsedAt` moves on every fetch of every source that uses the
      // credential. Naming it would ring the chip on a ten-minute poll cycle.
      expect(credentialFingerprint({ ...base, lastUsedAt: "2026-09-02T09:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
      expect(credentialFingerprint({ ...base, revokedAt: "2026-09-02T09:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
      expect(credentialFingerprint({ ...base, createdAt: "2027-01-01T00:00:00Z" })).toBe(
        credentialFingerprint(base),
      );
    });
  });
});
