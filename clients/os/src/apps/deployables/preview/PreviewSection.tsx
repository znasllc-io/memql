import { useMemo, useState } from "react";
import { Check, CircleDashed, ExternalLink, ShoppingBag, X } from "lucide-react";

import { Button, Caption, CopyValue, Notice, Select, Subhead } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { RefreshButton } from "../../../kit/RefreshButton";
import type { DeploymentRow } from "../packages/rows";
import type { PartsHeld } from "../parts";
import { type SiteRow } from "../rows";
import {
  OBSERVATION_ORDER,
  OBSERVATION_WORDS,
  grantIsOpen,
  latestByKind,
  shortRef,
  type ObservationKind,
  type PreviewObservationRow,
  type PreviewReadiness,
  type PreviewRefusal,
} from "./rows";
import {
  usePreviewGrants,
  usePreviewObservations,
  usePreviewReadiness,
  usePreviewWrites,
} from "./usePreview";

// PreviewSection -- the version being exercised, beside the version that serves
// (epic memql#5531; DESIGN.md rules 6, 8 and 12; SUPERVISED-VISUAL-COMPOSITION).
//
// ===========================================================================
// THE TWO LANES ARE THE WHOLE IDEA, SO THEY ARE THE WHOLE DESIGN
// ===========================================================================
// A storefront preview is one sentence: THERE IS A VERSION THAT SERVES AND A
// VERSION BEING EXERCISED, AND EACH TALKS TO A DIFFERENT STORE. Everything a
// person can get wrong here is a confusion between those two, and the worst of
// them -- exercising against the store shoppers reach -- is silent right up to
// the moment a test payment lands in a merchant's real orders.
//
// So the two readings are drawn as two LANES of one object rather than as two
// cards: a shared marker column, filled for the one that serves and hollow for
// the one that does not, and the store named ON each lane rather than in a slot
// somewhere else on the page. A person cannot read this section without reading
// which store each version talks to, which is the point.
//
// It is the one place this surface spends any boldness. Everything under it --
// the observations, the refusals, the open previews -- is deliberately quiet.
//
// ===========================================================================
// AN UNMEASURED STEP IS NOT A FAILED ONE, AND IT IS NOT A ZERO
// ===========================================================================
// Three states, three glyphs, three words, and never colour alone: a step that
// ANSWERED, a step that was asked and DID NOT, and a step NOBODY HAS ASKED. The
// third draws an open ring, which is the Benchmarks surface's open notch and
// the same rule: never turn a missing measurement into an invented fact.
//
// ===========================================================================
// AN ACT THAT IS NOT LEGAL IS ABSENT (rule 12)
// ===========================================================================
// And it can be, because the engine says so BEFORE the click:
// `sitePreviewReadiness` runs the same functions the write guard refuses with,
// so a control drawn here is one the engine will accept. Where an act is
// withheld the REASON is drawn instead, with the act that clears it -- a
// missing button and no explanation is the failure this replaces.
//
// ===========================================================================
// WHAT THIS SECTION DOES NOT CARRY
// ===========================================================================
// PROMOTE and ROLL BACK are not here. Promoting changes what the public is
// served, which is the deployable's own state, and rule 12 puts every act that
// changes it on the one action bar at the window's edge. Rolling back is
// pointing the bundle at the previous version -- one write, through the history
// this page already has -- and giving it a second home here would make the
// cheap operation look like the expensive one.

export function PreviewSection({
  site,
  runs,
  can,
  onOpenStore,
}: {
  site: SiteRow;
  /** This source's timeline, for the versions it has published. */
  runs: readonly DeploymentRow[];
  can: PartsHeld;
  /** Opens the Store panel, where a development store is attached. */
  onOpenStore: () => void;
}) {
  const readiness = usePreviewReadiness(site.id);
  const observations = usePreviewObservations(site.id);
  const grants = usePreviewGrants(site.id);
  const reread = (): void => {
    readiness.reread();
    observations.reread();
    grants.reread();
  };
  const writes = usePreviewWrites(reread);
  const now = Date.now();
  const open = grants.grants.filter((g) => grantIsOpen(g, now));
  const mine = readiness.readiness;

  // The versions this deployable has published, newest first, minus the one it
  // is already serving -- a candidate that is serving is not a candidate, and
  // the engine refuses it, so it is not offered.
  const versions = useMemo(() => publishedVersions(runs, site), [runs, site]);

  return (
    <section className="deployable-preview" aria-label="Preview">
      <header>
        <h3>Preview</h3>
        <RefreshButton label="Read the preview again" onClick={reread} busy={readiness.state === "reading"} />
      </header>

      {readiness.state === "failed" ? (
        <Notice tone="error" sentence="What is legal here could not be read." detail={readiness.error}>
          <Button onClick={reread}>Try again</Button>
        </Notice>
      ) : null}

      <ol className="preview-lanes">
        <li className="preview-lane" data-serving="true">
          <span className="preview-lane-mark" aria-hidden />
          <span className="preview-lane-role">Serving</span>
          <strong className="os-mono">{site.bundleRef === "" ? "No version yet" : shortRef(site.bundleRef)}</strong>
          <StoreLine
            domain={mine?.storeDomain ?? ""}
            storeId={mine?.storeId ?? ""}
            readable={mine?.storeReadable ?? false}
            blurb="the store shoppers reach"
            attach="store"
            storefront={mine?.storefront ?? false}
            onOpenStore={onOpenStore}
          />
        </li>
        <li className="preview-lane" data-serving="false">
          <span className="preview-lane-mark" aria-hidden />
          <span className="preview-lane-role">Candidate</span>
          {site.candidateRef === "" ? (
            <strong className="preview-lane-empty">No version being exercised</strong>
          ) : (
            <strong className="os-mono">{shortRef(site.candidateRef)}</strong>
          )}
          {/* THE LANE SAYS WHAT THE STORE IS, NOT WHAT THE SLOT IS FOR. It said
              "development store" under whatever was bound, so a preview binding
              pointed at the live store was LABELLED a development store on the
              same screen as the refusal explaining that it is not one -- a
              sentence contradicting its own notice, which is a class of defect
              only a rendered page shows. The lane carries the warning now. */}
          <StoreLine
            domain={mine?.previewStoreDomain ?? ""}
            storeId={mine?.previewStoreId ?? ""}
            readable={mine !== null && mine.previewStoreId !== "" && mine.previewStoreDomain !== ""}
            blurb={previewStoreBlurb(mine)}
            attach="development store"
            storefront={mine?.storefront ?? false}
            onOpenStore={onOpenStore}
          />
        </li>
      </ol>

      {/* ONE ACT LINE, UNDER THE LANES THEY ACT ON. Two stacked rows of buttons
          with a sentence between them read as two unrelated groups, which is
          what this was before the pixels said so -- and rule 5 asks for one
          control line. Primary LAST, as the action bar orders its own. */}
      {can.preview && !site.systemOwned ? (
        <CandidateControls
          site={site}
          versions={versions}
          busy={writes.busy}
          canPreview={mine !== null && mine.canPreview}
          canCheck={open.length > 0 && mine !== null && mine.storefront}
          onSet={(ref) => void writes.setCandidate(site.id, ref)}
          onClear={() => void writes.clearCandidate(site.id)}
          onOpen={() => void writes.openPreview(site.id)}
          onCheck={() => {
            const newest = open[0];
            if (newest !== undefined) void writes.runChecks(newest.id);
          }}
        />
      ) : null}

      {/* THE REFUSAL, WHERE THE ACT WOULD HAVE BEEN. */}
      {mine !== null && site.candidateRef !== "" && !mine.canPreview ? (
        <RefusalNotice refusal={mine.previewRefusal} onOpenStore={onOpenStore} storefront={mine.storefront} />
      ) : null}

      {writes.error !== "" ? (
        <Notice tone="error" sentence="The cluster refused that." detail={writes.error} />
      ) : null}
      {writes.note !== "" && writes.opened === null ? <Notice sentence={writes.note} /> : null}

      {writes.opened !== null ? (
        <div className="preview-link">
          <Subhead>Your preview link</Subhead>
          <Caption>
            It opens {shortRef(writes.opened.candidateRef)} at {writes.opened.hostname}, for you and for
            nobody else, for the next {writes.opened.ttlMinutes} minutes. It is shown once -- the cluster
            keeps only a digest of it, so it cannot be read back.
          </Caption>
          <div className="preview-link-row">
            <a className="preview-link-open" href={writes.opened.url} target="_blank" rel="noreferrer">
              <ExternalLink size={14} aria-hidden />
              Open the preview
            </a>
            <CopyValue value={writes.opened.url} label="preview link" />
          </div>
        </div>
      ) : null}

      {mine !== null && mine.storefront ? (
        <Observations
          rows={observations.observations}
          state={observations.state}
          error={observations.error}
          onRetry={() => observations.reread()}
        />
      ) : null}

      {open.length > 0 ? (
        <OpenPreviews
          grants={open}
          busy={writes.busy}
          canEnd={can.preview}
          onEnd={(id) => void writes.endPreview(id)}
          hostname={site.hostname}
        />
      ) : null}
    </section>
  );
}

/**
 * The store one lane talks to.
 *
 * FOUR ANSWERS AND THEY ARE DIFFERENT FACTS. Nothing attached is an invitation.
 * A store that reads back gets its domain. A store that is named and cannot be
 * read is NOT drawn as unattached -- that would hide a real misconfiguration
 * behind a state that looks deliberate, which is the same rule the Store slot
 * on the composition follows. A deployable that is not a storefront has no
 * store at all and the line is absent rather than empty.
 */
function StoreLine({
  domain,
  storeId,
  readable,
  blurb,
  attach,
  storefront,
  onOpenStore,
}: {
  domain: string;
  storeId: string;
  readable: boolean;
  /** What this store IS, in a reader's words. Never what the slot is for. */
  blurb: string;
  /** What to invite when nothing is bound, which IS what the slot is for. */
  attach: string;
  storefront: boolean;
  onOpenStore: () => void;
}) {
  if (!storefront) return <span className="preview-lane-store preview-lane-store-none">No store — this is not a storefront</span>;
  if (storeId === "") {
    return (
      <button type="button" className="preview-lane-store preview-lane-store-empty" onClick={onOpenStore}>
        <ShoppingBag size={13} aria-hidden />
        Attach a {attach}
      </button>
    );
  }
  return (
    <span className="preview-lane-store">
      <ShoppingBag size={13} aria-hidden />
      <strong>{readable && domain !== "" ? domain : "A store you cannot read"}</strong>
      <small>{blurb}</small>
    </span>
  );
}

/**
 * What to call the store on the CANDIDATE lane.
 *
 * It is the one label on this surface that must not be assumed. A preview
 * binding pointed at the store shoppers reach is the exact situation the guard
 * refuses, and calling it a development store on the way past would be the
 * screen asserting the thing the notice beneath it denies.
 */
function previewStoreBlurb(mine: PreviewReadiness | null): string {
  if (mine === null || mine.previewStoreId === "") return "development store";
  if (mine.previewStoreId === mine.storeId) return "the store shoppers reach — not a development store";
  return "development store";
}

/**
 * The candidate's own acts, on one line: withdraw it, open a preview of it, run
 * the checks against it.
 *
 * THEY ARE ONE LINE BECAUSE THEY ARE ONE SUBJECT -- the version being exercised
 * -- and rule 5 asks for one control line. An act whose part or whose state
 * does not permit it is ABSENT from the line rather than drawn inert.
 */
function CandidateControls({
  site,
  versions,
  busy,
  canPreview,
  canCheck,
  onSet,
  onClear,
  onOpen,
  onCheck,
}: {
  site: SiteRow;
  versions: readonly string[];
  busy: string;
  canPreview: boolean;
  canCheck: boolean;
  onSet: (ref: string) => void;
  onClear: () => void;
  onOpen: () => void;
  onCheck: () => void;
}) {
  const [picked, setPicked] = useState("");
  const [byHand, setByHand] = useState(false);
  const [typed, setTyped] = useState("");

  if (site.candidateRef !== "") {
    return (
      <>
        <div className="preview-acts">
          <Button busy={busy === "candidate"} busyLabel="Withdrawing" onClick={onClear}>
            Withdraw the candidate
          </Button>
          {canCheck ? (
            <Button busy={busy === "checks"} busyLabel="Checking" onClick={onCheck}>
              Run the checks
            </Button>
          ) : null}
          {canPreview ? (
            <Button tone="primary" busy={busy === "open"} busyLabel="Opening" onClick={onOpen}>
              Open a preview
            </Button>
          ) : null}
        </div>
        <Caption>Withdrawing ends every preview of it. Nothing the public sees changes.</Caption>
      </>
    );
  }

  // NO VERSIONS AND NO SOURCE is not a dead end -- a deployable published by CI
  // has versions this page has no timeline for, and its operator knows the
  // reference. The field is behind a disclosure rather than in front, because
  // typing a bundle reference is the rare path.
  return (
    <div className="preview-candidate-choose">
      {versions.length > 0 && !byHand ? (
        <>
          <Select
            id={`candidate-${site.id}`}
            label="A version to exercise"
            value={picked}
            onChange={setPicked}
          >
            <option value="">Choose a version to exercise…</option>
            {versions.map((ref) => (
              <option key={ref} value={ref}>
                {shortRef(ref)}
              </option>
            ))}
          </Select>
          <Button
            tone="primary"
            busy={busy === "candidate"}
            busyLabel="Setting"
            onClick={() => {
              if (picked !== "") onSet(picked);
            }}
          >
            Exercise this version
          </Button>
        </>
      ) : (
        <>
          <input
            className="os-input"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            placeholder="blob://sites/<id>/<version>/"
            aria-label="A bundle reference to exercise"
          />
          <Button
            tone="primary"
            busy={busy === "candidate"}
            busyLabel="Setting"
            onClick={() => {
              if (typed.trim() !== "") onSet(typed);
            }}
          >
            Exercise this version
          </Button>
        </>
      )}
      {versions.length > 0 ? (
        <Button tone="quiet" onClick={() => setByHand((v) => !v)}>
          {byHand ? "Choose from this source" : "Name a version instead"}
        </Button>
      ) : null}
    </div>
  );
}

/** A refusal, with the act that clears it. */
function RefusalNotice({
  refusal,
  storefront,
  onOpenStore,
}: {
  refusal: PreviewRefusal;
  storefront: boolean;
  onOpenStore: () => void;
}) {
  if (refusal.code === "") return null;
  // The store is where all three store-shaped refusals are answered, so the
  // notice carries the way there rather than leaving a person to find it.
  const aboutTheStore =
    refusal.code === "preview_binding_is_not_development_store" ||
    refusal.code === "no_preview_binding" ||
    refusal.code === "bound_store_unreadable";
  return (
    <Notice tone="warn" sentence={refusal.message} next={refusal.remedy}>
      {aboutTheStore && storefront ? <Button onClick={onOpenStore}>Open the store</Button> : null}
    </Notice>
  );
}

/**
 * What this cluster watched, in the order it happens.
 *
 * AN ORDERED READING, because the four steps ARE a sequence: nothing can be
 * added to a cart without a catalog, and no order arrives without a checkout.
 * No numbered markers -- the order alone carries it, and a person reading down
 * the list finds the first step that did not answer, which is the one to fix.
 */
function Observations({
  rows,
  state,
  error,
  onRetry,
}: {
  rows: readonly PreviewObservationRow[];
  state: string;
  error: string;
  onRetry: () => void;
}) {
  const latest = useMemo(() => latestByKind(rows), [rows]);
  const anything = OBSERVATION_ORDER.some((kind) => latest[kind] !== undefined);

  return (
    <div className="preview-watched">
      <Subhead>What this cluster watched</Subhead>
      {state === "failed" ? (
        <Notice tone="error" sentence="The observations could not be read." detail={error}>
          <Button onClick={onRetry}>Try again</Button>
        </Notice>
      ) : null}
      {!anything && state !== "failed" ? (
        <Caption>
          Nothing has been exercised yet. Open a preview, then run the checks to ask the development
          store these four questions.
        </Caption>
      ) : null}
      <ul className="preview-observations">
        {OBSERVATION_ORDER.map((kind) => (
          <ObservationRow key={kind} kind={kind} row={latest[kind]} />
        ))}
      </ul>
      <Caption>
        The payment itself happens in a browser on Shopify's own checkout. This cluster cannot watch
        it, and does not pretend to.
      </Caption>
    </div>
  );
}

function ObservationRow({ kind, row }: { kind: ObservationKind; row: PreviewObservationRow | undefined }) {
  const words = OBSERVATION_WORDS[kind];
  // THREE STATES, THREE GLYPHS, THREE WORDS. Never colour alone: the glyphs
  // differ in shape so the reading survives a monochrome screen and a reader
  // who cannot tell the tones apart.
  const measured = row !== undefined;
  const ok = measured && row.ok;
  const state = !measured ? "unmeasured" : ok ? "answered" : "failed";
  const word = !measured ? "not measured yet" : ok ? "answered" : "did not answer";
  const said = !measured ? words.blurb : ok ? row.detail : row.failure;
  return (
    <li className="preview-observation" data-state={state}>
      <span className="preview-observation-mark" aria-hidden>
        {!measured ? <CircleDashed size={15} /> : ok ? <Check size={15} /> : <X size={15} />}
      </span>
      <span className="preview-observation-label">{words.label}</span>
      <span className="preview-observation-said">
        <strong>{word}</strong>
        {/* A MEASURED ROW SHOWS THE ENGINE'S OWN WORDS OR NOTHING. Falling back
            to the blurb would put a question ("whether a cart accepts a line")
            under an answer ("answered"), which reads as a surface that did not
            know what it measured. */}
        {said !== "" ? <small>{said}</small> : null}
      </span>
      <span className="preview-observation-took">
        {measured && row.durationMs > 0 ? `${row.durationMs} ms` : ""}
      </span>
    </li>
  );
}

/**
 * The previews open right now.
 *
 * LISTED AT ALL because a preview link is a credential somebody is holding: an
 * operator needs to see what is out there, and be able to end it. Ending one is
 * REVOCATION -- the link stops working for everybody, which is what makes it
 * worth having beside a link that can be forwarded.
 */
function OpenPreviews({
  grants,
  busy,
  canEnd,
  onEnd,
  hostname,
}: {
  grants: readonly { id: string; candidateRef: string; expiresAt: string; lastSeenAt: string }[];
  busy: string;
  canEnd: boolean;
  onEnd: (id: string) => void;
  hostname: string;
}) {
  return (
    <div className="preview-open">
      <Subhead>Previews open now</Subhead>
      <ul>
        {grants.map((g) => (
          <li key={g.id}>
            <span className="os-mono">{shortRef(g.candidateRef)}</span>
            <small>
              {/* ABSENT MEANS NOBODY OPENED IT, never "a long time ago". The two
                  are different answers to somebody deciding whether a link
                  escaped, so they are drawn as different sentences. */}
              {g.lastSeenAt === "" ? "not opened yet" : `last used ${relative(g.lastSeenAt)}`}
              {" · "}
              {expiryWord(g.expiresAt)}
            </small>
            {canEnd ? (
              <IconButton label="End this preview" onClick={() => onEnd(g.id)} disabled={busy === "end"}>
                <X size={15} aria-hidden />
              </IconButton>
            ) : null}
          </li>
        ))}
      </ul>
      <Caption>
        A preview link works on {hostname} and for the person it was opened for. Ending one stops it
        working for anybody holding it.
      </Caption>
    </div>
  );
}

/**
 * The versions this deployable has published, newest first, minus the one it
 * serves.
 *
 * THE SERVING VERSION IS EXCLUDED because a candidate that is serving is not a
 * candidate: there would be nothing to exercise, and the engine refuses it. An
 * option the server would reject is a broken control, not a permission.
 */
export function publishedVersions(runs: readonly DeploymentRow[], site: SiteRow): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const run of runs) {
    for (const outcome of run.deployables) {
      if (outcome.siteId !== site.id && outcome.name !== site.packageDeployableName) continue;
      const ref = outcome.bundleRef.trim();
      if (ref === "" || ref === site.bundleRef || seen.has(ref)) continue;
      seen.add(ref);
      out.push(ref);
    }
  }
  return out;
}

/**
 * How long a preview has left, in the largest unit that reads.
 *
 * MINUTES ALONE DO NOT SCALE, which the pixels said and no test could: a grant
 * far enough out reads "38015623 minutes left", which is not a duration
 * anybody can hold. A preview is half an hour by default and capped at four,
 * so minutes cover every real case -- but a fixture, a clock skew or a
 * configuration nobody expected must not produce a number that reads as a
 * rendering bug.
 */
function expiryWord(at: string): string {
  const ms = Date.parse(at);
  if (!Number.isFinite(ms)) return "expiry unknown";
  const mins = Math.round((ms - Date.now()) / 60000);
  if (mins <= 0) return "expired";
  if (mins < 60) return mins === 1 ? "1 minute left" : `${mins} minutes left`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return hours === 1 ? "1 hour left" : `${hours} hours left`;
  const days = Math.round(hours / 24);
  return days === 1 ? "1 day left" : `${days} days left`;
}

function relative(at: string): string {
  const ms = Date.parse(at);
  if (!Number.isFinite(ms)) return "at an unreadable time";
  const mins = Math.round((Date.now() - ms) / 60000);
  if (mins <= 0) return "just now";
  if (mins === 1) return "a minute ago";
  if (mins < 60) return `${mins} minutes ago`;
  const hours = Math.round(mins / 60);
  return hours === 1 ? "an hour ago" : `${hours} hours ago`;
}
