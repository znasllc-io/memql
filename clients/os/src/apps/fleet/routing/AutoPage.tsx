import { useEffect, useId, useState } from "react";

import { Button, Fact, Facts, Head, Notice } from "../../../kit";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { Details, DetailsSection, SettingRow } from "../../../kit/DetailsSection";
import { PageHelp } from "../../../kit/pageHelp";
import { useAppReach } from "../../../kit/ReadinessStates";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { jobStatus, sentence } from "./JobBoard";
import {
  AUTO_CANDIDATES,
  EDITABLE_JOBS,
  JOB_TITLES,
  routeRefusal,
  sameBalance,
  usd,
  type AutoBalance,
  type EditableJob,
  type EmbedderReading,
  type Balance,
  type Budget,
  type RouteMode,
} from "./routeModes";
import { readSource, servingIndex, type RoutingFacts } from "./sources";
import type { RouteWrites, ValueState } from "./useRouteModes";
import { STALE_SENTENCE } from "./vocabulary";

// Auto's page: how it balances, the person's budget, and what it would pick
// for each job right now.
//
// BALANCE IS ONE GROUPED EDIT (DESIGN.md, "Edits together"): three sliders,
// then one right-aligned row -- Discard and Save balance while it differs,
// Reset to recommended while the saved balance is not the recommended one.
// An owner or developer edits it; anyone else reads the same three values,
// with no controls. The recommended value is marked on each track, so "how
// far from MemQL's default" is read off the slider, not a second number.
//
// THE BUDGET IS THE PERSON'S and is edited in their Profile; this page states
// it and links there. A cluster that does not track budgets has no Budget
// section at all -- absent, never a broken one.
//
// "WHAT AUTO PICKS NOW" IS READ, NEVER GUESSED: the engine's own picks when it
// reports them, otherwise the first source that can serve now along the
// candidates Auto ranks. When the local model Auto would try first cannot
// take the call, why -- busy, out of memory -- rides along, so a pick of an
// app or a vendor is never a mystery.

/** What Auto would pick for a job now, and why not the local model when it cannot. */
export function autoPick(job: EditableJob, route: RouteMode | undefined, facts: RoutingFacts): { serves: string; reason: string; passed: string } {
  const reported = route?.jobs.find((j) => j.job === job);
  const candidates = reported && reported.sources.length > 0 ? reported.sources : AUTO_CANDIDATES[job];
  const at = servingIndex(candidates, facts);
  const serves = reported?.servesNow || (at.index >= 0 ? readSource(candidates[at.index]!, facts).serves : "");
  if (serves === "") return { serves, reason: jobStatus(candidates, facts).text, passed: "" };
  const local = readSource(AUTO_CANDIDATES[job][0]!, facts);
  const onFleet = facts.machines.some((m) => m.models.some((model) => model.modelId === serves));
  const passed = local.readiness === "idle" && local.reason !== "" && !onFleet ? `${local.label}: ${local.reason}` : "";
  return { serves, reason: "", passed };
}

const WEIGHTS: readonly { id: keyof Balance; label: string; hint: string }[] = [
  { id: "speed", label: "Speed", hint: "Faster answers" },
  { id: "cost", label: "Cost", hint: "Spending less" },
  { id: "quality", label: "Quality", hint: "Stronger models" },
];

export function AutoPage({
  route,
  facts,
  embedder,
  balance,
  budget,
  editable,
  writes,
  onBack,
}: {
  route: RouteMode | undefined;
  facts: RoutingFacts;
  embedder: EmbedderReading;
  balance: ValueState<AutoBalance>;
  budget: ValueState<Budget>;
  /** Owner or developer: may change the balance. */
  editable: boolean;
  writes: RouteWrites;
  onBack: () => void;
}) {
  const settings = useAppReach("settings");
  const profileReachable = settings.sections.includes("profile") && settings.canOpenWindows;
  const saved = balance.value;
  const [draft, setDraft] = useState<Balance | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ stale: boolean; text: string } | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const shown: Balance | null = draft ?? saved;
  const dirty = draft !== null && saved !== null && !sameBalance(draft, saved);
  const canEdit = editable && saved !== null && balance.available && writes.balanceSupported;

  // A draft that came back equal to the saved balance is no draft at all.
  useEffect(() => {
    if (draft !== null && saved !== null && sameBalance(draft, saved)) setDraft(null);
  }, [draft, saved]);

  async function write(kind: "save" | "reset") {
    if (saved === null || busy) return;
    setBusy(true);
    setError(null);
    try {
      if (kind === "save" && draft !== null) await writes.saveBalance(draft, saved.revision);
      else await writes.resetBalance(saved.revision);
      setDraft(null);
      setAnnouncement(kind === "save" ? "Balance saved." : "Balance is back to the recommended one.");
      balance.reread();
    } catch (err: unknown) {
      setError(routeRefusal(err));
      balance.reread();
    } finally {
      setBusy(false);
    }
  }

  const picks = EDITABLE_JOBS.map((job) => ({ job, ...autoPick(job, route, facts) }));

  return (
    <div className="os-deploy-pane fleet-route-page fleet-auto-page">
      <div className="os-deploy-scroll">
        <ActivityTarget target="fleet:routes:auto">
          <Head title="Auto" breadcrumbs={[{ label: "Routes", onSelect: onBack }, { label: "Auto" }]} back={{ label: "Routes", onSelect: onBack }}>
            <PageHelp title="How Auto decides">
              <p>For every call, Auto ranks the sources that could serve it by how fast and reliable they have been, what they cost and how strong they are, and takes the best one that can serve right now.</p>
              <p>Your own machines and app subscriptions cost nothing here. With a monthly budget, Auto favours free sources as you near it and stops using vendors once it is reached.</p>
            </PageHelp>
          </Head>
          <p className="fleet-route-description">{route?.description || "Picks the best balance of speed, cost and quality for every call"}</p>
          <Details>
            <DetailsSection title="Balance">
              {error === null ? null : error.stale ? (
                <Notice tone="warn" sentence={STALE_SENTENCE} next="It has been read again." />
              ) : (
                <Notice tone="error" sentence="The balance was not saved." detail={error.text} />
              )}
              {!balance.available ? (
                <p className="os-caption">This cluster does not tune Auto yet.</p>
              ) : balance.error ? (
                <Notice tone="warn" sentence="The balance could not be read." detail={balance.error} />
              ) : shown === null ? (
                <InlineSkeleton label="Loading the balance" />
              ) : (
                <>
                  {WEIGHTS.map((weight) => (
                    <SettingRow key={weight.id} label={weight.label}>
                      <WeightSlider
                        label={weight.label}
                        hint={weight.hint}
                        value={shown[weight.id]}
                        recommended={saved?.recommended[weight.id] ?? 50}
                        readOnly={!canEdit || busy}
                        onChange={(value) => setDraft({ ...shown, [weight.id]: value })}
                      />
                    </SettingRow>
                  ))}
                  {editable ? null : <p className="os-caption">An owner or developer can change the balance.</p>}
                  {canEdit && (dirty || saved?.customized) ? (
                    <div className="os-panel-actions fleet-auto-actions">
                      {dirty ? (
                        <>
                          <button type="button" className="os-link" disabled={busy} onClick={() => { setDraft(null); setError(null); }}>Discard changes</button>
                          <Button tone="primary" busy={busy} busyLabel="Saving balance…" onClick={() => void write("save")}>Save balance</Button>
                        </>
                      ) : (
                        <button type="button" className="os-link" disabled={busy} onClick={() => void write("reset")}>Reset to recommended</button>
                      )}
                    </div>
                  ) : null}
                </>
              )}
            </DetailsSection>
            {budget.available ? (
              <DetailsSection title="Budget">
                {budget.error ? (
                  <Notice tone="warn" sentence="The budget could not be read." detail={budget.error} />
                ) : budget.value === null ? (
                  <InlineSkeleton label="Loading the budget" />
                ) : (
                  <>
                    <Facts>
                      <Fact label="Monthly budget" value={budget.value.monthlyLimitUSD === null ? "No budget" : usd(budget.value.monthlyLimitUSD)} />
                      <Fact label="Spent this month" value={usd(budget.value.spentThisMonthUSD)} />
                      {budget.value.resetsAt ? <Fact label="Resets" value={resetsOn(budget.value.resetsAt)} /> : null}
                    </Facts>
                    {budget.value.state === "over" ? (
                      <p className="os-caption fleet-auto-budget" data-state="over">Budget reached. Auto uses only your machines and apps until it resets.</p>
                    ) : budget.value.state === "near" ? (
                      <p className="os-caption fleet-auto-budget">Near your budget. Auto favours your machines and apps.</p>
                    ) : null}
                    {profileReachable ? (
                      <div className="os-panel-actions fleet-auto-actions">
                        <button type="button" className="os-link" onClick={() => settings.open("profile")}>Change in your profile</button>
                      </div>
                    ) : null}
                  </>
                )}
              </DetailsSection>
            ) : null}
            <DetailsSection title="What Auto picks now">
              <Facts>
                {picks.map((pick) => (
                  <Fact
                    key={pick.job}
                    label={JOB_TITLES[pick.job]}
                    value={
                      pick.serves !== "" ? (
                        <>
                          <span className="fleet-auto-pick" data-ready>{pick.serves}</span>
                          {pick.passed === "" ? null : <span className="fleet-auto-passed">{pick.passed}</span>}
                        </>
                      ) : pick.reason === "" ? (
                        <InlineSkeleton label="Checking" />
                      ) : (
                        <span className="fleet-auto-pick">{pick.reason}</span>
                      )
                    }
                  />
                ))}
                <Fact
                  label={JOB_TITLES.embeddings}
                  value={
                    embedder.state === "loading" ? (
                      <InlineSkeleton label="Checking" />
                    ) : (
                      <span className="fleet-auto-pick" data-ready={embedder.state === "bound" || undefined}>
                        {embedder.state === "none" ? "No embedding model yet" : embedder.state === "unread" ? "Not read" : embedder.name}
                      </span>
                    )
                  }
                />
              </Facts>
            </DetailsSection>
          </Details>
          <p role="status" className="os-sr-only">{announcement}</p>
        </ActivityTarget>
      </div>
    </div>
  );
}

function resetsOn(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  return at.toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
}

/**
 * One weight: a 0..100 track with MemQL's recommended value marked on it.
 *
 * The mark is drawn, not announced twice: the slider's accessible value says
 * "60, recommended 50", and the tick under the track says the same to the eye.
 */
export function WeightSlider({
  label,
  hint,
  value,
  recommended,
  readOnly,
  onChange,
}: {
  label: string;
  hint: string;
  value: number;
  recommended: number;
  readOnly: boolean;
  onChange: (value: number) => void;
}) {
  const id = useId();
  return (
    <div className="fleet-weight" data-read-only={readOnly || undefined}>
      <div className="fleet-weight-track">
        <input
          id={id}
          className="fleet-weight-input"
          type="range"
          min={0}
          max={100}
          step={5}
          value={value}
          disabled={readOnly}
          aria-label={label}
          aria-valuetext={`${value}${value === recommended ? ", recommended" : `, recommended ${recommended}`}`}
          style={{ ["--fleet-weight-fill" as string]: `${value}%` }}
          onChange={(event) => onChange(Number(event.target.value))}
        />
        <span className="fleet-weight-mark" style={{ left: `${recommended}%` }} aria-hidden title={`Recommended: ${recommended}`} />
      </div>
      <span className="fleet-weight-value">{value}</span>
      <span className="fleet-weight-hint">{sentence(hint)}</span>
    </div>
  );
}
