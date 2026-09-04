import { useState } from "react";
import { Archive, ArrowUpCircle, GitBranch, KeyRound, RotateCcw, Zap } from "lucide-react";

import { Button, Caption, Check, Chip, Chips, Fact, Facts, FormRow, Input } from "../../../../kit";
import { formatMoment } from "../../../../kit/format";
import { usePackageActions } from "../../packages/actions";
import { ProblemNotice } from "../../packages/ReportView";
import { shortVersion, sourceLabel, type PackageRow } from "../../packages/rows";
import { bundleForm, bundleFormNote, siteName, type SiteRow } from "../../rows";
import { CredentialField } from "../../sources/CredentialField";
import { credentialIsRevoked, isGithubAppGrant, type CredentialRow } from "../../sources/rows";
import { isPlaceholderBundle, type RailProblem } from "../rail";
import { ZipPicker } from "./ZipPicker";

// The Source stop: where this deployable comes from, as facts.
//
// The rail's note already names the source in one line -- the repository at
// its ref, or the bundle form -- so the body does not repeat it (DESIGN.md
// rule 7). What it adds is what a person asks next: which version is
// deployed and which is upstream, which credential the fetch runs under, the
// bundle reference the site holds now (with the flip marker when it just
// changed under them), and the acts that belong to the SOURCE rather than
// to any one app it produced -- archive with every app, restore, or for a
// hand-made deployable the zip that becomes its next version.

export function SourceStop({
  site,
  pkg,
  credentials,
  canWrite,
  flipped,
  zipOpen,
  onZipOpenChange,
  refusal,
  onOpenSource,
}: {
  site: SiteRow;
  pkg: PackageRow | null;
  credentials: readonly CredentialRow[];
  canWrite: boolean;
  /** The bundle reference changed while the page was open. */
  flipped: boolean;
  zipOpen: boolean;
  onZipOpenChange: (open: boolean) => void;
  /** The newest run's refusal, when it stopped here. */
  refusal: RailProblem | null;
  /** Opens the source's own view. Absent for a hand-made deployable, which has no source. */
  onOpenSource?: () => void;
}) {
  return (
    <div className="os-stop-body">
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {pkg === null ? (
        <HandMadeSource site={site} flipped={flipped} />
      ) : (
        <PackageSource pkg={pkg} site={site} credentials={credentials} flipped={flipped} />
      )}
      {/* THE SOURCE'S OWN CONTROLS ARE NOT HERE ANY MORE (epic memql#4937,
          D4). The credential, the auto-deploy switch, the run history and
          "archive this source and every app it produced" are facts about the
          SOURCE, and rendering them inside each app's Source stop drew the
          same 2,600px of history twice and put a control that archives a
          SIBLING 1,614px above this page's own archive. They live on the
          source's own view; this is the way in. */}
      {pkg !== null && onOpenSource ? (
        <Button onClick={onOpenSource}>
          <GitBranch size={12} aria-hidden /> Open {sourceLabel(pkg)}
        </Button>
      ) : null}
      {/* A system-owned row is baked into the image and takes no zip: it
          renders no lifecycle control anywhere, this one included. */}
      {pkg === null && canWrite && !site.systemOwned ? (
        <ZipPicker site={site} open={zipOpen} onOpenChange={onZipOpenChange} />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// A package-produced deployable
// ---------------------------------------------------------------------------

function PackageSource({
  pkg,
  site,
  credentials,
  flipped,
}: {
  pkg: PackageRow;
  site: SiteRow;
  credentials: readonly CredentialRow[];
  flipped: boolean;
}) {
  return (
    <>
      <Chips label="Source facts">
        <CredentialChip pkg={pkg} credentials={credentials} />
        {pkg.updateAvailable ? (
          /* A STANDING MARK, not the arrival cue: "there is a newer version
             than the one you are running" is a state, true until somebody
             deploys, and the Head's Deploy the update is what ends it. */
          <Chip tone="accent" title={`Newer upstream: ${pkg.latestKnownVersion}`}>
            <ArrowUpCircle size={11} aria-hidden /> update
          </Chip>
        ) : null}
      </Chips>
      {pkg.updateAvailable ? (
        <Caption>
          There is a newer version upstream: {shortVersion(pkg.latestKnownVersion)}. Deploy the update, above, starts
          a fresh run and shows you what it would do first.
        </Caption>
      ) : null}
      <Facts>
        <Fact
          label="Tracking"
          value={pkg.repoRef === "" ? (pkg.sourceKind === "repo" ? "default branch" : "") : pkg.repoRef}
        />
        {pkg.sourceKind === "artifact" ? <Fact label="Zip" value={pkg.artifactId} mono /> : null}
        <Fact label="Deployed" value={pkg.deployedVersion === "" ? "" : shortVersion(pkg.deployedVersion)} mono />
        <Fact
          label="Latest upstream"
          value={pkg.latestKnownVersion === "" ? "" : shortVersion(pkg.latestKnownVersion)}
          mono
        />
        <BundleFact site={site} flipped={flipped} />
        <Fact label="Added" value={formatMoment(pkg.createdAt)} />
      </Facts>
    </>
  );
}

/**
 * The auto-deploy switch (epic memql#4900, task memql#4903).
 *
 * ON THE SOURCE STOP because it is a property of the SOURCE rather than of
 * any one run: it answers "when this source moves, then what", which is the
 * same question the version facts above it answer for the past.
 *
 * A CHECKBOX IS CORRECT HERE, and it is the case DESIGN.md rule 10 leaves
 * open: it states a CHOICE in a form, which is what a checkbox is for,
 * rather than filtering content in front of a list, which is what the rule
 * forbids.
 *
 * The caption carries the whole promise, because the switch is worthless
 * without it -- somebody arming this needs to know the confirm gate is still
 * there for anything that changed, or they will either not use it or use it
 * believing something untrue. It writes immediately rather than behind a
 * Save: there is one bit to change and no second field to keep it company.
 */
export function AutoDeploySwitch({ pkg }: { pkg: PackageRow }) {
  const actions = usePackageActions();
  return (
    <section className="os-report-part">
      <h4 className="os-report-heading">
        <Zap size={12} aria-hidden /> When this source moves
      </h4>
      <Check
        checked={pkg.autoDeploy}
        disabled={actions.busy}
        onChange={(on) => void actions.setAutoDeploy(pkg.id, on)}
      >
        Deploy the update by itself when the plan is unchanged
      </Check>
      <Caption>
        {pkg.autoDeploy
          ? "A push that plans exactly what the last deploy planned goes live without a click. Anything new -- an app, some MemQL, a changed build command, a problem -- still waits for you here."
          : "A push lights the update chip and waits for you. Turn this on and one that changes nothing about the plan deploys itself."}
      </Caption>
      {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
    </section>
  );
}

/**
 * Switch which of the caller's credentials this source fetches under.
 *
 * ROTATION IS THIS CONTROL (design section D). There is no "rotate" write:
 * a credential's value is sealed once and never replaced, so rotating means
 * adding a new one and pointing the source at it -- which the picker does in
 * one place, add included. It lives on the SOURCE stop because a credential
 * is a fact about the source and not about any one app it produced, and the
 * chip above already names the one in force.
 *
 * Its own write hook, so the refusal renders beside the control that produced
 * it: `updatePackageSource` is owner-tier, and a cluster owner reading
 * somebody else's source gets the guard's own sentence here rather than a
 * silent no-op.
 */
export function SwitchCredential({ pkg, credentials }: { pkg: PackageRow; credentials: readonly CredentialRow[] }) {
  const actions = usePackageActions();
  const [chosen, setChosen] = useState(pkg.credentialId);
  const changed = chosen.trim() !== pkg.credentialId.trim();

  return (
    <section className="os-report-part">
      <h4 className="os-report-heading">
        <KeyRound size={12} aria-hidden /> Fetches under
      </h4>
      <CredentialField
        id={`os-source-credential-${pkg.id}`}
        credentials={credentials}
        value={chosen}
        onChange={setChosen}
      />
      <FormRow>
        <Button tone="primary" disabled={!changed} busy={actions.busy} onClick={() => void actions.setCredential(pkg.id, chosen)}>
          Save
        </Button>
        <Caption>The next fetch uses it. Nothing already deployed changes.</Caption>
      </FormRow>
      {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
    </section>
  );
}

/**
 * The credential a repository is fetched under, as its CARD: never a value --
 * there is no field on the projection that could hold one. An id that
 * resolves to no card belongs to somebody else's credential, and the chip
 * says exactly that rather than printing the id.
 *
 * A GRANT AND A PASTED TOKEN READ DIFFERENTLY, because they are different
 * things (epic memql#4915). A token's card is a label somebody chose and a
 * digest of the value; a GitHub App grant has neither in any useful sense --
 * what it has is an account, so it names the login. Rendering a grant
 * through the token spelling would print an empty label beside an empty
 * fingerprint and read as a credential that had lost its own facts.
 *
 * MUTED HERE, not accent. The accent chip that names a connected login is
 * the Sources card's, in Settings; this stop's one accent already belongs to
 * `update`, and a second on the same chips row would make accent a status
 * colour rather than the shell's one emphasis.
 *
 * The ended state is said in the warn tone either way, because the next
 * fetch under it will refuse and a person reading this stop is the one who
 * can put it right -- but a grant is `disconnected` rather than `revoked`,
 * which is the word the act that ended it used.
 */
function CredentialChip({ pkg, credentials }: { pkg: PackageRow; credentials: readonly CredentialRow[] }) {
  // A zip has nothing to fetch, so it has no credential to name.
  if (pkg.sourceKind !== "repo") return null;
  const id = pkg.credentialId.trim();
  if (id === "") {
    return (
      <Chip tone="muted" title="A public repository, fetched with no credential.">
        public
      </Chip>
    );
  }
  const card = credentials.find((c) => c.id === id) ?? null;
  if (card === null) {
    return (
      <Chip tone="muted" title="This source fetches under a credential that is not one of yours, so its card is not shown here.">
        a credential you cannot see
      </Chip>
    );
  }
  const grant = isGithubAppGrant(card);
  return (
    <>
      {grant ? (
        <Chip
          tone="muted"
          title="Fetched under your GitHub connection. This cluster renews it for you, and there is no token to manage."
        >
          {card.login === "" ? "your GitHub connection" : `@${card.login}`}
        </Chip>
      ) : (
        <Chip tone="muted" title={`Fetched under your credential for ${card.host}. The value is read only at fetch time and is never shown.`}>
          {card.label} <span>{card.fingerprint}</span>
        </Chip>
      )}
      {credentialIsRevoked(card) ? (
        <span className="os-deploy-status" data-tone="warn">
          {grant ? "disconnected" : "revoked"}
        </span>
      ) : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// A hand-made deployable
// ---------------------------------------------------------------------------

/**
 * A hand-made site IS its bundle, so the source facts are the bundle's:
 * the reference, what its form means for the next deploy, and where the
 * bytes came from -- a Library zip, or a CI push through the bundle route.
 * Only what is known is said: whether a zip had index.html at its root is a
 * fact the publish checked and this row does not carry.
 */
function HandMadeSource({ site, flipped }: { site: SiteRow; flipped: boolean }) {
  const form = bundleForm(site.bundleRef);
  const fromLibrary = site.artifactId !== "";
  const pushed = !fromLibrary && form === "uploaded";
  return (
    <Facts>
      <BundleFact site={site} flipped={flipped} />
      <Fact label="What that means" value={bundleFormNote(form)} />
      {fromLibrary ? (
        <>
          <Fact
            label="Published from"
            value={<code className="os-mono">{site.artifactId}</code>}
            title="Provenance only. The edge reads bundleRef and never this field."
          />
          <Fact label="Provenance" value="Published from the Library." />
        </>
      ) : null}
      {pushed ? (
        <Fact
          label="Provenance"
          value={
            isPlaceholderBundle(site.bundleRef) ? (
              <>
                Waiting for the first push from your CI, through{" "}
                <code className="os-mono">POST /sites/{site.id}/bundles</code>.
              </>
            ) : (
              <>
                Pushed by your CI through <code className="os-mono">POST /sites/{site.id}/bundles</code>.
              </>
            )
          }
        />
      ) : null}
    </Facts>
  );
}

/**
 * The bundle reference the site holds now, marked when it just changed.
 *
 * THE FLIP, MARKED. A CI publish through POST /sites/{id}/bundles happens on
 * a node nobody in this browser is talking to; the `updated` broadcast
 * brings it here, and without a marker the value would simply be different
 * from the one somebody read a moment ago with nothing to say it had moved.
 */
function BundleFact({ site, flipped }: { site: SiteRow; flipped: boolean }) {
  return (
    <Fact
      label="Bundle"
      value={
        <span className="os-deploy-bundle">
          <code className="os-mono">{site.bundleRef || "--"}</code>
          {flipped ? (
            <span className="os-livelist-tick" role="status">
              changed just now
            </span>
          ) : null}
        </span>
      }
    />
  );
}

// ---------------------------------------------------------------------------
// The source's own lifecycle: archive with every app, restore
// ---------------------------------------------------------------------------

/**
 * Package-level acts, on the Source stop because they belong to the SOURCE
 * and not to any one app it produced. Its own write hook, so a refusal
 * renders beside the control that produced it -- the server's
 * `package_has_active_deployables` names the apps still serving, and that
 * sentence belongs next to the button somebody just pressed.
 */
// `apps` is every deployable this source produced, as ONE list. It used to
// arrive split into a "site" and its "siblings" -- a shape that only made
// sense while this rendered inside one app's page, and which is exactly what
// let a control that archives every one of them sit on a single app's page.
export function PackageLifecycle({ pkg, apps }: { pkg: PackageRow; apps: readonly SiteRow[] }) {
  const actions = usePackageActions();
  const [archiving, setArchiving] = useState(false);
  const [confirmName, setConfirmName] = useState("");
  const produced = apps.length === 0 ? "nothing yet" : apps.map(siteName).join(", ");

  if (pkg.status === "archived") {
    return (
      <section className="os-report-part">
        <h4 className="os-report-heading">
          <RotateCcw size={12} aria-hidden /> Restore
        </h4>
        <Caption>Puts this source back on the active list. The apps it produced keep whatever state they have.</Caption>
        <Button onClick={() => void actions.restore(pkg.id)} busy={actions.busy}>
          <RotateCcw size={12} aria-hidden /> Restore this source
        </Button>
        {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
      </section>
    );
  }

  return (
    <section className="os-report-part os-danger-part">
      <h4 className="os-report-heading">
        <Archive size={12} aria-hidden /> Archive
      </h4>
      {archiving ? (
        <>
          <Caption>
            Archiving keeps the source and everything it recorded, and files the apps it produced -- {produced} -- with
            it. An app still serving is refused by name. Type <strong>{pkg.name}</strong> to confirm.
          </Caption>
          <div className="os-confirm-row">
            <Input
              id={`os-archive-source-${pkg.id}`}
              label={`Type ${pkg.name} to confirm`}
              value={confirmName}
              onChange={setConfirmName}
              placeholder={pkg.name}
            />
            <Button tone="quiet" onClick={() => setArchiving(false)}>
              Cancel
            </Button>
            <Button
              tone="danger"
              disabled={confirmName !== pkg.name}
              busy={actions.busy}
              onClick={() => void actions.archive(pkg.id, confirmName)}
            >
              Archive
            </Button>
          </div>
        </>
      ) : (
        <>
          <Caption>An archived source stays listed and can be restored. Nothing is deleted.</Caption>
          <Button onClick={() => setArchiving(true)}>
            <Archive size={12} aria-hidden /> Archive this source and every app it produced
          </Button>
        </>
      )}
      {actions.refusal ? <ProblemNotice problem={{ ...actions.refusal, fatal: true }} tone="error" /> : null}
    </section>
  );
}
