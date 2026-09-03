import { useMemo, useState } from "react";

import { Button, Caption, Chip, Input, Row as ListRow, Subhead, useNow } from "../../../kit";
import { formatFreshness } from "../../../kit/format";
import type { Refusal } from "../packages/actions";
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
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}

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
                <span className="os-deploy-status" data-tone="warn">
                  pending
                </span>
                <Caption>Waiting for an owner of {group.owner} to approve the app.</Caption>
              </>
            ) : (
              <div className="os-livelist-rows">
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
 * it did not need.
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
      {repo.private ? <Chip tone="muted">private</Chip> : null}
      {unusual ? <Chip tone="muted">{repo.visibility}</Chip> : null}
      {repo.defaultBranch === "" ? null : (
        <span className="os-caption os-mono">{repo.defaultBranch}</span>
      )}
      {repo.pushedAt === "" ? null : (
        <span className="os-caption">pushed {formatFreshness(repo.pushedAt, now)}</span>
      )}
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
      <InstallLink installUrl={installUrl} />
    </>
  );
}

/**
 * Where one installs the app on another account or organisation.
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
      Install on another organisation
    </a>
  );
}
