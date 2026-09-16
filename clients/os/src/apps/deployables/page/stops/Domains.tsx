import { useEffect, useState } from "react";
import { Check, Copy, Globe } from "lucide-react";

import { Button, Caption, Field, FormRow, Input, Notice, Select, Subhead } from "../../../../kit";
import { formatFreshness } from "../../../../kit/format";
import { useNow } from "../../../../kit/useNow";
import { LiveList } from "../../../../live/LiveList";
import { useLiveView } from "../../../../live/liveView";
import { useAddDomain, useRemoveDomain } from "../../domainActions";
import {
  domainSetupStep,
  DOMAIN_SETUP_STEPS,
  domainFingerprint,
  domainFromRow,
  failureSentence,
  isApex,
  isKnownFailure,
  isRecordAtFault,
  isRemovalPath,
  normalizeHostname,
  recordsFor,
  sortDomains,
  statusLabel,
  statusTone,
  type DnsRecord,
  type DomainRow,
  type PointingMethod,
} from "../../domains";
import { JourneyTrail } from "../../../../kit/JourneyTrail";
import { useDomainDNSGuidance } from "../../useDomainDNSGuidance";
import { InfoDetail } from "../../../../kit/InfoDetail";
import { useCustomDomains } from "../../useCustomDomains";
import type { SiteRow } from "../../rows";

// Domain setup stays in the Deployables page. Navigation reveals one task at
// a time; the live reconciliation feed alone determines completion.

export function DomainsContent({ site }: { site: SiteRow; domain: string }) {
  const { source: collection } = useCustomDomains();

  // PROJECT, THEN NARROW, in one pass -- the collection holds RAW wire rows,
  // so every predicate has to run on a `domainFromRow` result. The site filter
  // lives here rather than in the read: `customDomainsAll` takes no arguments,
  // so selecting another deployable changes no subscription.
  const view = useLiveView<Record<string, unknown>, DomainRow>(
    collection,
    `domains:${site.id}`,
    (rows) =>
      sortDomains(
        rows
          .map(domainFromRow)
          .filter((d) => d.id !== "" && d.siteId === site.id),
      ),
  );

  return (
    <section className="os-report-part" aria-label={`Domains for ${site.hostname || site.id}`}>
      <Subhead>Domains</Subhead>
      <Caption>
        Use your own domain for this app. Its cluster address{" "}
        <code className="os-mono">{site.hostname || "--"}</code> remains available.
      </Caption>
      {/* WHO THIS IS FOR, SAID ONCE. The concept is clusterOwner tier, so on a
          cluster where admin and cluster owner are different people an admin
          reads no bindings at all -- the filter narrows, it does not error, so
          without this line an empty list would be a false statement rather than
          an empty one. The panel deliberately does not detect which reader it
          has: it says what is true for both, and anyone who tries anyway gets
          the server's own sentence beside the control they used. */}

      {/* WHAT THE DOMAIN'S OWN STATUS DOES NOT SAY. A binding reaches `live`
          when ITS setup is finished -- both DNS records check out and the
          certificate is Ready -- and that is a fact about the domain. Whether a
          visitor gets anything is a fact about the DEPLOYABLE, decided by the
          status gate in component/edge/handler.go before any file is looked at.
          The two are independent, and a panel that showed only the first would
          say "serving" about a hostname the internet 404s.

          NAMED BY WHAT SERVES, not by listing what does not -- the same
          inversion the edge's own switch carries. `live` is the one status that
          serves, so every other value, including any added later, gets this
          notice without anybody remembering to come back for it. */}
      {site.status === "live" ? null : (
        <Notice
          tone="warn"
          sentence={`This deployable is ${site.status || "not live"}, so nothing is served at any of its domains.`}
          next="Go live to serve this app at its verified domains."
        />
      )}

      <AddDomain siteId={site.id} />

      {/* KEYED ON THE SITE ID so that changing deployable RE-BASELINES rather
          than animating. Revealing rows the browser already had is not the
          cluster sending them, and the arrival cue must only fire for news. */}
      <LiveList<DomainRow>
        key={`domains:${site.id}`}
        source={view}
        label={`Domains bound to ${site.hostname || site.id}`}
        emptyText="No custom domains. Add a hostname to get its DNS records."
        rowId={(d) => d.id}
        fingerprint={domainFingerprint}
        renderRow={(d) => <DomainCard key={d.id} domain={d} site={site} />}
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Add
// ---------------------------------------------------------------------------

function AddDomain({ siteId }: { siteId: string }) {
  const [hostname, setHostname] = useState("");
  const { busy, error, outcome, add, reset } = useAddDomain();

  const typed = normalizeHostname(hostname);
  // THE ONE THING A BROWSER CAN ANSWER AT KEYSTROKE RATE. Everything that
  // decides -- the cluster's own domain, a collision with a site or another
  // binding, the per-site maximum -- needs a read or an environment value the
  // browser does not have, so those arrive from the server and render verbatim.
  const shapeProblem =
    typed === "" || typed.includes(".") ? "" : "A domain needs at least one dot, like www.acme.com.";

  async function submit() {
    if (await add(siteId, typed)) setHostname("");
  }

  return (
    <div className="os-domain-add">
      <FormRow>
        <Input
          id="os-domain-hostname"
          label="Domain to bind"
          value={hostname}
          placeholder="www.acme.com"
          disabled={busy}
          onChange={(next) => {
            setHostname(next);
            reset();
          }}
          onEnter={() => void submit()}
        />
        <Button
          tone="primary"
          busy={busy}
          busyLabel="Adding"
          disabled={typed === "" || shapeProblem !== ""}
          onClick={() => void submit()}
        >
          <Globe size={13} aria-hidden /> Add domain
        </Button>
      </FormRow>

      {shapeProblem === "" ? null : <Caption>{shapeProblem}</Caption>}

      {outcome === null ? null : (
        <Notice
          tone="info"
          sentence={`${outcome.hostname} is bound and waiting for its DNS records.`}
          next="Add the DNS records below at your domain provider. Verification runs automatically."
        />
      )}

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="That domain was not bound."
          next="Check the hostname and the error below, then try again."
          detail={error}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// One binding
// ---------------------------------------------------------------------------

function DomainCard({ domain: d, site }: { domain: DomainRow; site: SiteRow }) {
  const now = useNow(15_000);
  const tone = d.status === "live" && site.status !== "live" ? "muted" : statusTone(d);
  const [method, setMethod] = useState<PointingMethod>("ADDRESS");
  const current = domainSetupStep(d);
  const [selected, setSelected] = useState(current);
  // Follow real progress, including regressions; inspecting earlier steps
  // never changes verification or marks a step complete.
  useEffect(() => setSelected(current), [current]);
  const labels = DOMAIN_SETUP_STEPS;
  const active = Math.min(selected, current);
  const guidance = useDomainDNSGuidance(d.id, active === 1 && !isRemovalPath(d.status));
  const records = recordsFor(d, guidance.value, method);
  const serving = d.status === "live" && site.status === "live";

  const removalPath = isRemovalPath(d.status);
  const sentence = failureSentence(d.failureReason);

  return (
    <article className="os-domain" data-tone={tone} data-status={d.status}>
      <header className="os-domain-head">
        <h5 className="os-domain-host">{d.hostname}</h5>
        <span className="os-domain-status" data-tone={tone}>
          {d.status === "live" && !serving ? "domain ready" : statusLabel(d.status)}
        </span>
        <RemoveDomain domain={d} />
      </header>

      {removalPath ? (
        <p className="os-domain-terminal">
          {d.status === "removed"
            ? "Removed. The record of this binding stays here on purpose -- what a cluster served, and when, is worth keeping."
            : "Taking the route and certificate away. The domain has already stopped being served."}
        </p>
      ) : (
        <JourneyTrail label={`Domain setup for ${d.hostname}`} selected={String(active)} onSelect={id => setSelected(Number(id))}
          steps={labels.map((label, i) => ({ id: String(i), label, state: i < current ? "done" : i === current ? (d.failureReason ? "stopped" : "current") : "ahead", available: i <= current }))} />
      )}

      {sentence === "" ? null : (
        <div className="os-domain-problem" data-known={isKnownFailure(d.failureReason)}>
          <p className="os-domain-problem-sentence">{sentence}</p>
          {/* THE SERVER'S OWN WORDS, in the data voice. What the sweep saw is
              a different fact from what is wrong, and somebody editing a zone
              file needs both -- "the token is missing" plus the value that IS
              published is the difference between a fix and a guess. */}
          {d.failureDetail === "" ? null : (
            <p className="os-domain-problem-detail os-mono">{d.failureDetail}</p>
          )}
        </div>
      )}

      {removalPath ? null : (
        <div className="deployable-journey-current">
          <div className="os-domain-head"><h3>{labels[active]}</h3>
            {active === 1 ? <InfoDetail title="How domain routing works"><p>DNS sends visitors to the cluster. The binding for {d.hostname} selects this website, and the visitor keeps your domain in the address bar. Use the displayed current addresses; update these records if the cluster’s public addresses change.</p></InfoDetail> : null}
          </div>
          {active === 0 ? <>
            <Caption>Add this TXT record at your domain provider. Verification runs automatically.</Caption>
            <RecordStrip record={records[0]!} faulty={isRecordAtFault(records[0]!, d.failureReason)} />
          </> : null}
          {active === 1 ? <>
            <Field label="DNS record type">
              <Select id={`domain-record-${d.id}`} label="DNS record type" value={method} onChange={value => setMethod(value as PointingMethod)}>
                <option value="ADDRESS">A / AAAA — standard DNS</option>
                <option value="CNAME">CNAME — subdomain or provider flattening</option>
                <option value="ALIAS">ALIAS / ANAME — if supported</option>
              </Select>
            </Field>
            {method === "CNAME" ? <Notice tone="info" sentence="A root domain needs provider support for CNAME flattening."
              next="GoDaddy does not support root CNAME records. Choose A / AAAA for a root domain there. For a subdomain, bind its full name, such as www.example.com." /> : null}
            {method === "ALIAS" ? <Caption>Use this only when your provider offers ALIAS or ANAME. Otherwise choose A / AAAA.</Caption> : null}
            {guidance.error ? <Notice tone="error" sentence="DNS targets could not be loaded." detail={guidance.error}><Button onClick={guidance.retry}>Try again</Button></Notice>
              : !guidance.value ? <Caption>Reading this cluster’s DNS targets…</Caption>
              : <>
                <Caption>{method === "ADDRESS" ? "Add the address records below at your DNS provider. Replace previous website addresses for this same name; keep your email and ownership records." : "Copy the name and target below into your DNS provider."}</Caption>
                {records.slice(1).map(record => <RecordStrip key={`${record.kind}:${record.value}`} record={record} faulty={isRecordAtFault(record, d.failureReason)} />)}
              </>}
            <Caption>{isApex(d.hostname) ? "@ means your root domain." : "If your provider adds the domain automatically, enter only the subdomain in its Name field."} The hostname stays {d.hostname}.</Caption>
          </> : null}
          {active === 2 ? <Caption>{d.failureReason === "" ? "Ownership and DNS are verified. Waiting for the HTTPS certificate and route to be ready." : "Certificate setup needs attention. The reported problem is shown above."}</Caption> : null}
          {active === 3 ? <>
            <Caption>{serving ? "DNS is verified and the HTTPS certificate is ready." : "DNS and HTTPS are ready. This deployable must be live before its domain can serve content."}</Caption>
            {serving ? <a href={`https://${d.hostname}`} target="_blank" rel="noreferrer noopener">Open {d.hostname}</a> : null}
          </> : null}
          {active < current ? <Button tone="quiet" onClick={() => setSelected(current)}>Continue to {labels[current]}</Button> : null}
        </div>
      )}

      <footer className="os-domain-foot">
        <span>
          {d.lastCheckedAt === ""
            ? "Not checked yet"
            : `Last checked ${formatFreshness(d.lastCheckedAt, now)}`}
        </span>
        {/* THE ABSENT BUTTON, EXPLAINED -- and explained by saying what DOES
            happen rather than what is missing. A control that is not there,
            with no account of why, reads as something somebody forgot to
            build; a defensive sentence about the button reads as an apology
            for it. This says the thing a person actually wants to know. */}
        {removalPath || d.status === "live" ? null : (
          <span>Checks run automatically every couple of minutes.</span>
        )}
      </footer>
    </article>
  );
}

// ---------------------------------------------------------------------------
// The record strip -- the signature of this surface
// ---------------------------------------------------------------------------

/**
 * One DNS record, in the three parts a registrar's own form asks for.
 *
 * TYPE / NAME / VALUE, separately copyable, because that is literally the shape
 * of the task: three fields, in another application, in another tab. A single
 * "copy record" button would hand somebody a line they then have to take apart.
 */
function RecordStrip({ record, faulty }: { record: DnsRecord; faulty: boolean }) {
  return (
    <div className="os-record" data-faulty={faulty}>
      <div className="os-record-parts">
        {/* TYPE IS NOT COPYABLE, and that is the point rather than an
            oversight: every registrar offers it as a dropdown, so nobody
            pastes "TXT". A copy button there would be an affordance for
            something no one does, crowding the two that matter. */}
        <div className="os-record-part">
          <span className="os-record-label">Type</span>
          <span className="os-record-kind">{record.kind}</span>
        </div>
        <RecordPart label="Name" value={record.name} grow />
        <RecordPart label="Value" value={record.value} grow />
      </div>
      <p className="os-record-purpose">{record.purpose}</p>
    </div>
  );
}

function RecordPart({ label, value, grow = false }: { label: string; value: string; grow?: boolean }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    } catch {
      // A CLIPBOARD REFUSAL IS NOT AN ERROR TO REPORT. The value is on screen
      // and selectable, so the fallback is the one somebody already has; a
      // notice here would be a message about the browser rather than about the
      // domain.
      setCopied(false);
    }
  }

  return (
    <div className="os-record-part" data-grow={grow}>
      <span className="os-record-label">{label}</span>
      <button
        type="button"
        className="os-record-value"
        onClick={() => void copy()}
        title={`Copy ${label.toLowerCase()}`}
        aria-label={`Copy ${label.toLowerCase()}: ${value}`}
      >
        <code>{value}</code>
        {copied ? <Check size={11} aria-hidden /> : <Copy size={11} aria-hidden />}
      </button>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Remove
// ---------------------------------------------------------------------------

/**
 * Remove, with the confirm IN SURFACE and naming the hostname.
 *
 * Never a browser dialog: `window.confirm` blocks the whole shell and looks
 * like a tab, which is the one thing a desktop window must not do. Cancel is a
 * no-op that leaves the row exactly as it was.
 */
function RemoveDomain({ domain: d }: { domain: DomainRow }) {
  const [confirming, setConfirming] = useState(false);
  const { busy, error, remove, reset } = useRemoveDomain();

  if (d.status === "removed" || d.status === "removing") return null;

  if (!confirming) {
    return (
      <Button
        onClick={() => {
          reset();
          setConfirming(true);
        }}
        ariaLabel={`Remove ${d.hostname}`}
      >
        Remove
      </Button>
    );
  }

  return (
    <div className="os-domain-confirm">
      <span>Stop serving {d.hostname}?</span>
      <Button
        tone="danger"
        busy={busy === d.id}
        busyLabel="Removing"
        onClick={() => {
          void remove(d.id).then((ok) => {
            if (ok) setConfirming(false);
          });
        }}
      >
        Remove
      </Button>
      <Button onClick={() => setConfirming(false)}>Keep</Button>
      {error === "" ? null : (
        <Notice tone="error" sentence="That domain was not removed." detail={error} />
      )}
    </div>
  );
}
