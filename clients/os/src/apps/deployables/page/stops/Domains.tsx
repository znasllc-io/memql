import { useState } from "react";
import { Check, Copy, Globe } from "lucide-react";

import { Button, Caption, FormRow, Input, Notice, Subhead } from "../../../../kit";
import { formatFreshness } from "../../../../kit/format";
import { useNow } from "../../../../kit/useNow";
import { LiveList } from "../../../../live/LiveList";
import { useLiveView } from "../../../../live/liveView";
import { useAddDomain, useRemoveDomain } from "../../domainActions";
import {
  DOMAIN_STEPS,
  domainFingerprint,
  domainFromRow,
  edgeHostFor,
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
  stepIndexFor,
  type DnsRecord,
  type DomainRow,
} from "../../domains";
import { useCustomDomains } from "../../useCustomDomains";
import type { SiteRow } from "../../rows";

// A client's own domain, bound to this deployable -- the content of what was
// the Domains panel (memql#4805), mounted as the Where-it-lives stop's body
// (epic memql#4885): on a redeploy the stop is facts, and each bound domain's
// stepped rail with its two records and what the sweep last saw is one of
// them. Nothing below changed in the move; `domains.ts`, `domainActions.ts`
// and `useCustomDomains.ts` are untouched.
//
// ===========================================================================
// THE SURFACE IS ABOUT TWO RECORDS AND WHAT WE SEE AT THEM
// ===========================================================================
// Somebody reading this panel is about to alt-tab to Cloudflare, Route 53 or
// GoDaddy and type into a form whose fields are called Type, Name and Value. So
// every record is rendered in exactly those three parts, in that vocabulary,
// each one copyable on its own -- and directly beneath it, joined by a hairline,
// is what this cluster actually SAW at that name the last time it looked.
//
// That pairing is the whole design. "dns_not_pointing" says which record is
// wrong; the observation says what is in it. A person fixing a zone file needs
// both, and neither one alone is worth a panel.
//
// ===========================================================================
// PRESENTATION OVER SERVER LAW
// ===========================================================================
// v1:platform:customDomain is clusterOwner tier and the three guards run in Go
// beside executeWrite. The admin gate here decides what RENDERS and nothing
// else (design D1) -- showing somebody a button that always fails teaches
// nobody who can use it.
//
// THERE IS NO RE-CHECK BUTTON, anywhere, deliberately (design D5). Retries ride
// the sweep's own schedule. A button would invite hammering exactly the two
// things that must not be hammered -- a recursive resolver and an ACME endpoint
// -- and would make the fastest path to a certificate the one where somebody
// clicks fastest. The panel says so in as many words, because an absent control
// with no explanation reads as an omission.

export function DomainsContent({ site, domain }: { site: SiteRow; domain: string }) {
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
        A client's own domain, served by this deployable. Its own hostname{" "}
        <code className="os-mono">{site.hostname || "--"}</code> keeps working either way.
      </Caption>
      {/* WHO THIS IS FOR, SAID ONCE. The concept is clusterOwner tier, so on a
          cluster where admin and cluster owner are different people an admin
          reads no bindings at all -- the filter narrows, it does not error, so
          without this line an empty list would be a false statement rather than
          an empty one. The panel deliberately does not detect which reader it
          has: it says what is true for both, and anyone who tries anyway gets
          the server's own sentence beside the control they used. */}
      <Caption>
        Binding a domain is a cluster owner's job in this version -- a certificate is a
        cluster-level resource with real rate limits.
      </Caption>

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
          next="A domain below marked serving has finished its own setup -- both DNS records check out and its certificate is issued. What a visitor actually gets is decided by the deployable's status, above."
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
        emptyText="No domains here. Add one above and this cluster will tell you which records to create."
        rowId={(d) => d.id}
        fingerprint={domainFingerprint}
        renderRow={(d) => <DomainCard key={d.id} domain={d} clusterDomain={domain} />}
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
          next="Create the two records shown on its card below. This cluster checks every couple of minutes and will say which one is still missing."
        />
      )}

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="That domain was not bound."
          next="The cluster decides which hostnames can be bound here -- including whether one is already taken, which this window cannot know."
          detail={error}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// One binding
// ---------------------------------------------------------------------------

function DomainCard({ domain: d, clusterDomain }: { domain: DomainRow; clusterDomain: string }) {
  const now = useNow(15_000);
  const tone = statusTone(d);
  const records = recordsFor(d, clusterDomain);
  const removalPath = isRemovalPath(d.status);
  const sentence = failureSentence(d.failureReason);

  return (
    <article className="os-domain" data-tone={tone} data-status={d.status}>
      <header className="os-domain-head">
        <h5 className="os-domain-host">{d.hostname}</h5>
        <span className="os-domain-status" data-tone={tone}>
          {statusLabel(d.status)}
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
        <StatusRail status={d.status} blocked={d.failureReason !== ""} />
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
        <div className="os-domain-records">
          {records.map((r) => (
            <RecordStrip
              key={r.kind + r.name}
              record={r}
              faulty={isRecordAtFault(r, d.failureReason)}
            />
          ))}
          {isApex(d.hostname) ? (
            <Caption>
              A domain's root cannot carry a CNAME. Most providers offer ALIAS or ANAME for exactly
              this; if yours does not, create A records pointing at whatever{" "}
              <code className="os-mono">{edgeHostFor(clusterDomain)}</code> resolves to.
            </Caption>
          ) : null}
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
          <span>We look again every couple of minutes, so there is nothing to press.</span>
        )}
      </footer>
    </article>
  );
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

/**
 * The walk, as four stops.
 *
 * A NUMBERED SEQUENCE IS HONEST HERE, which is not usually true of stepped
 * rails: a binding genuinely passes through each of these in turn and cannot
 * skip one, so the order carries information the reader needs -- "the records
 * check out, we are waiting on a certificate" is a different situation from
 * "the records are wrong", and they look different because they are.
 */
function StatusRail({ status, blocked }: { status: string; blocked: boolean }) {
  const at = stepIndexFor(status);
  const here = at >= 0 ? (DOMAIN_STEPS[at] ?? null) : null;
  return (
    <div className="os-domain-progress">
      <ol className="os-domain-rail" aria-label="Progress">
        {DOMAIN_STEPS.map((step, i) => {
          const state = i < at ? "done" : i === at ? (blocked ? "blocked" : "current") : "ahead";
          return (
            <li key={step.status} className="os-domain-stop" data-state={state}>
              <span className="os-domain-stop-dot" aria-hidden />
              {step.label}
            </li>
          );
        })}
      </ol>
      {/* ONLY THE CURRENT STOP EXPLAINS ITSELF, and it does so BELOW the rail
          rather than inside its own item. Four blurbs at once is a paragraph
          nobody reads; one inside its item makes that stop twice as wide as
          the others and turns an evenly-spaced sequence into a ragged one. */}
      {here === null ? null : <p className="os-domain-stop-blurb">{here.blurb}</p>}
    </div>
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
