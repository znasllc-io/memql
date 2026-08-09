import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { PROPORTION_BAR_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { useConcepts } from "../cluster/useConcepts";
import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Elsewhere, Reading, Refused } from "./AdminLayout";
import { adminPath, surfaceById } from "./urls";
import {
  AUDIT_CATEGORIES,
  lastRotation,
  useAdminAccess,
  useAuditTrail,
  useClusterSettings,
  usePeople,
  useSigningKeys,
} from "./useAdminConsole";

const PERSON_CONCEPT_ID = "v1:identity:user";
const AUDIT_CONCEPT_ID = "v1:identity:auditEvent";

// Cluster overview -- what the server-rendered /admin/ dashboard was, plus the
// two things it could not show.
//
// THE FOUR READINGS ARE FOUR DIFFERENT READS, and they are rendered as
// readings rather than as a stat strip for exactly that reason: view-kit's
// stat tiles total measures over ONE row set, and fabricating a row array to
// carry a key id and a registration mode would claim a population that does
// not exist. The rail and the table below ARE row sets, and they go through
// the element library like everything else in this product.
//
// WHAT IT ADDS OVER THE PAGE IT REPLACES: the dashboard reported the signing
// key's age from the KeyManager's in-process clock. This reports the ROTATION
// ITSELF -- the instant, and whether a person or the schedule did it -- which
// is an audit row an operator can open, and it stays true across replicas.
export function OverviewPage(): ReactNode {
  const surface = surfaceById("");
  const { role, canAdminister, resolved } = useAdminAccess();
  const people = usePeople(canAdminister);
  const settings = useClusterSettings(canAdminister);
  const keys = useSigningKeys();

  // Two audit reads with different jobs. The strip is whatever the operator
  // filtered to; the rotation is always read from `configuration`, because a
  // rotation the operator filtered away has not stopped being the answer to
  // "when did this key change".
  const [category, setCategory] = useState("");
  const activity = useAuditTrail(canAdminister, category);
  const rotations = useAuditTrail(canAdminister, "configuration");
  const rotated = lastRotation(rotations.data);

  const { concepts } = useConcepts();
  const personConcept = concepts.find((c) => c.id === PERSON_CONCEPT_ID);
  const auditConcept = concepts.find((c) => c.id === AUDIT_CONCEPT_ID);

  if (surface === undefined) return null;
  if (!canAdminister) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} />
      </AdminFrame>
    );
  }

  const signingKey = keys.keys[0];

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={
        <MetaButton
          onClick={() => {
            people.reload();
            activity.reload();
            rotations.reload();
            settings.reload();
            keys.reload();
          }}
        >
          Refresh
        </MetaButton>
      }
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="People who can sign in"
            value={people.loading ? "…" : String(people.data.length)}
            sub={people.error === "" ? "active and deactivated" : "could not read"}
          />
          <Reading
            label="Registration"
            value={
              settings.loading
                ? "…"
                : ((settings.data?.["registrationMode"] as string | undefined) ?? "unknown")
            }
            sub="how a new person gets an account"
          />
          <Reading
            label="Signing key"
            value={keys.loading ? "…" : (signingKey?.kid ?? "none published")}
            sub={
              keys.keys.length > 1
                ? `${keys.keys.length} keys published, overlap window open`
                : "1 key published"
            }
          />
          <Reading
            label="Last rotation"
            value={rotated === null ? "none recorded" : rotated.at}
            sub={rotated === null ? "in the events read so far" : `by ${rotated.by}`}
          />
        </div>
        {people.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read the people list: {people.error}</ErrorMessage>
          </div>
        )}
      </Band>

      <Band title="By role" meta="Cluster-wide role, not per-partition grants">
        {personConcept === undefined ? (
          <p className="text-sm text-subtle">
            This node does not publish the user concept, so the role split cannot
            be drawn.
          </p>
        ) : (
          <ViewElement
            element={PROPORTION_BAR_ELEMENT}
            rows={people.data}
            concept={personConcept}
            options={{ bindings: { category: "role", value: [] } }}
          />
        )}
      </Band>

      <Band
        title="Recent activity"
        meta={
          <span className="flex items-center gap-2">
            <label htmlFor="audit-category" className="sr-only">
              Category
            </label>
            <select
              id="audit-category"
              value={category}
              onChange={(event) => setCategory(event.target.value)}
              className="rounded border border-line bg-surface px-2 py-1 text-xs text-fg"
            >
              <option value="">every category</option>
              {AUDIT_CATEGORIES.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
            <Link to="/views/audit" className="text-xs text-muted hover:text-fg hover:underline">
              Open the audit view
            </Link>
          </span>
        }
        panel
      >
        {activity.error !== "" ? (
          <ErrorMessage>Could not read the audit trail: {activity.error}</ErrorMessage>
        ) : auditConcept === undefined ? (
          <p className="p-3 text-sm text-subtle">
            This node does not publish the audit-event concept.
          </p>
        ) : activity.data.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {activity.loading
              ? "Reading the trail…"
              : category === ""
                ? "Nothing has been recorded yet."
                : `Nothing recorded under ${category}.`}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={activity.data}
            concept={auditConcept}
            // Named rather than left to the generic scan: an audit row carries
            // a dozen fields and the six that matter during an incident are
            // when, what, who, to what, how it ended, and from where.
            options={{
              bindings: {
                column: [
                  "occurredAt",
                  "category",
                  "action",
                  "actorEmail",
                  "targetId",
                  "outcome",
                  "failureReason",
                  "sourceIP",
                ],
              },
              sort: { field: "occurredAt", direction: "desc" },
            }}
          />
        )}
      </Band>

      <Elsewhere what="Where a person gets changed">
        Editing a profile, changing someone&apos;s role and suspending an
        account are one screen along, under{" "}
        <Link to={adminPath("people")} className="underline hover:text-fg">
          People
        </Link>
        . They are kept off this page deliberately: an overview is where an
        operator looks to find out what is going on, and a control that changes
        who can do what does not belong on a page nobody arrived at intending to
        act. The whole population, with the sessions open in their name, is in
        the{" "}
        <Link to="/views/people" className="underline hover:text-fg">
          People view
        </Link>
        , and every one of those writes is audited — see the activity table
        above.
      </Elsewhere>

      <p className="text-xs text-subtle">
        Sessions and tokens live under{" "}
        <Link to={adminPath("tokens")} className="underline hover:text-fg">
          Tokens
        </Link>
        ; the published keys under{" "}
        <Link to={adminPath("keys")} className="underline hover:text-fg">
          Signing keys
        </Link>
        .
      </p>
    </AdminFrame>
  );
}
