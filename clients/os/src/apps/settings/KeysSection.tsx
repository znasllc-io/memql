import { Button, Caption, Chip, CopyValue, Fact, Facts, Head, Notice, Panel, Row, Subhead } from "../../kit";
import { useSession } from "../../chrome/access";
import type { RoleRequirement } from "../../system/roles";
import { agreementOf, PROBE_READS, useKeyFacts } from "./keyFacts";

// Signing keys (epic memql#4984): what the identity service is publishing,
// read the way a verifier reads it.
//
// THE SECTION LEADS WITH AGREEMENT, NOT WITH A KEY TABLE, and that ordering is
// the design. A list of keys is something `curl` gives you. What `curl` does
// not give you -- and what actually breaks clusters -- is whether every
// identity replica publishes the SAME keyset: divergent keysets fail roughly
// half of all auth (memql#3400), because a token minted by one replica is
// rejected by a verifier that fetched JWKS from another. Sign-in works, then
// does not, then does, and every manifest looks correct. `make status` checks
// this from a terminal; nothing in a browser did.
//
// WHAT THE CHECK IS WORTH IS STATED, NOT IMPLIED. Disagreement is proof; four
// matching reads are evidence, because the front door chooses which replica
// answers each one. Claiming "coherent" from a single read would be worse than
// no check at all -- it is the reassuring answer, the one an operator stops
// at, and the one a broken cluster gives half the time.
//
// ROTATION IS NOT A BUTTON HERE, and its absence is the honest state rather
// than an omission: the KeyManager is in-process on the identity node, and in
// every deployed environment the key arrives sealed in the env envelope, where
// RotationSupported() is false and rotating is a re-seal plus a roll. A
// control that could only ever refuse would teach nobody that.

/** The section's role floor. Presentation only; every gate is server-side. */
export const KEYS_SECTION_ROLE: RoleRequirement = { min: "admin" };

export function KeysSection() {
  const { access, config } = useSession();
  const origin = config.identityUrl || "";
  const facts = useKeyFacts(origin, access?.clusterRole === "owner");
  const agreement = agreementOf(facts.probe);

  return (
    <div className="os-settings">
      <Head title="Keys" meta={facts.probe.keys.length === 0 ? undefined : `${facts.probe.keys.length} published`} />
      <p className="os-caption">
        The keys every verifier in this mesh checks a token against, read from{" "}
        <span className="os-mono">{origin || "an origin this deployment could not derive"}</span>{" "}
        the same unauthenticated way a verifier reads them.
      </p>

      {facts.error ? (
        <Notice tone="warn" sentence="The feed could not be read." detail={facts.error} />
      ) : (
        <Notice
          tone={agreement.tone === "diverged" ? "error" : "info"}
          sentence={agreement.sentence}
          next={
            agreement.tone === "diverged"
              ? "Roll the identity Deployment so every replica loads one keyset, then read again."
              : undefined
          }
        />
      )}

      {facts.probe.failures.length > 0 ? (
        <Notice
          tone="warn"
          sentence={`${facts.probe.failures.length} of ${PROBE_READS} reads did not answer.`}
          next="A read that failed is not a keyset that disagreed -- the agreement line above counts only the reads that came back."
          detail={facts.probe.failures[0]}
        />
      ) : null}

      {facts.probe.distinct.length > 1 ? (
        <Panel label="The keysets that came back">
          <Subhead>The keysets that came back</Subhead>
          <ul className="os-hidden-list" aria-label="Distinct keysets">
            {facts.probe.distinct.map((print, i) => (
              <li key={print}>
                <Row name={`Keyset ${i + 1}`} state={<Chip tone="muted">{print.split(" ").length} keys</Chip>}>
                  <CopyValue value={print} label="keyset" />
                </Row>
              </li>
            ))}
          </ul>
          <Caption>
            Each line is the key ids one read returned, sorted. A JWKS feed
            states no order, so sorting is what makes the comparison honest --
            two replicas holding an identical keyset can serve it in different
            orders, and an operator who chases one false alarm will not chase
            the real one.
          </Caption>
        </Panel>
      ) : null}

      <Panel label="Published keys">
        <Subhead>Published keys</Subhead>
        {facts.probe.keys.length === 0 ? (
          <Caption>
            {facts.loading ? "Reading the feed" : "The feed carried no keys."}
          </Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Published keys">
            {facts.probe.keys.map((key) => (
              <li key={key.kid}>
                <Row
                  name={<span className="os-mono">{key.kid}</span>}
                  current
                  state={
                    <>
                      <Chip tone="neutral">{key.alg || "unknown alg"}</Chip>
                      {key.use ? <Chip tone="muted">{key.use}</Chip> : null}
                    </>
                  }
                >
                  {key.kty}
                  {key.crv ? ` ${key.crv}` : ""}
                </Row>
              </li>
            ))}
          </ul>
        )}
        <Caption>
          Every key here is one a verifier will accept a signature from. More
          than one is normal during a rotation overlap; the feed itself does not
          say which is being minted with.
        </Caption>
      </Panel>

      <Panel label="Rotation">
        <Subhead>Rotation</Subhead>
        {facts.rotationWithheld ? (
          <Caption>
            The rotation history is the cluster owner&apos;s to read, so it is
            not asked for here. Nothing was found is a different answer from
            nothing was asked, and this is the second.
          </Caption>
        ) : facts.rotation === null ? (
          <Caption>
            No key rotation is recorded in the recent audit page.
          </Caption>
        ) : (
          <Facts>
            <Fact label="Last rotated" value={facts.rotation.at} mono />
            <Fact label="By" value={facts.rotation.by} />
          </Facts>
        )}
        <Caption>
          There is no rotate control, and that is the state rather than a gap:
          the key manager runs in-process on the identity node, and where the
          key arrives sealed in the environment envelope -- every deployed
          cluster -- rotating it is a re-seal and a roll rather than a button.
        </Caption>
      </Panel>

      <div className="os-refresh-row">
        <Button onClick={facts.reload} busy={facts.loading} busyLabel="Reading">
          Read again
        </Button>
        <Caption>
          {facts.fetchedAt === null
            ? `Not read yet. Each press makes ${PROBE_READS} independent reads.`
            : `Read at ${new Date(facts.fetchedAt).toISOString()}. Each press makes ${PROBE_READS} independent reads, so pressing again samples the replicas again.`}
        </Caption>
      </div>
    </div>
  );
}
