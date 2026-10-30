import { Fact, Facts } from "./controls";

type PriceEvidence = {
  checkedAt?: string;
  refreshAfter?: string;
  sourceUrls?: string[];
  effectiveFrom?: string | null;
  effectiveUntil?: string;
  effectiveDateReason?: string;
  inputPerMillion?: number;
  outputPerMillion?: number;
  cacheReadPerMillion?: number;
  cacheWriteShortPerMillion?: number;
  cacheWriteLongPerMillion?: number;
  cacheDurations?: string[];
  longContextAbove?: number;
  longInputMultiplier?: number;
  longOutputMultiplier?: number;
};
type TariffEvidence = PriceEvidence & {
  provider?: string;
  name?: string;
  timezone?: string;
  rateBands?: { name: string; perKWh: number; appliesWhen: string }[];
};
export type CostEstimateData = {
  kind?: string;
  status?: string;
  reason?: string;
  amount?: number | null;
  minimum?: number | null;
  maximum?: number | null;
  currency?: string;
  freshness?: string;
  servedModel?: string;
  modelMapping?: string;
  calculatedAt?: string;
  assumptions?: string[];
  quote?: PriceEvidence;
  tariff?: TariffEvidence;
  kWh?: number | null;
  energy?: { method?: string; watts?: number | null; seconds?: number; source?: string; allocation?: string };
  usage?: { inputTokens?: number; outputTokens?: number; cacheReadTokens?: number | null; cacheWriteShortTokens?: number | null; cacheWriteLongTokens?: number | null; known?: boolean; aggregate?: boolean; estimated?: boolean };
};
export function costEstimateFrom(value: unknown): CostEstimateData | undefined {
  return value && typeof value === "object" && !Array.isArray(value) ? value as CostEstimateData : undefined;
}
const finite = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);
function numberText(v: number) {
  // Rounding a small positive electricity estimate to zero conceals the cost.
  if (v > 0 && v < .000001) return "<0.000001";
  return v.toFixed(v > 0 && v < .0001 ? 6 : 4);
}
export function estimateText(e?: CostEstimateData): string {
  if (!e || e.status === "unavailable") return "Unavailable";
  const money = (v: number) => `${e.currency || "USD"} ${numberText(v)}`;
  if (finite(e.amount)) return money(e.amount);
  if (finite(e.minimum) && finite(e.maximum)) return `${money(e.minimum)}–${numberText(e.maximum)}`;
  return "Unavailable";
}
export function CostEstimate({ estimate, label = "API-equivalent estimate" }: { estimate?: CostEstimateData; label?: string }) {
  const evidence = estimate?.quote ?? estimate?.tariff;
  const stale = estimate?.freshness === "stale" || (!!evidence?.refreshAfter && Date.now() >= Date.parse(evidence.refreshAfter));
  const assumptions = Array.isArray(estimate?.assumptions) ? estimate.assumptions.filter(a => typeof a === "string") : [];
  const sources = Array.isArray(evidence?.sourceUrls) ? evidence.sourceUrls.filter(s => typeof s === "string" && s.startsWith("https://")) : [];
  const q = estimate?.quote;
  const t = estimate?.tariff;
  const u = estimate?.usage;
  const count = (value?: number | null) => u?.known && finite(value) ? value.toLocaleString("en-US") : "Unavailable";
  const price = (value?: number) => finite(value) ? `${estimate?.currency || "USD"} ${value} / 1M tokens` : "Unavailable";
  return <div className="os-cost-estimate">
    <Facts>
      <Fact label={label} value={estimateText(estimate)} />
      <Fact label="Actual billing" value="Unavailable — no billing receipt" />
      {estimate?.servedModel ? <Fact label={estimate.usage?.aggregate ? "Reported model (aggregate)" : "Served model"} value={estimate.servedModel} /> : null}
      {evidence?.checkedAt ? <Fact label="Price evidence" value={`${stale ? "Stale · " : ""}Checked ${evidence.checkedAt}`} /> : null}
    </Facts>
    {estimate?.reason ? <p>{estimate.reason}</p> : null}
    {evidence || assumptions.length || sources.length ? <details>
      <summary>Estimate assumptions and sources</summary>
      <Facts>
        {estimate?.modelMapping ? <Fact label="Model mapping" value={estimate.modelMapping} /> : null}
        {estimate?.calculatedAt ? <Fact label="Calculated" value={estimate.calculatedAt} /> : null}
        {evidence?.refreshAfter ? <Fact label="Refresh due" value={evidence.refreshAfter} /> : null}
        {evidence?.effectiveFrom ? <Fact label="Effective from" value={evidence.effectiveFrom} /> : null}
        {evidence?.effectiveUntil ? <Fact label="Effective until (exclusive)" value={evidence.effectiveUntil} /> : null}
        {q ? <>
          <Fact label="Input" value={price(q.inputPerMillion)} />
          <Fact label="Output" value={price(q.outputPerMillion)} />
          <Fact label="Cache read" value={price(q.cacheReadPerMillion)} />
          <Fact label={`Cache write (${q.cacheDurations?.[0] || "short"})`} value={price(q.cacheWriteShortPerMillion)} />
          <Fact label={`Cache write (${q.cacheDurations?.[1] || "long"})`} value={price(q.cacheWriteLongPerMillion)} />
        </> : null}
        {u ? <>
          <Fact label="Total input tokens (including cache)" value={count(u.inputTokens)} />
          <Fact label="Output tokens" value={count(u.outputTokens)} />
          <Fact label="Cache read tokens" value={count(u.cacheReadTokens)} />
          <Fact label="Short cache write tokens" value={count(u.cacheWriteShortTokens)} />
          <Fact label="Long cache write tokens" value={count(u.cacheWriteLongTokens)} />
        </> : null}
        {t?.provider ? <Fact label="Utility" value={t.provider} /> : null}
        {t?.name ? <Fact label="Tariff" value={t.name} /> : null}
        {t?.timezone ? <Fact label="Tariff timezone" value={t.timezone} /> : null}
        {finite(estimate?.kWh) ? <Fact label={estimate.energy?.method === "measured-kwh" ? "Measured energy" : "Estimated energy"} value={`${estimate.kWh} kWh`} /> : null}
        {finite(estimate?.energy?.watts) ? <Fact label="Estimated power" value={`${estimate.energy.watts} W`} /> : null}
        {finite(estimate?.energy?.seconds) ? <Fact label="Attempt duration" value={`${estimate.energy.seconds} seconds`} /> : null}
        {estimate?.energy?.source ? <Fact label="Energy source" value={estimate.energy.source} /> : null}
      </Facts>
      {q && finite(q.longContextAbove) && q.longContextAbove > 0 ? <p>For requests above {q.longContextAbove.toLocaleString("en-US")} input tokens, input and cache rates multiply by {q.longInputMultiplier}; output rates multiply by {q.longOutputMultiplier}. Aggregate totals cannot establish each request's bracket.</p> : null}
      {Array.isArray(t?.rateBands) ? t.rateBands.map((band, i) => <p key={i}>{band.name}: {band.perKWh} {estimate?.currency}/kWh · {band.appliesWhen}</p>) : null}
      {estimate?.energy?.allocation ? <p>{estimate.energy.allocation}</p> : null}
      {assumptions.length ? <ul>{assumptions.map((a, i) => <li key={i}>{a}</li>)}</ul> : null}
      {q?.effectiveFrom === null ? <p>{q.effectiveDateReason}</p> : null}
      {sources.map((source, i) => <p key={i}><a href={source} target="_blank" rel="noreferrer">Price source {i + 1}</a></p>)}
    </details> : null}
  </div>;
}
