import { useCallback, useEffect, useRef, useState } from "react";
import { Concepts, isTransportInterruption, rowString, type QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, ContentSkeleton, Details, DetailsSection, Notice, Switch, flatten } from "../../kit";
import { PageHelp } from "../../kit/pageHelp";
import { useOsConnection } from "../../live/connection";
import { useGraphRevision } from "../../live/useGraphRevision";
import { useReadEffect } from "../../live/useReadEffect";
import { usePaneActive } from "../deployables/paneActivity";

const SECURITY_VARIABLE = "MEMQL_PIPELINES_SECURITY";

/** Match pipelines.ParseSecuritySettings: malformed configuration never opts in. */
export function parsePipelinesSecurity(raw: string): boolean {
  let value: unknown;
  try { value = JSON.parse(raw); } catch { return false; }
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  // Multiple case variants are ambiguous consent. The engine rejects them
  // too, after JSON's last-value rule for an exactly repeated key.
  if (Object.keys(value).length > 1) return false;
  let enabled = false;
  for (const [key, field] of Object.entries(value)) {
    if (key.toLowerCase() !== "enabled" || (field !== null && typeof field !== "boolean")) return false;
    if (typeof field === "boolean") enabled = field;
  }
  return enabled;
}

interface StoredSecurity { enabled: boolean }
interface ReadSecurity { stored: StoredSecurity | null; fresh: boolean; error: string }
interface SaveFailure { choice: boolean; base: boolean; interrupted: boolean; detail: string }

async function readSecurity(query: QueryClient, signal?: AbortSignal): Promise<StoredSecurity> {
  const result = await query.globalVariable({ name: SECURITY_VARIABLE }, signal ? { signal } : undefined);
  const rows = result.rows();
  // The engine refuses ambiguous consent. Do not show an arbitrary row as the
  // setting, or overwrite it while another active row still has the same name.
  if (rows.length > 1) throw new Error("More than one active security setting exists. Keep one configuration and try again.");
  const raw = rows[0];
  if (raw === undefined) return { enabled: false };
  return { enabled: parsePipelinesSecurity(rowString(flatten(raw), "value")) };
}

const messageOf = (error: unknown) => error instanceof Error ? error.message : String(error);

/**
 * One installation-wide opt-in, following the existing AI settings contract.
 * The cluster's read-back is the only source of the switch's position: neither
 * a refused write nor a dropped reply is evidence that the setting changed.
 * Graph events, reconnect and pane resume reread the same saved variable.
 *
 * A save calls `pipelinesSecuritySettingsSave` with the switch alone. The
 * engine owns the row (its name, which row a save updates, the stored JSON)
 * and decides who may save: a configuration variable is written by a cluster
 * owner or by the engine for a caller it has authorized, so this is not
 * `setGlobalVariable`. A refusal is shown in the engine's own words.
 */
export function PipelinesSecuritySettings() {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const revision = useGraphRevision([Concepts.PLATFORM_GLOBAL_VARIABLE], usePaneActive());
  const [read, setRead] = useState<ReadSecurity>({ stored: null, fresh: false, error: "" });
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState<SaveFailure | null>(null);
  const newest = useRef(0);
  const inFlight = useRef(false);
  const restoreFocus = useRef(false);
  const region = useRef<HTMLElement>(null);

  const load = useCallback(async (signal?: AbortSignal): Promise<StoredSecurity | null> => {
    if (query === null) return null;
    const mine = ++newest.current;
    setRead(held => ({ ...held, fresh: false }));
    try {
      const stored = await readSecurity(query, signal);
      if (!signal?.aborted && mine === newest.current) setRead({ stored, fresh: true, error: "" });
      return stored;
    } catch (error) {
      if (!signal?.aborted && mine === newest.current) {
        setRead(held => ({ stored: held.stored, fresh: false, error: isTransportInterruption(error) ? "" : messageOf(error) }));
      }
      return null;
    }
  }, [query]);
  useReadEffect(async signal => { await load(signal); }, [load], revision);

  const current = read.stored;
  const locked = saving || !read.fresh || query === null;
  useEffect(() => {
    if (locked || !restoreFocus.current) return;
    restoreFocus.current = false;
    // Disabling a focused switch during its write can move focus to the page.
    // Return it only when the person has not moved to another control.
    if (document.activeElement !== null && document.activeElement !== document.body) return;
    region.current?.querySelector<HTMLInputElement>('input[role="switch"]')?.focus();
  }, [locked]);

  async function save(choice: boolean) {
    if (inFlight.current || locked || current === null || query === null) return;
    inFlight.current = true;
    restoreFocus.current = region.current?.contains(document.activeElement) ?? false;
    setSaving(true);
    setFailure(null);
    let failed: SaveFailure | null = null;
    try {
      await query.pipelinesSecuritySettingsSave({ enabled: choice });
    } catch (error) {
      const interrupted = isTransportInterruption(error);
      failed = { choice, base: current.enabled, interrupted, detail: interrupted ? "" : messageOf(error) };
    }
    const after = await load();
    if (failed === null && after !== null && after.enabled !== choice) {
      failed = { choice, base: current.enabled, interrupted: false, detail: "" };
    }
    setFailure(failed);
    setSaving(false);
    inFlight.current = false;
  }

  const standing = failure !== null && current !== null && current.enabled !== failure.choice ? failure : null;
  const readFailure = read.error ? (
    <Notice tone="warn" sentence="Security settings could not be read." detail={read.error}>
      <Button disabled={saving} onClick={() => void load()}>Try again</Button>
    </Notice>
  ) : null;

  return (
    <section ref={region} className="os-settings os-settings-wide deployable-settings" aria-label="Security settings" aria-busy={saving || undefined}>
      <PageHelp title="Weekly security scans">
        <p>
          This setting applies to all pipelines in this installation. It allows scheduled stages marked as security
          scans. Each repository's pipeline defines its scanners and weekly schedule. Other checks keep their existing schedule.
        </p>
        <p>
          New findings create remediation goals under the pipeline owner's account. Repeated findings reuse their
          existing goals. Turning off prevents future scan runs and new remediation goals. Runs and goals already
          in progress may finish.
        </p>
      </PageHelp>
      {current === null ? (readFailure ?? <ContentSkeleton kind="detail" label="Loading security settings" />) : (
        <Details>
          <DetailsSection title="All pipelines">
            <div className="os-app-stack">
              <div>
                <Switch checked={current.enabled} disabled={locked} onChange={choice => void save(choice)}>
                  Weekly security scans
                </Switch>
                <Caption>Run scheduled security checks once a week and create goals for new findings.</Caption>
              </div>
              {standing ? (
                <Notice tone="error" sentence={standing.interrupted ? "The change could not be confirmed." : "The change was not saved."} detail={standing.detail || undefined}>
                  {current.enabled === standing.base ? (
                    <Button disabled={locked} onClick={() => void save(standing.choice)}>Try again</Button>
                  ) : <Caption>Settings changed elsewhere. Review the current choice and try again.</Caption>}
                </Notice>
              ) : null}
              {readFailure}
            </div>
          </DetailsSection>
        </Details>
      )}
    </section>
  );
}
