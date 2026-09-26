import { useEffect, useMemo, useRef, useState } from "react";
import { AddLink } from "../../../kit/AddButton";

import { Button, Caption, Chips, Input, RecordList, RecordListSkeleton, RecordRow, RefreshButton, Subhead, useNow } from "../../../kit";
import { formatFreshness } from "../../../kit/format";
import type { Refusal } from "../packages/actions";
import { toneFor } from "../packages/refusals";
import { ProblemNotice } from "../packages/ReportView";
import { groupRepositories, repositoryCount, type RepositoryPage, type RepositoryRow } from "./repositories";

// The repository picker: choose a repository instead of typing a URL (epic
// memql#4915, design section A step 3).
//
// ===========================================================================
// A LIST, NOT A SELECT, AND NOT A MODAL
// ===========================================================================
// `Select` deliberately draws no `<optgroup>` header (kit/controls.tsx), and
// a picker over two hundred repositories needs group headers, a search and
// three facts per row -- which is a list, and lists are what this shell
// already knows how to draw. It renders in the surface's own body at its
// full width (DESIGN.md rule 9) rather than in a modal, because the picker
// IS the content of the step it belongs to and not an interruption of it.
//
// A ROW IS THE AFFORDANCE. A repository is a noun, so the whole row is the
// button (rule 6) -- never a noun-shaped card with a "Select" verb parked
// inside it.
//
// SAY IT ONCE (rule 7). Under a group header the row carries the SHORT name
// alone; the full `owner/name` lives on the row's `title`, which is where a
// person goes when they want to be certain and nowhere else.
//
// IT IS NOT A LIVE LIST. Nothing broadcasts a repository -- these rows are
// not in this graph -- so the footer says when the list was read and offers
// to refresh. A LiveList here would caption liveness that is not there.

export function RepositoryPicker({
  page,
  readAt,
  busy,
  refusal,
  installUrl = "",
  chosen = "",
  idPrefix = "os-repo-picker",
  onChoose,
  onLookAgain,
  onReadMore,
  showRefresh = true,
  showChosenLabel = true,
  showGroupHeading = true,
}: {
  page: RepositoryPage;
  /** When the list was read, as an ISO instant. Empty = not read yet. */
  readAt: string;
  busy: boolean;
  refusal: Refusal | null;
  /** The app's installation page, when this cluster has one. */
  installUrl?: string;
  /** The chosen repository's full name, or "". */
  chosen?: string;
  /** Namespaces the search field's id, so two pickers can coexist. */
  idPrefix?: string;
  onChoose: (repo: RepositoryRow) => void;
  onLookAgain: () => void;
  onReadMore: () => void;
  showRefresh?: boolean;
  showChosenLabel?: boolean;
  showGroupHeading?: boolean;
}) {
  const [search, setSearch] = useState("");
  const now = useNow();

  const groups = useMemo(() => groupRepositories(page, search), [page, search]);
  const shown = repositoryCount(groups);
  const total = page.repositories.length;

  // COMING BACK FROM GITHUB IS THE ASK. Installing the app on another
  // organization happens in another tab, on github.com, and nothing tells
  // this list it happened -- so the person returned to the same list they
  // left and had to know that "Look again" was the next thing to press. Once
  // the link has been followed, the next time this tab is looked at the list
  // is read again, once. Not on every focus: a read is a call to GitHub under
  // this person's token, and only following the link is a reason to expect a
  // different answer.
  const awaitingInstall = useRef(false);
  const lookAgain = useRef(onLookAgain);
  lookAgain.current = onLookAgain;
  useEffect(() => {
    const onReturn = () => {
      if (!awaitingInstall.current || document.visibilityState !== "visible") return;
      awaitingInstall.current = false;
      lookAgain.current();
    };
    document.addEventListener("visibilitychange", onReturn);
    window.addEventListener("focus", onReturn);
    return () => {
      document.removeEventListener("visibilitychange", onReturn);
      window.removeEventListener("focus", onReturn);
    };
  }, []);

  return (
    <div className="os-stop-body">
      <div className="os-refresh-row">
        {showRefresh ? <RefreshButton label="Refresh repositories" onClick={onLookAgain} busy={busy} /> : null}
        <span className="os-caption">
          {total === 0
            ? readAt === ""
              ? "Not read yet."
              : `Nothing to show, read ${formatFreshness(readAt, now)}.`
            : `Showing ${shown} of ${total}, read ${formatFreshness(readAt, now)}.`}
        </span>
        {/* THE WALK, on demand and named for what it does. There is no
            infinite scroll here: a person picking one repository out of many
            should not have to make the browser fetch by accident. */}
        {page.nextPage > 0 ? (
          <Button onClick={onReadMore} busy={busy}>
            Read more
          </Button>
        ) : null}
        {/* ANOTHER ORGANIZATION IS ANOTHER GROUP IN THIS LIST, so the way to
            add one is here, under the groups, and not only on a settings page.
            It used to be offered when the list was EMPTY and nowhere else in
            the wizard: somebody with one organization connected, looking for a
            repository in a second, was shown a complete-looking list and no
            way to make it longer. TEXT, not a third button on this row: it
            leaves the product, and the row's one act is reading again. An
            empty list keeps it as its own control (`EmptyPicker`), where it is
            the only thing to do. */}
        {total > 0 ? <InstallLink installUrl={installUrl} text onFollow={() => { awaitingInstall.current = true; }} /> : null}
      </div>
      {/* THE REFUSAL FIRST, and above the list rather than instead of it: a
          refusal is not a zero (clients/os/README.md), so a read that failed
          leaves whatever was already read on screen and says what happened
          over it. */}
      {refusal ? <ProblemNotice problem={refusal} tone={toneFor(refusal.code)} /> : null}

      {/* SEARCH IS THE CONTROL THIS PICKER EXISTS TO OFFER, not a section
          filter, so it is a plain Input above the list rather than behind
          Refine -- rule 2 governs a SECTION's filters, and with two hundred
          repositories finding one IS the interaction. It is not drawn when
          there is nothing to search: filter chrome over no content is the
          thing that rule forbids. */}
      {total > 0 ? (
        <div className="os-form-row">
          <Input
            id={`${idPrefix}-search`}
            label="Search repositories"
            value={search}
            onChange={setSearch}
            placeholder="Search"
          />
        </div>
      ) : null}

      {groups.length === 0 && refusal ? null : groups.length === 0 && (busy || readAt === "") ? (
        <RecordListSkeleton label="Loading repositories" />
      ) : groups.length === 0 ? (
        <EmptyPicker total={total} searching={search.trim() !== ""} installUrl={installUrl} onFollow={() => { awaitingInstall.current = true; }} />
      ) : (
        groups.map((group) => (
          <div className="os-files-group" key={group.owner} role="group" aria-label={group.owner}>
            {showGroupHeading ? <Subhead meta={readAt !== "" && !busy && !refusal && !group.pending ? group.repositories.length : undefined}>{group.owner}</Subhead> : null}
            {group.pending ? (
              /* A PENDING INSTALLATION IS A GROUP WITH A SENTENCE INSTEAD OF
                 ROWS -- never hidden and never an error. The repair belongs
                 to somebody else, so `--os-warn` and not `--os-error`: this
                 is somebody's next step rather than a fault, and the one
                 useful thing this surface can do is name whom to ask. */
              <>
                {/* THE MARKER WEARS THE CHIP BOX, exactly as the connected-
                    account card's does. Bare, `.os-deploy-status` is a
                    boxless word that a flex column also stretches to the full
                    width of the picker -- so one list carried two grammars
                    for a marker, with `private` a pill on the rows above and
                    this a 698px slab. Standing it in `.os-chips` is what
                    gives it the box (styles/index.css), and the tone stays
                    `--os-warn`: an owner has not clicked yet, which is
                    somebody's next step and not a fault. */}
                <Chips label={`${group.owner} installation`}>
                  <span className="os-deploy-status" data-tone="warn">
                    pending
                  </span>
                </Chips>
                <Caption>Waiting for an owner of {group.owner} to approve the app.</Caption>
              </>
            ) : (
              <RecordList as="ul" label={`${group.owner} repositories`}>
                {group.repositories.map((repo) => (
                  <RepositoryChoice
                    key={repo.fullName}
                    repo={repo}
                    chosen={repo.fullName === chosen}
                    showChosenLabel={showChosenLabel}
                    now={now}
                    onChoose={onChoose}
                  />
                ))}
              </RecordList>
            )}
          </div>
        ))
      )}


    </div>
  );
}

/** Repository choices use the same flat row anatomy as every record list.
 * The default branch is the secondary identity line; privacy and selection
 * stay in the state column, and the last push remains a quiet summary fact. */
function RepositoryChoice({
  repo,
  chosen,
  showChosenLabel,
  now,
  onChoose,
}: {
  repo: RepositoryRow;
  chosen: boolean;
  showChosenLabel: boolean;
  now: Date;
  onChoose: (repo: RepositoryRow) => void;
}) {
  const unusual = repo.visibility !== "" && repo.visibility !== "public" && repo.visibility !== "private";
  return (
    <RecordRow
      name={<span title={repo.fullName}>{repo.name || repo.fullName}</span>}
      secondary={repo.defaultBranch ? <span title={repo.defaultBranch}>{repo.defaultBranch}</span> : undefined}
      current={chosen}
      open={showChosenLabel ? chosen : undefined}
      selected={showChosenLabel ? undefined : chosen}
      onOpen={() => onChoose(repo)}
      state={unusual ? repo.visibility : repo.private ? "private" : undefined}
      stateExtra={chosen && showChosenLabel ? <span className="os-livelist-tick">chosen</span> : null}
    >
      {repo.pushedAt ? <span>pushed {formatFreshness(repo.pushedAt, now)}</span> : null}
    </RecordRow>
  );
}

/**
 * Nothing to pick.
 *
 * EMPTY IS AN INVITATION, NOT A VOID. A grant that reaches no repositories
 * is one installation away from reaching some, and the link is the whole
 * answer; a search that matched nothing is a different sentence, because the
 * repair there is in the field the person is already typing in.
 */
function EmptyPicker({
  total,
  searching,
  installUrl,
  onFollow,
}: {
  total: number;
  searching: boolean;
  installUrl: string;
  onFollow: () => void;
}) {
  if (searching) {
    return <Caption>No repository here matches that. {total} were read.</Caption>;
  }
  return (
    <>
      <Caption>This connection reaches no repositories yet.</Caption>
      {/* ON THE CONTROL LINE, exactly as the connected-account card mounts
          the same link. `.os-deploy-publish` is a stretching flex column, so
          a bare anchor there became a 698px full-bleed slab of the same
          control the card renders at 214px -- one link, two shapes,
          depending only on which surface you reached it from. */}
      <div className="os-form-row">
        <InstallLink installUrl={installUrl} onFollow={onFollow} />
      </div>
    </>
  );
}

/**
 * Where one installs the app on another account or organization.
 *
 * A REAL ANCHOR IN A NEW TAB, and `rel` is not decoration: a new tab handed
 * a live `window.opener` can navigate the shell it came from. Rendered only
 * when a URL is in hand -- a cluster with no GitHub App has none, and a link
 * to nowhere is worse than no link.
 */
export function InstallLink({ installUrl, text = false, onFollow, compact = false }: {
  installUrl: string;
  /** Draw it as a text link, for a row that already has its one button. */
  text?: boolean;
  /** Told when the link is followed, so a list can read again on the way back. */
  onFollow?: () => void;
  compact?: boolean;
}) {
  if (installUrl === "") return null;
  if (compact) return <AddLink label="Add organization access" href={installUrl} target="_blank" rel="noreferrer noopener" onClick={onFollow} />;
  return (
    <a
      className={text ? "os-link" : "os-button"}
      data-tone={text ? undefined : "quiet"}
      href={installUrl}
      target="_blank"
      rel="noreferrer noopener"
      onClick={onFollow}
    >
      Install on another organization
    </a>
  );
}
