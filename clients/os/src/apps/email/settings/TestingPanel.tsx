import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ContentSkeleton, Field, Notice, Select, Subhead } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import type { LiveCollectionHandle } from "../../../live/useLiveCollection";
import { audienceFromRow } from "../rows";
import { knowledgeOf, listKnowledge, weakest, type ComposedFeed } from "../useEmail";

// TESTING: the audience an organization's campaign tests are sent to.
//
// A subject of Settings for the organization the page is on. The saved choice
// is the organization's row in the app's one testing-settings feed; this panel
// reads it and writes it, and opens no subscription of its own.
//
// WHAT IT KNOWS, SAID HONESTLY. No row for an organization is a real answer
// ("none chosen") and also exactly what an unread or refused organization looks
// like, so the panel asks the feed which it is: still reading is a skeleton in
// the shape of the form, a refusal is the engine's sentence with a way to try
// again, and neither draws a Select to save a guess through. A feed that is
// behind says so, over the last choice it knew.

const text = (row: Record<string, unknown> | null, key: string) => (typeof row?.[key] === "string" ? (row[key] as string) : "");

export function TestingPanel({ accountId, audiences, testSettings }: {
  accountId: string;
  audiences: LiveCollectionHandle<Row>;
  testSettings: ComposedFeed;
}) {
  const connection = useOsConnection();
  const row = testSettings.snapshot.rows.map(flatten).find((candidate) => text(candidate, "accountId") === accountId) ?? null;
  const saved = text(row, "audienceId");
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
  const knowledge = weakest(mine.knowledge, listKnowledge(audiences));

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
        expectedRevision: text(row, "createdAt") || undefined,
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
      {knowledge === "refused" ? (
        <Notice tone="error" sentence="The testing audience could not be read." detail={mine.refusal}>
          <Button onClick={() => testSettings.refresh(accountId)}>Try again</Button>
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
