import { useCallback, useEffect, useState, useSyncExternalStore, type ReactNode } from "react";
import { Check, Copy, Globe } from "lucide-react";

import { Button, Caption, Field, Input, Notice, Select, Subhead, type Stop } from "../../../../kit";
import type { Act } from "../../../../kit/ActionBar";
import type { Breadcrumb } from "../../../../kit/Breadcrumbs";
import { Wizard, type WizardStatus } from "../../../../kit/Wizard";
import { AddButton } from "../../../../kit/AddButton";
import { RecordList, RecordRow } from "../../../../kit/RecordRow";
import { formatFreshness } from "../../../../kit/format";
import { useNow } from "../../../../kit/useNow";
import { LiveList } from "../../../../live/LiveList";
import { useLiveView } from "../../../../live/liveView";
import { useAddDomain, useRemoveDomain } from "../../domainActions";
import {
  domainSetupReading,
  domainSetupStep,
  domainSteps,
  DOMAIN_SETUP_STEPS,
  domainFingerprint,
  domainFromRow,
  failureSentence,
  isApex,
  isKnownFailure,
  isListedDomain,
  isRecordAtFault,
  isRemovalPath,
  isWaitingReason,
  normalizeHostname,
  recordsFor,
  sortDomains,
  statusLabel,
  type DnsRecord,
  type DomainRow,
  type DomainStepId,
  type PointingMethod,
} from "../../domains";
import { useDomainDNSGuidance } from "../../useDomainDNSGuidance";
import { InfoDetail } from "../../../../kit/InfoDetail";
import { useCustomDomains } from "../../useCustomDomains";
import { liveUrlFor, type SiteRow } from "../../rows";
import { siteStateWord } from "../../words";

// Domain setup stays in the Deployables page. Navigation reveals one task at
// a time; the live reconciliation feed alone determines completion.

/**
 * The bindings on ONE deployable, projected and narrowed in one pass.
 *
 * `listed` is the LIST's reading (`isListedDomain`): what answers or is on its
 * way to. The wizard reads them all, because it follows one binding through
 * whatever happens to it, including its removal.
 */
function useSiteDomains(site: SiteRow, listed = false) {
  const { source: collection } = useCustomDomains();
  // PROJECT, THEN NARROW, in one pass -- the collection holds RAW wire rows,
  // so every predicate has to run on a `domainFromRow` result. The site filter
  // lives here rather than in the read: `customDomainsAll` takes no arguments,
  // so selecting another deployable changes no subscription.
  return useLiveView<Record<string, unknown>, DomainRow>(
    collection,
    `domains:${site.id}:${listed ? "listed" : "all"}`,
    (rows) =>
      sortDomains(
        rows
          .map(domainFromRow)
          .filter((d) => d.id !== "" && d.siteId === site.id && (!listed || isListedDomain(d))),
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
  // THE LIST'S READING: a cancelled or removed domain is not in it.
  const view = useSiteDomains(site, true);
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
    // The only removal that is still LISTED is one the cluster could not
    // finish (`isListedDomain`), so that is what the line says.
    ? "Could not be removed yet \u00b7 its name is still held"
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

// ---------------------------------------------------------------------------
// The wizard -- adding a domain, and coming back to one that is not finished
// ---------------------------------------------------------------------------

/** The four setup stages, in the order the rail draws them. */
const SETUP_STEPS: readonly DomainStepId[] = ["ownership", "dns", "certificate", "serving"];
const SETUP_NAMES: Record<DomainStepId, string> = { ownership: "Ownership", dns: "DNS", certificate: "Certificate", serving: "Serving" };

/**
 * A DOMAIN, from its name to the moment it serves -- and back again.
 *
 * It used to be two pages that did not know about each other: a text field
 * with its own button, and -- after going back to the list and opening the row
 * that had appeared -- a card with a horizontal stepper and the records to
 * create. The first version of this wizard fixed the first half and left the
 * second, and the owner found the seam within a minute of binding a real
 * domain: Leave, click the row, and the thing they had just been using had
 * turned into something else. "It should just take me back to the wizard
 * where we left off."
 *
 * So there is ONE surface. With no `domainId` it opens on the name; with one
 * it opens on that binding, at whatever stage the cluster has walked it to --
 * which is exactly where a person who pressed Leave was. Nothing here decides
 * that a stage passed: the reconciliation feed does.
 *
 * THE SERVER'S ANSWER STANDS IN UNTIL THE ROW ARRIVES. `customDomainAdd`
 * returns the id, the hostname and the token; the row itself comes on its own
 * broadcast a beat later. Drawing the ownership record from the answer means
 * the record is on screen the instant the bind returns, rather than after a
 * "reading..." nobody can do anything with.
 */
export function DomainWizard({ site, name, domainId = "", trail, back, onLeave }: {
  site: SiteRow;
  /** What the deployable is CALLED, as its own page titles it -- not its hostname. */
  name: string;
  /** The binding to come back to. Empty is a new one. */
  domainId?: string;
  /** The ancestors of this page; the wizard names itself. */
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
  /** Back to the list this was opened from. */
  onLeave: () => void;
}) {
  const [hostname, setHostname] = useState("");
  const { busy, error, outcome, add, reset } = useAddDomain();
  const removal = useRemoveDomain();
  const [confirming, setConfirming] = useState(false);
  const now = useNow(15_000);

  const view = useSiteDomains(site);
  const subscribe = useCallback((listener: () => void) => view?.subscribe(listener) ?? (() => {}), [view]);
  const snapshot = useSyncExternalStore(subscribe, () => view?.snapshot ?? null, () => null);
  const rows = snapshot?.rows ?? [];
  const settled = snapshot !== null && snapshot.state === "live";

  const boundId = outcome?.domainId ?? "";
  // FOUND BY ITS NAME, NOT ONLY BY ITS ID. The add's reply and the graph row
  // are two egress seams and they do not promise to spell an id alike -- the
  // Fleet's add flow meets the same seam with its mint reply. Bound to a real
  // domain, this wizard sat on "not checked yet" through two sweeps while the
  // list behind it read "checking DNS": it was looking for an id the feed
  // never sent, and went on drawing its own stand-in.
  //
  // A hostname is this concept's natural key: the server refuses a second
  // live binding of one, so among THIS deployable's rows that are not on the
  // way out there is at most one. A removed binding of the same name is
  // history and must never be mistaken for the one just made. Nothing here
  // composes or parses an id.
  const boundHost = outcome === null ? "" : normalizeHostname(outcome.hostname);
  const arrived = domainId !== ""
    // Come back to: the row the list opened, whatever state it is in.
    ? rows.find((d) => d.id === domainId)
    : boundId === "" ? undefined : rows.find(
        (d) => !isRemovalPath(d.status) && (d.id === boundId || (boundHost !== "" && normalizeHostname(d.hostname) === boundHost)),
      );
  const bound: DomainRow | null = arrived ?? (outcome === null || boundId === ""
    ? null
    : {
        id: boundId, siteId: site.id, hostname: outcome.hostname, accountId: "", token: outcome.token,
        status: "pending_dns", failureReason: "", failureDetail: "", lastCheckedAt: "", verifiedAt: "",
        issuedAt: "", removedAt: "", createdAt: "",
      });

  const typed = normalizeHostname(hostname);
  // THE ONE THING A BROWSER CAN ANSWER AT KEYSTROKE RATE. Everything that
  // decides -- the cluster's own domain, a collision with a site or another
  // binding, the per-site maximum -- needs a read or an environment value the
  // browser does not have, so those arrive from the server and render verbatim.
  const wellFormed = typed !== "" && typed.includes(".");
  const siteLive = site.status === "live";
  const leaving = bound !== null && isRemovalPath(bound.status);
  const finished = bound !== null && bound.status === "live";
  const setup = bound === null || leaving ? null : domainSteps(bound, siteLive);

  // THE RAIL OPENS WHERE THE FLOW IS, and a person may open any reachable step
  // to read it again. Their choice is forgotten the moment the flow moves on.
  const computed = setup === null
    ? "domain"
    : setup.find((step) => step.state === "current" || step.state === "stopped" || step.state === "open")?.id ?? "serving";
  const [chosen, setChosen] = useState<string | null>(null);
  useEffect(() => setChosen(null), [computed]);
  const open = chosen ?? computed;

  // WHAT IT IS CALLED. While a domain is being added this is the wizard that
  // adds it, and coming back to one lands on the same words it was left on.
  // Once it serves, or is on its way out, the adding is over and the page is
  // the domain's own.
  const title = bound !== null && (finished || leaving) ? bound.hostname : "Add a domain";
  const lead = bound === null || !(finished || leaving)
    ? `Serve ${name} at a domain you own.`
    : leaving ? `A domain of ${name}, removed.` : `A domain of ${name}.`;

  // COMING BACK, BEFORE THE FEED HAS SAID. Absent while the rows are still
  // arriving is "not read yet", not "removed".
  const reading = domainId !== "" && bound === null && !settled;
  const missing = domainId !== "" && bound === null && settled;

  const steps: Stop[] = reading || missing || leaving ? [] : [
    {
      id: "domain",
      name: "Domain",
      state: bound === null ? "open" : "done",
      answer: bound === null ? "" : bound.hostname,
      // Bound, the name is a fact: there is nothing behind the line to open.
      openable: bound === null,
      body: bound !== null ? undefined : (
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
          onEnter={() => {
            if (wellFormed) void add(site.id, typed);
          }}
        />
      ),
    },
    ...SETUP_STEPS.map((id): Stop => {
      const reading = setup?.find((step) => step.id === id);
      if (bound === null || reading === undefined) return { id, name: SETUP_NAMES[id], state: "ahead" };
      const reachable = reading.state !== "ahead";
      return {
        id,
        name: reading.name,
        state: reading.state,
        answer: reading.answer,
        openable: reachable,
        body: reachable ? <DomainStepBody domain={bound} site={site} step={id} /> : undefined,
      };
    }),
  ];

  const removable = bound !== null && arrived !== undefined && !leaving;

  return (
    <Wizard
      className="os-domain-page"
      icon={<Globe aria-hidden />}
      title={title}
      lead={lead}
      breadcrumbs={[...trail, { label: title }]}
      back={back}
      /* ONE NAME FOR AS LONG AS IT IS BEING ADDED. A list that renamed itself
         the moment the bind returned would be announced as a different list. */
      label={bound !== null && (finished || leaving) ? `Setup of ${bound.hostname}` : "Adding a domain"}
      steps={steps}
      open={open}
      onOpen={setChosen}
      status={statusFor()}
      acts={actsFor()}
      confirm={!confirming || bound === null ? null : (
        <div className="os-actbar-confirm">
          {/* NAMED, and said in the words of what it costs: a domain that is
              serving stops; one still being set up simply stops being set up. */}
          {finished ? `Stop serving ${bound.hostname}?` : `Cancel adding ${bound.hostname}? It is removed, and its setup stops here.`}
        </div>
      )}
      context={{ page: "Deployable", siteId: site.id, hostname: site.hostname, view: bound === null ? "Add a domain" : "Domain setup", domain: bound?.hostname, step: open }}
      notices={
        <>
          {bound === null || leaving ? null : <NotServing site={site} />}
          {error === "" ? null : (
            <Notice
              tone="error"
              sentence="That domain was not added."
              next="Nothing was written. The cluster's own reason is below."
              detail={error}
            />
          )}
          {removal.error === "" ? null : <Notice tone="error" sentence="That domain was not removed." detail={removal.error} />}
          {missing ? <Notice tone="warn" sentence="This domain is not bound to this deployable any more." next="It may have been removed since this page opened." /> : null}
          {leaving && bound !== null ? (
            <p className="os-domain-terminal">
              {bound.status === "removed"
                ? "Removed. The record of this binding stays here on purpose -- what a cluster served, and when, is worth keeping."
                : "Taking the route and certificate away. The domain has already stopped being served."}
            </p>
          ) : null}
          {/* A REMOVAL THE CLUSTER COULD NOT FINISH is the one removal a person is
              shown, and what it reported is why: its name stays held until it
              is `removed`, and the sweep keeps trying on its own. */}
          {leaving && bound !== null && bound.status === "removing" && bound.failureReason !== "" ? (
            <div className="os-domain-problem" data-known={isKnownFailure(bound.failureReason)}>
              <p className="os-domain-problem-sentence">{failureSentence(bound.failureReason)}</p>
              {bound.failureDetail === "" ? null : <p className="os-domain-problem-detail os-mono">{bound.failureDetail}</p>}
            </div>
          ) : null}
        </>
      }
    />
  );

  // THE FLOOR HAS TWO VERBS AND ONE BUTTON, and they are the owner's words:
  //
  //   CANCEL   undoes the whole thing. Before the bind that is simply leaving,
  //            because nothing exists; after it, the domain is REMOVED -- so it
  //            asks first, by name, and says what it costs.
  //   LEAVE    goes, and everything stays: the binding exists, the cluster
  //            keeps checking it, and its row opens this wizard again where it
  //            was left.
  //
  // Cancel is a text action and the act is the button (`Act.text`): two
  // buttons side by side ask to be weighed against each other, and a way out
  // is not the same kind of thing as what happens next.
  function actsFor(): Act[] {
    if (bound === null) {
      if (domainId !== "") return [];
      return [
        { label: "Cancel", text: true, onAct: onLeave },
        // ABSENT, NEVER DISABLED (rule 12). The bar says what is still missing.
        ...(wellFormed ? [{ label: "Add domain", tone: "primary" as const, busy, onAct: () => void add(site.id, typed) }] : []),
      ];
    }
    const host = bound.hostname;
    const id = bound.id;
    if (confirming) {
      return [
        { label: "Keep", text: true, onAct: () => setConfirming(false) },
        {
          label: "Remove", tone: "danger", busy: removal.busy === id,
          onAct: () => void removal.remove(id).then((ok) => {
            if (!ok) return;
            setConfirming(false);
            // A domain cancelled is gone, and the list is where the person
            // was going next. One that was serving stays on its page, which
            // now says it is on its way out.
            if (!finished) onLeave();
          }),
        },
      ];
    }
    const ask = () => { removal.reset(); setConfirming(true); };
    if (leaving) return [{ label: "Done", tone: "primary", onAct: onLeave }];
    if (finished) {
      // THE ADDING IS OVER, so there is nothing to cancel: what is left is the
      // domain's own page, and taking a serving domain away is Remove.
      return [
        ...(removable ? [{ label: "Remove", ariaLabel: `Remove ${host}`, text: true, onAct: ask }] : []),
        { label: "Done", tone: "primary", onAct: onLeave },
      ];
    }
    return [
      // Only once the row has arrived, because that is the id a removal is
      // asked about. It is a beat; until then Leave is the whole floor.
      ...(removable ? [{ label: "Cancel", ariaLabel: `Cancel adding ${host}`, text: true, onAct: ask }] : []),
      { label: "Leave", onAct: onLeave },
    ];
  }

  function statusFor(): WizardStatus {
    // WHILE IT IS ASKING, THE FLOOR IS THE QUESTION. The waiting sentence, its
    // detail and its measure stood beside the question and squeezed each other
    // into ellipses; the Fleet's floor already steps aside the same way.
    if (confirming) return { word: finished ? "Remove?" : "Cancel?", tone: "paused" };
    if (reading) return { word: "Reading this domain", tone: "busy" };
    if (missing) return { word: "Not bound", detail: "there is nothing here to set up" };
    if (bound !== null) {
      if (bound.status === "removed") return { word: "Removed", detail: "it is kept as a record of what this cluster served" };
      if (bound.status === "removing") {
        return bound.failureReason === ""
          ? { word: "Removing", detail: "it has already stopped being served", tone: "busy" }
          : { word: "Could not be removed yet", detail: "the cluster tries again by itself", tone: "paused", meta: bound.lastCheckedAt === "" ? undefined : `tried ${formatFreshness(bound.lastCheckedAt, now)}` };
      }
      const reading = domainSetupReading(bound, siteLive);
      return {
        ...reading,
        meta: reading.tone !== "busy" ? undefined : bound.lastCheckedAt === "" ? "not checked yet" : `checked ${formatFreshness(bound.lastCheckedAt, now)}`,
      };
    }
    if (busy) return { word: `Adding ${typed}`, tone: "busy" };
    if (error !== "") return { word: "Not added", detail: "change the name, or try again", tone: "paused" };
    if (wellFormed) return { word: "Ready to add", detail: `${typed} will be added to ${name}` };
    return {
      word: "Name the domain",
      detail: typed === "" ? "the full name visitors will type, like www.acme.com" : "a domain needs at least one dot, like www.acme.com",
    };
  }
}

/**
 * What one setup stage holds.
 *
 * THE REPORT SITS AT THE STEP IT IS ABOUT. The old card printed the sweep's
 * sentence and detail in a block above all four steps, in the error colour,
 * whatever the reason -- so a record somebody had been handed a minute ago
 * and not created yet opened on a red box saying it was not published. Here a
 * reason the sweep will simply outwait is the step's own waiting line plus
 * what the last check SAW; only a reason it cannot outwait is a problem, and
 * it is said under the step that stopped.
 */
function DomainStepBody({ domain: d, site, step }: { domain: DomainRow; site: SiteRow; step: DomainStepId }) {
  const [method, setMethod] = useState<PointingMethod>("ADDRESS");
  const guidance = useDomainDNSGuidance(d.id, step === "dns" && !isRemovalPath(d.status));
  const records = recordsFor(d, guidance.value, method);
  const serving = d.status === "live" && site.status === "live";

  const at = SETUP_STEPS[domainSetupStep(d)];
  const report = d.status === "live" || step !== at || d.failureReason === "" ? null : isWaitingReason(d.failureReason)
    // THE SERVER'S OWN WORDS, in the data voice. What the sweep saw is a
    // different fact from what is wrong, and somebody editing a zone file
    // needs it -- "no such record" and "resolves to 198.51.100.7" are the
    // difference between a fix and a guess.
    ? d.failureDetail === "" ? null : <p className="os-wizard-seen">The last check saw: <code>{d.failureDetail}</code></p>
    : (
      <div className="os-domain-problem" data-known={isKnownFailure(d.failureReason)}>
        <p className="os-domain-problem-sentence">{failureSentence(d.failureReason)}</p>
        {d.failureDetail === "" ? null : <p className="os-domain-problem-detail os-mono">{d.failureDetail}</p>}
      </div>
    );

  if (step === "ownership") return <>
    <Caption>Add this TXT record at your domain provider. Verification runs automatically.</Caption>
    <RecordStrip record={records[0]!} awaited={isRecordAtFault(records[0]!, d.failureReason)} />
    {report}
  </>;
  if (step === "dns") return <>
    <div className="os-domain-method">
      <Field label="DNS record type">
        <Select id={`domain-record-${d.id}`} label="DNS record type" value={method} onChange={value => setMethod(value as PointingMethod)}>
          <option value="ADDRESS">A / AAAA — standard DNS</option>
          <option value="CNAME">CNAME — subdomain or provider flattening</option>
          <option value="ALIAS">ALIAS / ANAME — if supported</option>
        </Select>
      </Field>
      <InfoDetail title="How domain routing works"><p>DNS sends visitors to the cluster. The binding for {d.hostname} selects this website, and the visitor keeps your domain in the address bar. Use the displayed current addresses; update these records if the cluster’s public addresses change.</p></InfoDetail>
    </div>
    {method === "CNAME" ? <Notice tone="info" sentence="A root domain needs provider support for CNAME flattening."
      next="GoDaddy does not support root CNAME records. Choose A / AAAA for a root domain there. For a subdomain, bind its full name, such as www.example.com." /> : null}
    {method === "ALIAS" ? <Caption>Use this only when your provider offers ALIAS or ANAME. Otherwise choose A / AAAA.</Caption> : null}
    {guidance.error ? <Notice tone="error" sentence="DNS targets could not be loaded." detail={guidance.error}><Button onClick={guidance.retry}>Try again</Button></Notice>
      : !guidance.value ? <Caption>Reading this cluster’s DNS targets…</Caption>
      : <>
        <Caption>{method === "ADDRESS" ? "Add the address records below at your DNS provider. Replace previous website addresses for this same name; keep your email and ownership records." : "Copy the name and target below into your DNS provider."}</Caption>
        {records.slice(1).map(record => <RecordStrip key={`${record.kind}:${record.value}`} record={record} awaited={isRecordAtFault(record, d.failureReason)} />)}
      </>}
    <Caption>{isApex(d.hostname) ? "@ means your root domain." : "If your provider adds the domain automatically, enter only the subdomain in its Name field."} The hostname stays {d.hostname}.</Caption>
    {report}
  </>;
  if (step === "certificate") return <>
    {report ?? <Caption>Ownership and DNS are verified. Waiting for the HTTPS certificate and route to be ready.</Caption>}
  </>;
  return <>
    <Caption>{serving ? "DNS is verified and the HTTPS certificate is ready." : "DNS and HTTPS are ready. This deployable must be live before its domain can serve content."}</Caption>
    {serving ? <a href={`https://${d.hostname}`} target="_blank" rel="noreferrer noopener">Open {d.hostname}</a> : null}
  </>;
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
function RecordStrip({ record, awaited }: { record: DnsRecord; awaited: boolean }) {
  return (
    <div className="os-record" data-awaited={awaited}>
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
