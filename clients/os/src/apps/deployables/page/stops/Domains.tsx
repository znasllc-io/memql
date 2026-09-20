import { useCallback, useEffect, useState, useSyncExternalStore, type ReactNode } from "react";
import { Check, Copy, Globe } from "lucide-react";

import { Button, Caption, Field, FormRow, Input, Notice, Select, Subhead } from "../../../../kit";
import { AddButton } from "../../../../kit/AddButton";
import { RecordList, RecordRow } from "../../../../kit/RecordRow";
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
import { liveUrlFor, type SiteRow } from "../../rows";
import { siteStateWord } from "../../words";

// Domain setup stays in the Deployables page. Navigation reveals one task at
// a time; the live reconciliation feed alone determines completion.

/** The bindings on ONE deployable, projected and narrowed in one pass. */
function useSiteDomains(site: SiteRow) {
  const { source: collection } = useCustomDomains();
  // PROJECT, THEN NARROW, in one pass -- the collection holds RAW wire rows,
  // so every predicate has to run on a `domainFromRow` result. The site filter
  // lives here rather than in the read: `customDomainsAll` takes no arguments,
  // so selecting another deployable changes no subscription.
  return useLiveView<Record<string, unknown>, DomainRow>(
    collection,
    `domains:${site.id}`,
    (rows) =>
      sortDomains(
        rows
          .map(domainFromRow)
          .filter((d) => d.id !== "" && d.siteId === site.id),
      ),
  );
}

/**
 * THE DOMAINS OF A DEPLOYABLE, AS A LIST -- every name it answers on.
 *
 * It used to be two different things. The address the deployable actually
 * answers at was a bare underlined link at the top of the stop, and "Domains"
 * beneath it listed only the CUSTOM ones -- so a seeded deployable, which has
 * an address and can have no custom domain, read "No custom domains" with its
 * own domain sitting a few lines above. And each binding drew itself as a whole
 * card in the list: one domain filled the panel, and the list and its detail
 * shared a scroll column (DESIGN.md rule 11).
 *
 * It is one list now, on the kit's `RecordRow` -- the row the Deployables and
 * Machines lists draw. The cluster address is ALWAYS its first row, because a
 * deployable always has one; a custom domain is a row that OPENS its setup.
 *
 * A SEEDED deployable (MemQL OS, the VS Code site) shows its built-in address
 * FIXED -- that row is the site's own, re-seeded at every boot -- and still
 * offers Add, because a binding is a separate record the server accepts for a
 * seeded site like any other. `editable` is about the reader, not the site:
 * without the `domains` part the Add control is ABSENT rather than disabled,
 * which is rule 12's grammar.
 */
export function DomainsContent({ site, bindings, editable, onOpenDomain, onAdd, addressFacts }: {
  site: SiteRow;
  domain: string;
  /** Quiet facts for the cluster address row -- who the deployable is for. */
  addressFacts?: ReactNode;
  /** Does this reader hold the `domains` part? Only then is the bindings feed
   *  mounted at all: the concept is clusterOwner tier, and subscribing for
   *  somebody who may not read it is a refusal waiting to be rendered. */
  bindings: boolean;
  /** May this reader add a binding? Seeded or not makes no difference. */
  editable: boolean;
  onOpenDomain: (domainId: string) => void;
  onAdd: () => void;
}) {
  const url = liveUrlFor(site.hostname);
  const serving = site.status === "live";

  return (
    <section className="os-record-section" aria-label={`Domains for ${site.hostname || site.id}`}>
      <div className="os-record-heading">
        <Subhead>Domains</Subhead>
        {editable ? <AddButton label="Add a domain" onClick={onAdd} /> : null}
      </div>

      <NotServing site={site} />

      <RecordList label={`Domains of ${site.hostname || site.id}`}>
        {/* THE CLUSTER ADDRESS, first and always. It is not a binding and
            nothing can remove it, so it is a plain line rather than a row that
            opens: the address is CONTENT, and it is the link -- the one thing
            on the page whose text is the thing it opens. */}
        <RecordRow
          icon={<Globe size={18} aria-hidden />}
          name={url === "" ? <code className="os-mono">{site.hostname || "--"}</code> : <a className="os-mono" href={url} target="_blank" rel="noreferrer noopener">{site.hostname}</a>}
          secondary={site.systemOwned ? "Cluster address \u00b7 built in" : "Cluster address"}
          state={siteStateWord(site)}
          tone={serving ? "accent" : "muted"}
          current={serving}
        >
          {addressFacts}
        </RecordRow>
        {bindings ? <BoundDomains site={site} onOpenDomain={onOpenDomain} /> : null}
      </RecordList>

      {/* SAID ONCE (DESIGN.md rule 7), and only what is true for this reader:
          what is fixed, and what they may do about it. */}
      {site.systemOwned || editable ? (
        <Caption>
          {site.systemOwned
            ? editable
              ? "This address is built into the cluster and cannot be changed. You can add your own domains alongside it."
              : "This address is built into the cluster and cannot be changed."
            : "Add your own domain for this app. Its cluster address stays available alongside."}
        </Caption>
      ) : null}
    </section>
  );
}

/**
 * WHAT THE DOMAIN'S OWN STATUS DOES NOT SAY. A binding reaches `live` when ITS
 * setup is finished -- both DNS records check out and the certificate is Ready
 * -- and that is a fact about the domain. Whether a visitor gets anything is a
 * fact about the DEPLOYABLE, decided by the status gate in
 * component/edge/handler.go before any file is looked at. The two are
 * independent, and a surface that showed only the first would say "serving"
 * about a hostname the internet 404s.
 *
 * NAMED BY WHAT SERVES, not by listing what does not -- the same inversion the
 * edge's own switch carries. `live` is the one status that serves, so every
 * other value, including any added later, gets this notice without anybody
 * remembering to come back for it.
 *
 * SAID ON THE LIST AND ON A BINDING'S OWN PAGE. The page is where somebody
 * reads "domain ready", and that is exactly where it must not be mistaken for
 * "serving".
 */
function NotServing({ site }: { site: SiteRow }) {
  if (site.status === "live") return null;
  return (
    <Notice
      tone="warn"
      sentence={`This deployable is ${site.status || "not live"}, so nothing is served at any of its domains.`}
      next="Go live to serve this app at its verified domains."
    />
  );
}

/** The custom bindings, mounted only for a reader who may read them. */
function BoundDomains({ site, onOpenDomain }: { site: SiteRow; onOpenDomain: (domainId: string) => void }) {
  const view = useSiteDomains(site);
  // KEYED ON THE SITE ID so that changing deployable RE-BASELINES rather than
  // animating. Revealing rows the browser already had is not the cluster
  // sending them, and the arrival cue must only fire for news.
  return (
    <LiveList<DomainRow>
      key={`domains:${site.id}`}
      source={view}
      label={`Domains bound to ${site.hostname || site.id}`}
      emptyText=""
      rowId={(d) => d.id}
      fingerprint={domainFingerprint}
      renderRow={(d, tick) => <DomainListRow key={d.id} domain={d} site={site} changed={tick !== null} onOpen={() => onOpenDomain(d.id)} />}
    />
  );
}

/** One binding, as a row. The setup it opens is `DomainDetail`. */
function DomainListRow({ domain: d, site, changed, onOpen }: { domain: DomainRow; site: SiteRow; changed: boolean; onOpen: () => void }) {
  const step = domainSetupStep(d);
  const total = DOMAIN_SETUP_STEPS.length;
  // A domain that finished its own setup still serves nothing on a deployable
  // that is not live -- the same independence the notice above explains.
  const serving = d.status === "live" && site.status === "live";
  const where = isRemovalPath(d.status)
    ? "Being removed"
    : d.status === "live"
      ? "Custom domain \u00b7 set up"
      : `Custom domain \u00b7 step ${Math.min(step + 1, total)} of ${total}, ${DOMAIN_SETUP_STEPS[Math.min(step, total - 1)]}`;
  return (
    <RecordRow
      icon={<Globe size={18} aria-hidden />}
      name={d.hostname}
      secondary={where}
      state={statusLabel(d.status)}
      tone={serving ? "accent" : "muted"}
      stateExtra={changed ? <span className="os-livelist-tick">updated</span> : null}
      current={serving}
      dim={isRemovalPath(d.status)}
      label={`Open ${d.hostname}, ${statusLabel(d.status)}`}
      onOpen={onOpen}
    />
  );
}

/**
 * ONE BINDING'S SETUP, as the detail its row opens.
 *
 * The card is unchanged -- the stepped rail, the records to create, what the
 * sweep last saw, removal -- it simply has a page of its own now instead of
 * being a row in the list it was chosen from.
 */
export function DomainDetail({ site, domainId }: { site: SiteRow; domainId: string }) {
  const view = useSiteDomains(site);
  // A SUBSCRIPTION, not a read of `view.snapshot` in render: the binding's
  // status changes under the person watching it, and a plain read would show
  // the step it was on when the page opened.
  const subscribe = useCallback((listener: () => void) => view?.subscribe(listener) ?? (() => {}), [view]);
  const snapshot = useSyncExternalStore(subscribe, () => view?.snapshot ?? null, () => null);
  const found = (snapshot?.rows ?? []).find((d) => d.id === domainId);
  if (found) return <><NotServing site={site} /><DomainCard domain={found} site={site} /></>;
  // Absent while the feed is still arriving is "not read yet", not "removed".
  if (snapshot === null || snapshot.state !== "live") return <Caption>Reading this domain...</Caption>;
  return <Notice tone="warn" sentence="This domain is not bound to this deployable any more." next="It may have been removed since this page opened." />;
}

/** Adding a binding, as the page the list's Add control opens. */
export function AddDomainView({ siteId }: { siteId: string }) {
  return (
    <>
      <Caption>Bind a domain you own. You will be given the DNS records to create, and the cluster checks them on its own every couple of minutes.</Caption>
      <AddDomain siteId={siteId} />
    </>
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
