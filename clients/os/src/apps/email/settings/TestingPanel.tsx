import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ContentSkeleton, Field, Notice, Select, Subhead } from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import type { LiveCollectionHandle } from "../../../live/useLiveCollection";
import { audienceFromRow, testSettingFor } from "../rows";
import { knowledgeOf, listKnowledge, weakest, type ComposedFeed } from "../useEmail";

// TESTING: the audience an organization's campaign tests are sent to.
//
// A subject of Settings for the organization the page is on. The saved choice
// is the organization's row in the app's one testing-settings feed; this panel
// reads it and writes it, and opens no subscription of its own.
//
// WHAT IT KNOWS, SAID HONESTLY. No row for an organization is a real answer
// ("none chosen") and also exactly what an unread or refused organization looks
// like, and a list that came back empty is not a list that was never read. So
// the panel asks the feeds which it is, for BOTH things it depends on -- the
// organization's saved choice and the audiences it is chosen from: still
// reading is a skeleton in the shape of the form, a refusal is the engine's
// sentence with a way to try again, and neither draws a Select to save a guess
// through. Drawn over a list it could not read, the Select would call a saved
// audience "Unavailable" and offer to clear it. A feed that is behind says so,
// over the last choice it knew.

export function TestingPanel({ accountId, audiences, testSettings }: {
  accountId: string;
  audiences: LiveCollectionHandle<Row>;
  testSettings: ComposedFeed;
}) {
  const connection = useOsConnection();
  const setting = testSettingFor(testSettings.snapshot.rows, accountId);
  const saved = setting?.audienceId ?? "";
  const [choice, setChoice] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const selected = choice ?? saved;
  const dirty = selected !== saved;

  // Once the feed delivers what was chosen it is the saved choice, and the
  // local one lets go -- so a change made somewhere else shows, not a stale pick.
  useEffect(() => {
    if (choice !== null && choice === saved) setChoice(null);
  }, [choice, saved]);

  // The choice is read from the organization's testing settings, and offered
  // from its audiences: it is only as known as the less known of the two.
  const mine = knowledgeOf(testSettings, accountId);
  const audienceList = listKnowledge(audiences);
  const knowledge = weakest(mine.knowledge, audienceList);

  const options = audiences.snapshot.rows
    .map(audienceFromRow)
    .filter((audience) => audience.accountId === accountId && audience.status === "active");

  async function save() {
    if (!connection || busy) return;
    setBusy(true);
    setError("");
    try {
      await connection.query.campaignConfigureTestAudience({
        accountId,
        audienceId: selected,
        expectedRevision: setting?.createdAt || undefined,
      });
      // The one organization that changed is read again; the rest are not.
      testSettings.refresh(accountId);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="os-app-stack" aria-label="Testing">
      <Subhead actions={knowledge === "known" && dirty ? <Button busy={busy} onClick={() => void save()}>Save</Button> : null}>Testing</Subhead>
      {knowledge === "reading" ? <ContentSkeleton kind="form" label="Loading the testing audience" /> : null}
      {mine.knowledge === "refused" ? (
        <Notice tone="error" sentence="The testing audience could not be read." detail={mine.refusal}>
          <Button onClick={() => testSettings.refresh(accountId)}>Try again</Button>
        </Notice>
      ) : null}
      {audienceList === "refused" ? (
        <Notice tone="error" sentence="The audiences could not be read." detail={audiences.snapshot.error}>
          <Button onClick={audiences.reseed}>Try again</Button>
        </Notice>
      ) : null}
      {knowledge === "unconfirmed" ? (
        <Notice tone="warn" sentence="Testing settings are not confirmed." next="Waiting for the connection to recover." />
      ) : null}
      {knowledge === "known" || knowledge === "unconfirmed" ? (
        <>
          <div className="email-field">
            <Field label="Testing audience">
              <Select id="email-testing-audience" label="Testing audience" value={selected} onChange={setChoice}>
                <option value="">None selected</option>
                {options.map((audience) => (
                  <option key={audience.id} value={audience.id}>{audience.name}</option>
                ))}
                {selected && !options.some((audience) => audience.id === selected) ? <option value={selected}>Unavailable audience</option> : null}
              </Select>
            </Field>
          </div>
          <Caption>Tests of this organization's campaigns go to this audience. Keep it separate from the audiences campaigns send to.</Caption>
        </>
      ) : null}
      {error ? <Notice tone="error" sentence="The testing audience wasn't saved." detail={error} /> : null}
    </section>
  );
}
