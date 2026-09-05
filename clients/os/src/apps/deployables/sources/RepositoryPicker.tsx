import { useMemo, useState } from "react";

import { Button, Caption, Chip, Chips, Input, Row as ListRow, Subhead, useNow } from "../../../kit";
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
// to look again. A LiveList here would caption liveness that is not there.

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
}) {
  const [search, setSearch] = useState("");
  const now = useNow();

  const groups = useMemo(() => groupRepositories(page, search), [page, search]);
  const shown = repositoryCount(groups);
  const total = page.repositories.length;

  return (
    <div className="os-deploy-publish">
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

      {groups.length === 0 ? (
        <EmptyPicker total={total} searching={search.trim() !== ""} installUrl={installUrl} />
      ) : (
        groups.map((group) => (
          <div className="os-files-group" key={group.owner} role="group" aria-label={group.owner}>
            <Subhead>{group.owner}</Subhead>
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
              <div className="os-livelist-rows os-repo-rows">
                {group.repositories.map((repo) => (
                  <RepositoryChoice
                    key={repo.fullName}
                    repo={repo}
                    chosen={repo.fullName === chosen}
                    now={now}
                    onChoose={onChoose}
                  />
                ))}
              </div>
            )}
          </div>
        ))
      )}

      {/* WHEN IT WAS READ, BESIDE THE CONTROL THAT READS IT AGAIN. The
          control and the answer to "how old is this" are one thought
          (.os-refresh-row), and separating them is how a stale reading gets
          read as a live one. */}
      <div className="os-refresh-row">
        <span className="os-caption">
          {total === 0
            ? readAt === ""
              ? "Not read yet."
              : `Nothing to show, read ${formatFreshness(readAt, now)}.`
            : `Showing ${shown} of ${total}, read ${formatFreshness(readAt, now)}.`}
        </span>
        <Button onClick={onLookAgain} busy={busy} busyLabel="Reading...">
          Look again
        </Button>
        {/* THE WALK, on demand and named for what it does. There is no
            infinite scroll here: a person picking one repository out of many
            should not have to make the browser fetch by accident. */}
        {page.nextPage > 0 ? (
          <Button onClick={onReadMore} busy={busy} busyLabel="Reading...">
            Read more
          </Button>
        ) : null}
      </div>
    </div>
  );
}

/**
 * One repository.
 *
 * `private` is a chip and `public` says NOTHING: most repositories are
 * public, and silence is the default state -- a lock icon AND the word would
 * be the same fact twice. `visibility` is printed only when GitHub says
 * something other than the two it already told us through `private`, which
 * is how `internal` reaches the eye without every other row carrying a word
 * it did not need -- and it REPLACES the private chip when it is present,
 * because "private internal" is that same fact twice with an extra word.
 *
 * ===========================================================================
 * THE FACTS ARE COLUMNS, AND EVERY ROW RENDERS ALL THREE
 * ===========================================================================
 * `.os-row` is a plain flex line, so a fact placed after the name starts
 * wherever that name happens to end: measured over seven rows, the branch
 * began at seven different offsets (101.75 / 142.94 / 164.19 / 188.05 /
 * 192.2 / 228.09 / 283.08) and so did "pushed", which is a list somebody has
 * to re-find the same column in on every line.
 *
 * So each fact sits in a cell of its own with a DEFINITE width, and the cell
 * is rendered EVEN WHEN IT IS EMPTY -- an absent cell is exactly what let
 * the next one move. The names are the visible content and the cells are
 * geometry, so the empty ones say nothing.
 *
 * A DEFINITE WIDTH MAKES THE OVERFLOW SOMEBODY ELSE'S PROBLEM, and the two
 * text cells answer it in the browser (styles/index.css): they are BLOCKS, so
 * `text-overflow` actually applies, and the branch carries its whole value on
 * a `title`. Measured before that, `release-candidate` rendered `release-c`
 * with no ellipsis -- a clipped branch name is indistinguishable from a real
 * one, which is the same objection the pushed cell's width was chosen to
 * avoid.
 */
function RepositoryChoice({
  repo,
  chosen,
  now,
  onChoose,
}: {
  repo: RepositoryRow;
  chosen: boolean;
  now: Date;
  onChoose: (repo: RepositoryRow) => void;
}) {
  const unusual = repo.visibility !== "" && repo.visibility !== "public" && repo.visibility !== "private";
  return (
    <ListRow
      name={<span title={repo.fullName}>{repo.name || repo.fullName}</span>}
      current={chosen}
      open={chosen}
      onOpen={() => onChoose(repo)}
      state={chosen ? <span className="os-livelist-tick">chosen</span> : null}
    >
      <span className="os-repo-cell" data-cell="visibility">
        {unusual ? (
          <Chip tone="muted">{repo.visibility}</Chip>
        ) : repo.private ? (
          <Chip tone="muted">private</Chip>
        ) : null}
      </span>
      {/* THE WHOLE BRANCH NAME IS ON THE `title`, for the reason the row's own
          name carries one: the cell has a definite width, a branch name has
          no bound, and `release-candidate` ellipsises. The mark says a word
          was cut and the title says which. */}
      <span
        className="os-repo-cell os-caption os-mono"
        data-cell="branch"
        {...(repo.defaultBranch === "" ? {} : { title: repo.defaultBranch })}
      >
        {repo.defaultBranch}
      </span>
      <span className="os-repo-cell os-caption" data-cell="pushed">
        {repo.pushedAt === "" ? "" : `pushed ${formatFreshness(repo.pushedAt, now)}`}
      </span>
    </ListRow>
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
}: {
  total: number;
  searching: boolean;
  installUrl: string;
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
        <InstallLink installUrl={installUrl} />
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
export function InstallLink({ installUrl }: { installUrl: string }) {
  if (installUrl === "") return null;
  return (
    <a className="os-button" data-tone="quiet" href={installUrl} target="_blank" rel="noreferrer noopener">
      Install on another organization
    </a>
  );
}
