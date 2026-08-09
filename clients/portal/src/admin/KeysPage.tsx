import type { ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Elsewhere, Reading, Refused } from "./AdminLayout";
import { signingKeyRows, SIGNING_KEY_CONCEPT } from "./rows";
import { surfaceById } from "./urls";
import { lastRotation, useAdminAccess, useAuditTrail, useSigningKeys } from "./useAdminConsole";

// Signing keys.
//
// THE PAGE READS THE PUBLIC FEED, deliberately. Every verifier node in the
// mesh checks a token's signature against /.well-known/jwks.json, so reading
// the same document is the only way this console can tell an operator what the
// cluster is actually publishing rather than what one replica believes. It is
// also why the page is useful during the failure it exists for: a JWKS that
// has gone incoherent across replicas shows up here as the wrong kid, and no
// gated internal read would have shown it.
//
// ROTATION COMES FROM THE AUDIT TRAIL, not from a key's age. The feed carries
// no timestamps -- an RFC 7517 JWK has no birthday -- and the KeyManager's
// in-process createdAt is not something a browser can see. The rotation event
// is the better source anyway: it is the exact instant, it names whether a
// person or the schedule did it, and it is a row.
export function KeysPage(): ReactNode {
  const surface = surfaceById("keys");
  const { role, canAdminister, resolved } = useAdminAccess();
  const keys = useSigningKeys();
  const rotations = useAuditTrail(canAdminister, "configuration");
  const rotated = lastRotation(rotations.data);
  const rows = signingKeyRows(keys.keys);

  if (surface === undefined) return null;
  if (!canAdminister) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} />
      </AdminFrame>
    );
  }

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={
        <MetaButton
          onClick={() => {
            keys.reload();
            rotations.reload();
          }}
        >
          Refresh
        </MetaButton>
      }
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="Signing with"
            value={keys.loading ? "…" : (rows[0]?.kid ?? "no key published")}
            sub="the kid stamped on tokens minted right now"
          />
          <Reading
            label="Keys published"
            value={keys.loading ? "…" : String(rows.length)}
            sub={
              rows.length > 1
                ? "the overlap window is open"
                : "no rotation in flight"
            }
          />
          <Reading
            label="Last rotation"
            value={rotated === null ? "none recorded" : rotated.at}
            sub={rotated === null ? "in the events read so far" : `by ${rotated.by}`}
          />
        </div>
        {keys.origin === "" ? (
          <p className="mt-3 text-sm text-muted">
            This deployment publishes no identity origin, so the console cannot
            find the feed. Set the portal's <code>identityUrl</code> and reload.
          </p>
        ) : null}
        {keys.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read the key feed: {keys.error}</ErrorMessage>
          </div>
        )}
      </Band>

      <Band
        title="Published keys"
        meta={keys.origin === "" ? undefined : `${keys.origin}/.well-known/jwks.json`}
        panel
      >
        {rows.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {keys.loading ? "Reading the feed…" : "The feed published no keys."}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={rows}
            concept={SIGNING_KEY_CONCEPT}
            options={{ bindings: { column: ["kid", "role", "algorithm", "curve", "purpose"] } }}
          />
        )}
      </Band>

      <Band title="How rotation works">
        <p className="max-w-3xl text-sm text-muted">
          The cluster signs every access token with the current Ed25519 keypair
          and rotates on a schedule. During rotation the previous key stays in
          the feed for an overlap window, so tokens minted before the change keep
          verifying until they expire. A second row in the table above means that
          window is open right now. Verifier nodes refresh the feed in the
          background and on any kid they have not seen, so a rotation reaches the
          mesh without a restart.
        </p>
      </Band>

      <Elsewhere what="Rotating a key">
        There is no rotate button here, and in a deployed cluster there is no
        rotate button anywhere — the retired identity console had one, and it
        answers with an error in every environment that runs the way this one
        does. A staging or production node receives its signing key sealed in
        the environment envelope (<code>MEMQL_IDENTITY_SIGNING_KEY_B64</code>),
        so that every replica derives the SAME key and any of them can verify a
        token another minted. A key manager in that mode reports
        <code> RotationSupported() == false</code> and refuses to rotate,
        because rotating in one replica&apos;s memory would leave the other
        replicas signing with the old key and rejecting the new one. Rotation
        there is a re-seal and a rolling restart, not a click — see the
        identity-service runbook. The scheduled rotation (
        <code>MEMQL_IDENTITY_KEY_ROTATION_DAYS</code>, 90 by default) applies
        only to the on-disk key directory a single-node development cluster
        uses. Either way the rotation writes a <code>jwks_rotated</code> audit
        event and shows up in the reading above.
      </Elsewhere>
    </AdminFrame>
  );
}
