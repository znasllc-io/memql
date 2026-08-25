import { useState, type ReactNode } from "react";

import {
  Button,
  Checkbox,
  ConfirmDialog,
  DataText,
  Field,
  FormRow,
  RadioGroup,
  Textarea,
  TextInput,
} from "../../ui";
import { ErrorMessage } from "../../components/StatusMessage";
import type { CheckResult, ReleaseRow, ReleasesState } from "./useReleases";

// The Releases card: cutting a version of MemQL itself, and the history of
// having done so.
//
// # Why this is a component and not a band on the view
//
// Same reason DeploymentOps is: portal_view_composition_test.go forbids
// iteration inside src/views/, and this renders a list. The view supplies the
// slot, the card lives out here.
//
// # It is ABSENT for a non-owner, never disabled
//
// instanceActions' doctrine, and it is the whole authorization story on this
// side: never offer a button whose only outcome is a refusal. A non-owner does
// not see a greyed-out "Cut a release" -- they see no Releases band at all, and
// the view decides that before this component is reached. The engine enforces
// it independently (integrations/release's Go owner wall, before any network
// call), so this is a courtesy rather than a control, exactly like the deploy
// console's role matrix.
//
// # Setup states render INSTEAD of the button
//
// release_repo_unconfigured and credential_unavailable are not failures -- they
// are true statements about an installation that has not been told what to cut
// or with what. Rendering a button that can only refuse would be the thing the
// doctrine above forbids, so the card renders the step to take.
//
// # The headline is a TAG and the list is ROWS
//
// A release cut by hand -- release.sh's break-glass path, which still exists --
// creates a tag this cluster never hears about. So "newest" comes from GitHub's
// tags (via the dry run) and the list is only this cluster's own history, and
// the card SAYS so: an operator who cut by hand needs to understand why their
// release is missing from the list rather than concluding the list is broken.

// The phrase an owner types to arm a cut. The capability-script convention for
// a consequential action (docs/internal/design/capability-script-contract.md),
// carried into the UI: a deliberate keystroke, not a password.
//
// Hyphenated and spelled out rather than the bare verb DeploymentOps uses for
// repair, because this action is not undoable in the same way. A repair
// re-converges a cluster; a release publishes a version to everyone who
// installs, and the tag cannot be un-cut without deleting it by hand.
export const CUT_CONFIRM_PHRASE = "cut-a-release";

export function ReleasesCard({ state }: { state: ReleasesState }): ReactNode {
  const [bump, setBump] = useState<"major" | "minor" | "patch">("patch");
  const [notes, setNotes] = useState("");
  const [bumpPin, setBumpPin] = useState(false);
  const [phrase, setPhrase] = useState("");
  const [confirming, setConfirming] = useState(false);

  const close = (): void => {
    setConfirming(false);
    setPhrase("");
  };

  if (state.setup !== "") {
    return <SetupState setup={state.setup} />;
  }

  return (
    <>
      {state.error ? (
        <div className="mb-3">
          <ErrorMessage>Could not read the release state: {state.error}</ErrorMessage>
        </div>
      ) : null}
      {state.actionError ? (
        <div className="mb-3">
          <ErrorMessage>{state.actionError}</ErrorMessage>
        </div>
      ) : null}
      {state.lastCut ? (
        <p role="status" className="mb-3 rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg">
          Cut <DataText kind="id">{state.lastCut}</DataText>. The image build starts from the
          published Release; use Check images below to see when it has finished.
        </p>
      ) : null}

      <Headline state={state} />

      <div className="mt-3">
        <FormRow>
          <BumpChoice value={bump} onChange={setBump} />
          <Field label="Notes (optional)" grow>
            <Textarea
              value={notes}
              onChange={setNotes}
              rows={2}
              placeholder="Prepended to GitHub's generated release notes."
            />
          </Field>
        </FormRow>
      </div>

      <div className="mt-2">
        <Checkbox
          checked={bumpPin}
          onChange={setBumpPin}
          label="Also open a pull request bumping the VS Code extension’s pinned release"
        />
      </div>

      <div className="mt-3">
        <Button tone="danger" onClick={() => setConfirming(true)} disabled={state.busy}>
          Cut a release
        </Button>
      </div>

      <ConfirmDialog
        open={confirming}
        title="Cut a release?"
        confirmLabel="Cut the release"
        tone="danger"
        busy={state.busy}
        confirmDisabled={phrase.trim() !== CUT_CONFIRM_PHRASE}
        onConfirm={() => {
          void state.cut({ bump, notes, bumpExtensionPin: bumpPin });
          close();
        }}
        onCancel={close}
      >
        <p>
          Owner-only, enforced on the engine before anything is created. Creates the tag{" "}
          <DataText kind="id">{state.plan?.version ?? ""}</DataText> at main&rsquo;s head (
          <DataText kind="id">{shortSha(state.plan?.baseSha ?? "")}</DataText>) and publishes a
          GitHub Release, which is what starts the image build for every node type.
        </p>
        <p className="mt-2">
          This is not undoable from here: the tag and the Release would have to be deleted on
          GitHub by hand.
        </p>
        <div className="mt-3">
          <Field label={`Type "${CUT_CONFIRM_PHRASE}" to confirm`}>
            <TextInput value={phrase} onChange={setPhrase} placeholder={CUT_CONFIRM_PHRASE} />
          </Field>
        </div>
      </ConfirmDialog>

      <History state={state} />
    </>
  );
}

// BumpChoice is a RADIO GROUP and not a dropdown, which is a deliberate choice
// rather than an arbitrary one.
//
// All three options are on screen at once. A dropdown hides `major` behind a
// click, and the whole point of this control is that the operator sees the
// range of what they are choosing between before choosing -- these three are
// not equivalent, and `major` is the one somebody should never reach by
// accident. The same reasoning makes the consequence text sit beside each
// option rather than in a hint below the field.
//
// Hand-rolled inputs rather than a ui-kit control because the kit has no radio
// group; this is the portal's first. The markup is a <fieldset> with a
// <legend>, which is what makes it announce as one group with three choices to
// a screen reader -- a set of loose labelled inputs does not.
function BumpChoice({
  value,
  onChange,
}: {
  value: "major" | "minor" | "patch";
  onChange: (next: "major" | "minor" | "patch") => void;
}): ReactNode {
  const options: { key: "patch" | "minor" | "major"; says: string }[] = [
    { key: "patch", says: "a fix" },
    { key: "minor", says: "new behaviour" },
    { key: "major", says: "a break" },
  ];
  return (
    <RadioGroup
      name="release-bump"
      legend="Bump"
      value={value}
      onChange={(next) => onChange(next as "patch" | "minor" | "major")}
      options={options.map((option) => ({
        value: option.key,
        label: option.key,
        hint: option.says,
      }))}
    />
  );
}

// Headline: what exists now, and what a cut would produce.
function Headline({ state }: { state: ReleasesState }): ReactNode {
  if (state.loading && state.plan === null) {
    return <p className="text-sm text-muted">Reading the release state&hellip;</p>;
  }
  if (state.plan === null) return null;
  return (
    <p className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
      <span className="text-lg font-semibold tracking-tight">
        <DataText kind="id">{state.plan.previousTag}</DataText>
      </span>
      <span className="text-muted">
        newest tag on <DataText kind="id">{state.plan.repository}</DataText>
      </span>
      <span className="text-muted">
        a patch cut would be <DataText kind="id">{state.plan.version}</DataText>
      </span>
    </p>
  );
}

// SetupState renders the step to take, instead of a button that can only
// refuse.
function SetupState({ setup }: { setup: string }): ReactNode {
  const [what, how] =
    setup === "release_repo_unconfigured"
      ? [
          "This installation has not been told which repository to cut releases of.",
          "Seed the global variable MEMQL_RELEASE_REPO with the repository in owner/name form.",
        ]
      : [
          "This installation has no GitHub credential for cutting releases.",
          "Seed the global secret MEMQL_GITHUB_RELEASE_TOKEN with a fine-grained token holding Contents: read/write on that repository.",
        ];
  return (
    <div className="rounded border border-line bg-raised px-3 py-2">
      <p className="text-sm text-fg">{what}</p>
      <p className="mt-1 text-sm text-muted">{how}</p>
      <p className="mt-1 text-xs text-subtle">
        The release-cutting runbook covers minting the token and the first dry run.
      </p>
    </div>
  );
}

// History: this cluster's own cuts, newest first.
function History({ state }: { state: ReleasesState }): ReactNode {
  if (state.rows.length === 0) {
    return (
      <p className="mt-4 text-sm text-subtle">
        {state.loading
          ? "Reading the release history…"
          : "This cluster has cut no releases. A release cut by hand appears as the newest tag above and not here — these rows are what this installation did."}
      </p>
    );
  }
  return (
    <div className="mt-4 overflow-x-auto">
      <p className="mb-2 text-xs text-subtle">
        What this installation cut. A release cut by hand is the newest tag above and has no row
        here.
      </p>
      <table className="w-full min-w-[40rem] text-sm">
        <thead>
          <tr className="border-b border-line text-left text-xs text-muted">
            <th className="py-1 pr-3 font-medium">Version</th>
            <th className="py-1 pr-3 font-medium">Status</th>
            <th className="py-1 pr-3 font-medium">From</th>
            <th className="py-1 pr-3 font-medium">Cut by</th>
            <th className="py-1 font-medium">Images</th>
          </tr>
        </thead>
        <tbody>
          {state.rows.map((row) => (
            <ReleaseRowView
              key={row.id === "" ? row.version : row.id}
              row={row}
              check={state.checks[row.version]}
              checking={state.checking === row.version}
              onCheck={() => void state.check(row.version)}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ReleaseRowView({
  row,
  check,
  checking,
  onCheck,
}: {
  row: ReleaseRow;
  check: CheckResult | undefined;
  checking: boolean;
  onCheck: () => void;
}): ReactNode {
  return (
    <tr className="border-b border-line align-top">
      <td className="py-2 pr-3">
        {row.releaseUrl === "" ? (
          <DataText kind="id">{row.version}</DataText>
        ) : (
          <a href={row.releaseUrl} target="_blank" rel="noreferrer" className="underline hover:text-fg">
            <DataText kind="id">{row.version}</DataText>
          </a>
        )}
        <div className="text-xs text-subtle">{row.bump}</div>
      </td>
      <td className="py-2 pr-3">
        <StatusText row={row} />
      </td>
      <td className="py-2 pr-3">
        <DataText kind="id">{shortSha(row.baseSha)}</DataText>
      </td>
      <td className="py-2 pr-3 text-muted">{row.requestedByEmail}</td>
      <td className="py-2">
        <Button size="xs" onClick={onCheck} disabled={checking}>
          {checking ? "Checking…" : "Check images"}
        </Button>
        {check === undefined ? null : <CheckReading check={check} />}
      </td>
    </tr>
  );
}

// StatusText spells out the half-done state, which is the one an operator has
// to ACT on rather than merely read.
function StatusText({ row }: { row: ReleaseRow }): ReactNode {
  if (row.status === "tag_created_release_failed") {
    return (
      <>
        <span className="font-medium text-fg">tag created, release failed</span>
        <div className="text-xs text-muted">
          The tag <DataText kind="id">{row.tagName}</DataText> exists and no Release does, so
          nothing is building. Publish a Release for that tag on GitHub to start the build, or
          delete the tag to undo the cut.
        </div>
        {row.error === "" ? null : <div className="mt-1 text-xs text-subtle">{row.error}</div>}
      </>
    );
  }
  return (
    <>
      <span className={row.status === "images_available" ? "text-muted" : "font-medium text-fg"}>
        {row.status === "images_available" ? "images available" : row.status}
      </span>
      {row.pinBumpPrUrl === "" ? null : (
        <div className="text-xs">
          <a href={row.pinBumpPrUrl} target="_blank" rel="noreferrer" className="underline hover:text-fg">
            pin-bump pull request
          </a>
        </div>
      )}
      {row.pinBumpNote === "" ? null : <div className="text-xs text-subtle">{row.pinBumpNote}</div>}
    </>
  );
}

// CheckReading renders the three-valued answer as three different sentences.
// The errored case is never rendered as an absence: it says the check could
// not tell, which is the whole point of the server refusing to guess.
function CheckReading({ check }: { check: CheckResult }): ReactNode {
  if (check.error !== "") {
    return (
      <div className="mt-1 text-xs text-fg">
        The check could not tell: {check.error}. The status above is unchanged.
      </div>
    );
  }
  if (check.status === "images_available") {
    return <div className="mt-1 text-xs text-muted">Every image is published.</div>;
  }
  const missing = check.images.filter((img) => !img.present);
  return (
    <div className="mt-1 text-xs text-muted">
      Still building{check.age === "" ? "" : ` — cut ${check.age}`}.
      {missing.length === 0 ? null : (
        <span className="block text-subtle">
          Not published yet: {missing.map((img) => img.repository).join(", ")}
        </span>
      )}
    </div>
  );
}

function shortSha(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha;
}
